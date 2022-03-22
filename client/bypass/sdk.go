// Copyright 2020 The ChubaoFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package main

/*
#define _GNU_SOURCE
#define __USE_LARGEFILE64
#ifndef FALLOC_FL_KEEP_SIZE
#define FALLOC_FL_KEEP_SIZE 0x01
#endif
#ifndef FALLOC_FL_PUNCH_HOLE
#define FALLOC_FL_PUNCH_HOLE 0x02
#endif
#include <dirent.h>
#include <fcntl.h>
#include <stdint.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/uio.h>
#include <unistd.h>

typedef struct {
    const char* master_addr;
    const char* vol_name;
    const char* owner;
    const char* follower_read;
    const char* log_dir;
    const char* log_level;
	const char* app;
    const char* prof_port;
	const char* auto_flush;
    const char* master_client;
    const char* tracing_sampler_type;
    const char* tracing_sampler_param;
    const char* tracing_report_addr;
    const char* tracing_flag;
} cfs_config_t;
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	syslog "log"
	"math"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	gopath "path"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/willf/bitset"

	"github.com/chubaofs/chubaofs/client/cache"
	"github.com/chubaofs/chubaofs/proto"
	"github.com/chubaofs/chubaofs/sdk/data"
	"github.com/chubaofs/chubaofs/sdk/master"
	"github.com/chubaofs/chubaofs/sdk/meta"
	"github.com/chubaofs/chubaofs/util/config"
	"github.com/chubaofs/chubaofs/util/exporter"
	"github.com/chubaofs/chubaofs/util/log"
	"github.com/chubaofs/chubaofs/util/tracing"
	"github.com/chubaofs/chubaofs/util/ump"
	"github.com/chubaofs/chubaofs/util/version"

	_ "github.com/chubaofs/chubaofs/util/log/http"     // HTTP APIs for logging control
	_ "github.com/chubaofs/chubaofs/util/tracing/http" // HTTP APIs for tracing
)

const (
	attrMode uint32 = 1 << iota
	attrUid
	attrGid
	attrModifyTime
	attrAccessTime
	attrSize
)

const (
	normalExtentSize      = 32 * 1024 * 1024
	defaultBlkSize        = uint32(1) << 12
	maxFdNum         uint = 1024000
	moduleName            = "kbpclient"
	redologPrefix         = "ib_logfile"
	binlogPrefix          = "mysql-bin"
	appMysql8             = "mysql_8"

	// cache
	maxInodeCache         = 10000
	inodeExpiration       = time.Hour
	inodeEvictionInterval = time.Hour
	dentryValidDuration   = time.Hour
)

var (
	gClientManager *clientManager
	once           sync.Once
	CommitID       string
	BranchName     string
	BuildTime      string
	Debug          string
)

var (
	statusOK = C.int(0)
	// error status must be minus value
	statusEPERM   = errorToStatus(syscall.EPERM)
	statusEIO     = errorToStatus(syscall.EIO)
	statusEINVAL  = errorToStatus(syscall.EINVAL)
	statusEEXIST  = errorToStatus(syscall.EEXIST)
	statusEBADFD  = errorToStatus(syscall.EBADFD)
	statusEACCES  = errorToStatus(syscall.EACCES)
	statusEMFILE  = errorToStatus(syscall.EMFILE)
	statusENOTDIR = errorToStatus(syscall.ENOTDIR)
	statusERANGE  = errorToStatus(syscall.ERANGE)
	statusENODATA = errorToStatus(syscall.ENODATA)
)

func init() {
	os.Setenv("GODEBUG", "madvdontneed=1")
	data.SetNormalExtentSize(normalExtentSize)
	gClientManager = &clientManager{
		clients: make([]*client, 2),
	}
}

func errorToStatus(err error) C.int {
	if err == nil {
		return 0
	}
	if errno, is := err.(syscall.Errno); is {
		return -C.int(errno)
	}
	return -C.int(syscall.EIO)
}

type clientManager struct {
	nextClientID int64
	clients      []*client
}

func newClient(conf *C.cfs_config_t) *client {
	id := atomic.AddInt64(&gClientManager.nextClientID, 1)
	c := &client{
		id:               id,
		fdmap:            make(map[uint]*file),
		fdset:            bitset.New(maxFdNum),
		inomap:           make(map[uint64]map[uint]bool),
		cwd:              "/",
		inodeDentryCache: make(map[uint64]*cache.DentryCache),
	}

	c.masterAddr = C.GoString(conf.master_addr)
	c.volName = C.GoString(conf.vol_name)
	c.owner = C.GoString(conf.owner)
	c.followerRead, _ = strconv.ParseBool(C.GoString(conf.follower_read))
	c.logDir = C.GoString(conf.log_dir)
	c.logLevel = C.GoString(conf.log_level)
	c.app = C.GoString(conf.app)
	c.useMetaCache = (c.app != appMysql8)
	c.profPort, _ = strconv.ParseUint(strings.Split(C.GoString(conf.prof_port), ",")[0], 10, 64)
	c.autoFlush, _ = strconv.ParseBool(C.GoString(conf.auto_flush))
	c.inodeCache = cache.NewInodeCache(inodeExpiration, maxInodeCache, inodeEvictionInterval, c.useMetaCache)
	c.masterClient = C.GoString(conf.master_client)

	c.readProcErrMap = make(map[string]int)
	c.tracingSamplerType = C.GoString(conf.tracing_sampler_type)
	if val, err := strconv.ParseFloat(C.GoString(conf.tracing_sampler_param), 64); err == nil {
		c.tracingSamplerParam = val
	}
	c.tracingReportAddr = C.GoString(conf.tracing_report_addr)

	// Just skip fd 0, 1, 2, to avoid confusion.
	c.fdset.Set(0).Set(1).Set(2)

	if len(gClientManager.clients) <= int(id) {
		gClientManager.clients = make([]*client, id+1)
	}
	gClientManager.clients[id] = c

	return c
}

func getClient(id int64) (c *client, exist bool) {
	//gClientManager.mu.RLock()
	//defer gClientManager.mu.RUnlock()
	c = gClientManager.clients[id]
	if c == nil {
		exist = false
	}
	exist = true
	return
}

func removeClient(id int64) {
	gClientManager.clients[id] = nil
}

type file struct {
	fd    uint
	ino   uint64
	flags uint32
	mode  uint32
	size  uint64
	pos   uint64

	// save the path for openat, fstat, etc.
	path string
	// symbolic file only
	target []byte
	// dir only
	dirp *dirStream
	// for file write lock
	mu      sync.RWMutex
	logType uint8
}

const (
	BinLogType  = 1
	RedoLogType = 2
)

type dirStream struct {
	pos     int
	dirents []proto.Dentry
}

type client struct {
	// client id allocated by libsdk
	id int64

	// mount config
	masterAddr   string
	volName      string
	owner        string
	followerRead bool
	logDir       string
	logLevel     string
	app          string

	profPort        uint64
	readProcErrMap  map[string]int // key: ip:port, value: count of error
	readProcMapLock sync.Mutex

	masterClient string

	autoFlush bool

	// profiling config
	tracingSamplerType  string
	tracingSamplerParam float64
	tracingReportAddr   string
	tracingFlag         bool

	// runtime context
	cwd    string // current working directory
	fdmap  map[uint]*file
	fdset  *bitset.BitSet
	fdlock sync.RWMutex
	inomap map[uint64]map[uint]bool // all open fd of given ino

	// server info
	mc *master.MasterClient
	mw *meta.MetaWrapper
	ec *data.ExtentClient

	// meta cache
	useMetaCache         bool
	inodeCache           *cache.InodeCache
	inodeDentryCache     map[uint64]*cache.DentryCache
	inodeDentryCacheLock sync.Mutex
}

/*
 * Client operations
 */

//export cfs_new_client
func cfs_new_client(conf *C.cfs_config_t) C.int64_t {
	c := newClient(conf)
	if err := c.start(); err != nil {
		removeClient(c.id)
		return C.int64_t(statusEIO)
	}
	return C.int64_t(c.id)
}

//export cfs_close_client
func cfs_close_client(id C.int64_t) {
	if c, exist := getClient(int64(id)); exist {
		if c.mc != nil {
			_ = c.mc.ClientAPI().ReleaseVolMutex(c.volName)
		}
		if c.ec != nil {
			_ = c.ec.Close(context.Background())
		}
		if c.mw != nil {
			_ = c.mw.Close()
		}
		removeClient(int64(id))
	}
	log.LogFlush()
	runtime.GC()
}

//export cfs_flush_log
func cfs_flush_log() {
	log.LogFlush()
}

/*
 * File operations
 */

//export cfs_close
func cfs_close(id C.int64_t, fd C.int) {
	var (
		path string
		ino  uint64
	)
	defer func() {
		if log.IsDebugEnabled() {
			log.LogDebugf("cfs_close: id(%v) fd(%v) path(%v) ino(%v)", id, fd, path, ino)
		}
	}()
	c, exist := getClient(int64(id))
	if !exist {
		return
	}
	f := c.releaseFD(uint(fd))
	if f == nil {
		return
	}
	path = f.path
	ino = f.ino

	var tracer = tracing.NewTracer("cfs_close").
		SetTag("fd", uint(fd)).
		SetTag("path", f.path)
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_close))
	defer ump.AfterTPUs(tpObject, nil)

	c.flush(ctx, f.ino)
	c.closeStream(f)
}

//export cfs_open
func cfs_open(id C.int64_t, path *C.char, flags C.int, mode C.mode_t) (re C.int) {
	return _cfs_open(id, path, flags, mode, -1)
}

func _cfs_open(id C.int64_t, path *C.char, flags C.int, mode C.mode_t, fd C.int) (re C.int) {
	var (
		c   *client
		ino uint64
		err error
	)
	defer func() {
		if re < 0 && err == nil {
			err = syscall.Errno(-re)
		}
		if r := recover(); r != nil || (re < 0 && re != errorToStatus(syscall.ENOENT)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) path(%v) ino(%v) flags(%v) mode(%v) fd(%v) re(%v) err(%v)", id, C.GoString(path), ino, flags, mode, fd, re, err)
			handleError(c, "cfs_open", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) path(%v) ino(%v) flags(%v) mode(%v) fd(%v) re(%v) err(%v)", id, C.GoString(path), ino, flags, mode, fd, re, err)
				log.LogDebugf("cfs_open: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_open").
		SetTag("id", int64(id)).
		SetTag("path", C.GoString(path)).
		SetTag("flags", uint32(flags)).
		SetTag("mode", uint32(mode))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_open))
	defer ump.AfterTPUs(tpObject, nil)

	fuseMode := uint32(mode) & uint32(0777)
	fuseFlags := uint32(flags) &^ uint32(0x8000)
	accFlags := fuseFlags & uint32(C.O_ACCMODE)
	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo

	// According to POSIX, flags must include one of the following
	// access modes: O_RDONLY, O_WRONLY, or O_RDWR.
	// But when using glibc, O_CREAT can be used independently (e.g. MySQL).
	if fuseFlags&uint32(C.O_CREAT) != 0 {
		dirpath, name := gopath.Split(absPath)
		dirInode, err := c.lookupPath(ctx, dirpath)
		if err != nil {
			return errorToStatus(err)
		}
		if len(name) == 0 {
			return statusEINVAL
		}
		inode, err := c.getDentry(ctx, dirInode, name)
		var newInfo *proto.InodeInfo
		if err == nil {
			if fuseFlags&uint32(C.O_EXCL) != 0 {
				return statusEEXIST
			} else {
				newInfo, err = c.getInode(ctx, inode)
			}
		} else if err == syscall.ENOENT {
			newInfo, err = c.create(ctx, dirInode, name, fuseMode, uint32(os.Getuid()), uint32(os.Getgid()), nil)
			if err != nil {
				return errorToStatus(err)
			}
		} else {
			return errorToStatus(err)
		}
		info = newInfo
	} else {
		var newInfo *proto.InodeInfo
		for newInfo, err = c.getInodeByPath(ctx, absPath); err == nil && fuseFlags&uint32(C.O_NOFOLLOW) == 0 && proto.IsSymlink(newInfo.Mode); {
			absPath := c.absPath(string(newInfo.Target))
			newInfo, err = c.getInodeByPath(ctx, absPath)
		}
		if err != nil {
			return errorToStatus(err)
		}
		info = newInfo
	}

	ino = info.Inode
	f := c.allocFD(info.Inode, fuseFlags, info.Mode, info.Target, int(fd))
	if f == nil {
		return statusEMFILE
	}
	f.size = info.Size
	f.path = absPath

	if proto.IsRegular(info.Mode) {
		c.ec.OpenStream(f.ino)
		if fuseFlags&uint32(C.O_TRUNC) != 0 {
			if accFlags != uint32(C.O_WRONLY) && accFlags != uint32(C.O_RDWR) {
				c.closeStream(f)
				c.releaseFD(f.fd)
				return statusEACCES
			}
			if err = c.truncate(ctx, f.ino, 0); err != nil {
				c.closeStream(f)
				c.releaseFD(f.fd)
				return statusEIO
			}
			info.Size = 0
		}
		c.ec.RefreshExtentsCache(ctx, f.ino)
	}
	f.size = info.Size
	f.path = absPath
	if strings.Contains(absPath, "ib_logfile") {
		f.logType = RedoLogType
	} else if strings.Contains(absPath, "mysql-bin") {
		f.logType = BinLogType
	}
	return C.int(f.fd)
}

//export cfs_openat
func cfs_openat(id C.int64_t, dirfd C.int, path *C.char, flags C.int, mode C.mode_t) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}

	return _cfs_open(id, C.CString(absPath), flags, mode, -1)
}

//export cfs_openat_fd
func cfs_openat_fd(id C.int64_t, dirfd C.int, path *C.char, flags C.int, mode C.mode_t, fd C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}

	return _cfs_open(id, C.CString(absPath), flags, mode, fd)
}

//export cfs_rename
func cfs_rename(id C.int64_t, from *C.char, to *C.char) (re C.int) {
	var (
		c   *client
		err error
	)
	defer func() {
		if r := recover(); r != nil || (re < 0 && re != errorToStatus(syscall.ENOENT)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) from(%v) to(%v) re(%v) err(%v)", id, C.GoString(from), C.GoString(to), re, err)
			handleError(c, "cfs_rename", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) from(%v) to(%v) re(%v) err(%v)", id, C.GoString(from), C.GoString(to), re, err)
				log.LogDebugf("cfs_rename: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_rename").
		SetTag("from", C.GoString(from)).
		SetTag("to", C.GoString(to))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_rename))
	defer ump.AfterTPUs(tpObject, nil)

	absFrom := c.absPath(C.GoString(from))
	absTo := c.absPath(C.GoString(to))
	if strings.Contains(absTo, absFrom) {
		if absTo == absFrom {
			return statusEINVAL
		}
		// can't make a directory a subdirectory of itself
		if absTo[len(absFrom)] == '/' {
			return statusEINVAL
		}
	}

	srcDirPath, srcName := gopath.Split(absFrom)
	srcDirInode, err := c.lookupPath(ctx, srcDirPath)
	if err != nil {
		return errorToStatus(err)
	}
	// mv /d/child /d
	if srcDirPath == (absTo + "/") {
		return statusOK
	}

	c.invalidateDentry(srcDirInode, srcName)
	c.inodeCache.Delete(ctx, srcDirInode)
	dstInfo, err := c.getInodeByPath(ctx, absTo)
	if err == nil && proto.IsDir(dstInfo.Mode) {
		err = c.mw.Rename_ll(ctx, srcDirInode, srcName, dstInfo.Inode, srcName)
		if err != nil {
			return errorToStatus(err)
		}
		return statusOK
	}

	dstDirPath, dstName := gopath.Split(absTo)
	dstDirInode, err := c.lookupPath(ctx, dstDirPath)
	if err != nil {
		return errorToStatus(err)
	}
	// If dstName exist when renaming, the inode of the dstName will be updated to the inode of the srcName.
	// So, the dstName shuold be invalidated, too,
	c.invalidateDentry(dstDirInode, dstName)
	c.inodeCache.Delete(ctx, dstDirInode)
	err = c.mw.Rename_ll(ctx, srcDirInode, srcName, dstDirInode, dstName)
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_renameat
func cfs_renameat(id C.int64_t, fromDirfd C.int, from *C.char, toDirfd C.int, to *C.char) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absFromPath, err := c.absPathAt(fromDirfd, from)
	if err != nil {
		return statusEINVAL
	}

	absToPath, err := c.absPathAt(toDirfd, to)
	if err != nil {
		return statusEINVAL
	}

	return cfs_rename(id, C.CString(absFromPath), C.CString(absToPath))
}

//export cfs_truncate
func cfs_truncate(id C.int64_t, path *C.char, len C.off_t) (re C.int) {
	var (
		c     *client
		inode uint64
		err   error
	)
	defer func() {
		if r := recover(); r != nil || (re < 0 && re != errorToStatus(syscall.ENOENT)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) path(%v) ino(%v) len(%v) re(%v) err(%v)", id, C.GoString(path), inode, len, re, err)
			handleError(c, "cfs_truncate", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) path(%v) ino(%v) len(%v) re(%v) err(%v)", id, C.GoString(path), inode, len, re, err)
				log.LogDebugf("cfs_truncate: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_truncate").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path)).
		SetTag("len", uint32(len))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_truncate))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(path))
	inode, err = c.lookupPath(ctx, absPath)
	if err != nil {
		return errorToStatus(err)
	}

	err = c.truncate(ctx, inode, int(len))
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_ftruncate
func cfs_ftruncate(id C.int64_t, fd C.int, len C.off_t) (re C.int) {
	var (
		c    *client
		path string
		ino  uint64
		err  error
	)
	defer func() {
		if r := recover(); r != nil || (re < 0 && re != errorToStatus(syscall.ENOENT)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) len(%v) re(%v) err(%v)", id, fd, path, ino, len, re, err)
			handleError(c, "cfs_ftruncate", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) len(%v) re(%v) err(%v)", id, fd, path, ino, len, re, err)
				log.LogDebugf("cfs_ftruncate: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}
	path = f.path
	ino = f.ino

	var tracer = tracing.NewTracer("cfs_ftruncate").
		SetTag("volume", c.volName).
		SetTag("fd", uint(fd)).
		SetTag("path", f.path).
		SetTag("len", uint32(len))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_ftruncate))
	defer ump.AfterTPUs(tpObject, nil)

	err = c.truncate(ctx, f.ino, int(len))
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_fallocate
func cfs_fallocate(id C.int64_t, fd C.int, mode C.int, offset C.off_t, len C.off_t) (re C.int) {
	var (
		c         *client
		path      string
		ino, size uint64
		err       error
	)
	defer func() {
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) mode(%v) offset(%v) len(%v) re(%v) err(%v)", id, fd, path, ino, size, mode, offset, len, re, err)
			handleError(c, "cfs_fallocate", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) mode(%v) offset(%v) len(%v) re(%v) err(%v)", id, fd, path, ino, size, mode, offset, len, re, err)
				log.LogDebugf("cfs_fallocate: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}
	path = f.path
	ino = f.ino

	var tracer = tracing.NewTracer("cfs_fallocate").
		SetTag("volume", c.volName).
		SetTag("fd", uint(fd)).
		SetTag("path", f.path).
		SetTag("mode", uint32(mode)).
		SetTag("offset", uint32(offset)).
		SetTag("len", uint32(len))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_fallocate))
	defer ump.AfterTPUs(tpObject, nil)

	info, err := c.getInode(ctx, f.ino)
	if err != nil {
		return errorToStatus(err)
	}
	size = info.Size

	if uint32(mode) == 0 {
		if uint64(offset+len) <= info.Size {
			return statusOK
		}
	} else if uint32(mode) == uint32(C.FALLOC_FL_KEEP_SIZE) ||
		uint32(mode) == uint32(C.FALLOC_FL_KEEP_SIZE|C.FALLOC_FL_PUNCH_HOLE) {
		// CFS does not support FALLOC_FL_PUNCH_HOLE for now. We cheat here.
		return statusOK
	} else {
		// unimplemented
		return statusEINVAL
	}

	err = c.truncate(ctx, info.Inode, int(offset+len))
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_posix_fallocate
func cfs_posix_fallocate(id C.int64_t, fd C.int, offset C.off_t, len C.off_t) (re C.int) {
	var (
		c         *client
		path      string
		ino, size uint64
		err       error
	)
	defer func() {
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) offset(%v) len(%v) re(%v) err(%v)", id, fd, path, ino, size, offset, len, re, err)
			handleError(c, "cfs_posix_fallocate", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) offset(%v) len(%v) re(%v) err(%v)", id, fd, path, ino, size, offset, len, re, err)
				log.LogDebugf("cfs_posix_fallocate: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}
	path = f.path
	ino = f.ino

	var tracer = tracing.NewTracer("cfs_posix_fallocate").
		SetTag("volume", c.volName).
		SetTag("fd", uint(fd)).
		SetTag("path", f.path).
		SetTag("offset", uint32(offset)).
		SetTag("len", uint32(len))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_posix_fallocate))
	defer ump.AfterTPUs(tpObject, nil)

	info, err := c.getInode(ctx, f.ino)
	if err != nil {
		return errorToStatus(err)
	}
	size = info.Size

	if uint64(offset+len) <= info.Size {
		return statusOK
	}

	err = c.truncate(ctx, info.Inode, int(offset+len))
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_flush
func cfs_flush(id C.int64_t, fd C.int) (re C.int) {
	var (
		c    *client
		path string
		ino  uint64
		err  error
	)
	defer func() {
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) re(%v) err(%v)", id, fd, path, ino, re, err)
			handleError(c, "cfs_flush", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) re(%v) err(%v)", id, fd, path, ino, re, err)
				log.LogDebugf("cfs_flush: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}
	path = f.path
	ino = f.ino

	if !proto.IsRegular(f.mode) {
		// Some application may call fdatasync() after open a directory.
		// In this situation, CFS will do nothing.
		return statusOK
	}

	var tracer = tracing.NewTracer("cfs_flush").
		SetTag("fd", uint(fd)).
		SetTag("path", f.path)
	defer tracer.Finish()
	var ctx = tracer.Context()
	act := ump_cfs_flush
	if f.logType == RedoLogType {
		act = ump_cfs_flush_redolog
	} else if f.logType == BinLogType {
		act = ump_cfs_flush_binlog
	}
	tpObject1 := ump.BeforeTP(c.umpFunctionKeyFast(act))
	tpObject2 := ump.BeforeTP(c.umpFunctionGeneralKeyFast(act))
	defer func() {
		ump.AfterTPUs(tpObject1, nil)
		ump.AfterTPUs(tpObject2, nil)
	}()

	if err = c.flush(ctx, f.ino); err != nil {
		return statusEIO
	}
	return statusOK
}

/*
 * Directory operations
 */

//export cfs_mkdirs
func cfs_mkdirs(id C.int64_t, path *C.char, mode C.mode_t) (re C.int) {
	var (
		c   *client
		err error
	)
	defer func() {
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) path(%v) mode(%v) re(%v) err(%v)%s", id, C.GoString(path), mode, re, err, stack)
			handleError(c, "cfs_mkdirs", msg)
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	dirpath := c.absPath(C.GoString(path))
	if dirpath == "/" {
		return statusEEXIST
	}

	var tracer = tracing.NewTracer("cfs_mkdirs").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path)).
		SetTag("mode", uint32(mode))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_mkdirs))
	defer ump.AfterTPUs(tpObject, nil)

	pino := proto.RootIno
	dirs := strings.Split(dirpath, "/")
	fuseMode := uint32(mode)&0777 | uint32(os.ModeDir)
	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		child, err := c.getDentry(ctx, pino, dir)
		if err != nil {
			if err == syscall.ENOENT {
				info, err := c.create(ctx, pino, dir, fuseMode, uid, gid, nil)
				if err != nil {
					return errorToStatus(err)
				}
				child = info.Inode
			} else {
				return errorToStatus(err)
			}
		}
		pino = child
	}

	return statusOK
}

//export cfs_mkdirsat
func cfs_mkdirsat(id C.int64_t, dirfd C.int, path *C.char, mode C.mode_t) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}
	return cfs_mkdirs(id, C.CString(absPath), mode)
}

//export cfs_rmdir
func cfs_rmdir(id C.int64_t, path *C.char) (re C.int) {
	var (
		c   *client
		err error
	)
	defer func() {
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) path(%v) re(%v) err(%v)", id, C.GoString(path), re, err)
			handleError(c, "cfs_rmdir", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) path(%v) re(%v) err(%v)", id, C.GoString(path), re, err)
				log.LogDebugf("cfs_rmdir: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_rmdir").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_rmdir))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(path))
	if absPath == "/" {
		return statusOK
	}
	dirpath, name := gopath.Split(absPath)
	dirInode, err := c.lookupPath(ctx, dirpath)
	if err != nil {
		return errorToStatus(err)
	}

	_, err = c.delete(ctx, dirInode, name, true)
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_getcwd
func cfs_getcwd(id C.int64_t) *C.char {
	c, exist := getClient(int64(id))
	if !exist {
		return C.CString("")
	}
	return C.CString(c.cwd)
}

//export cfs_chdir
func cfs_chdir(id C.int64_t, path *C.char) (re C.int) {
	var ino uint64
	defer func() {
		if log.IsDebugEnabled() {
			log.LogDebugf("cfs_chdir: id(%v) path(%v) ino(%v) re(%v)", id, path, ino, re)
		}
	}()
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_chdir").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	cwd := c.absPath(C.GoString(path))
	dirInfo, err := c.getInodeByPath(ctx, cwd)
	if err != nil {
		return errorToStatus(err)
	}
	ino = dirInfo.Inode
	if !proto.IsDir(dirInfo.Mode) {
		return statusENOTDIR
	}
	c.cwd = cwd
	return statusOK
}

//export cfs_fchdir
func cfs_fchdir(id C.int64_t, fd C.int, buf unsafe.Pointer, size C.int) (re C.int) {
	var (
		path string
		ino  uint64
	)
	defer func() {
		if log.IsDebugEnabled() {
			log.LogDebugf("cfs_fchdir: id(%v) fd(%v) path(%v) ino(%v)", id, path, ino)
		}
	}()
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil || f.path == "" {
		return statusEBADFD
	}
	path = f.path
	ino = f.ino

	if !proto.IsDir(f.mode) {
		return statusENOTDIR
	}

	if int(size) < len(f.path)+1 {
		return statusERANGE
	}

	if buf != nil {
		var buffer []byte
		hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
		hdr.Data = uintptr(buf)
		hdr.Len = len(f.path) + 1
		hdr.Cap = len(f.path) + 1
		copy(buffer, f.path)
		copy(buffer[len(f.path):], "\000")
	}

	c.cwd = f.path
	return statusOK
}

//export cfs_getdents
func cfs_getdents(id C.int64_t, fd C.int, buf unsafe.Pointer, count C.int) (n C.int) {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}

	var tracer = tracing.NewTracer("cfs_getents").
		SetTag("volume", c.volName).
		SetTag("fd", uint(fd)).
		SetTag("path", f.path).
		SetTag("count", uint(count))
	defer tracer.Finish()
	var ctx = tracer.Context()

	if f.dirp == nil {
		f.dirp = &dirStream{}
		dentries, err := c.mw.ReadDir_ll(ctx, f.ino)
		if err != nil {
			return errorToStatus(err)
		}
		f.dirp.dirents = dentries
	}

	dirp := f.dirp
	var dp *C.struct_dirent
	for dirp.pos < len(dirp.dirents) && n < count {
		// the d_name member in struct dirent is array of length 256,
		// so the bytes beyond 255 are truncated
		nameLen := len(dirp.dirents[dirp.pos].Name)
		if nameLen >= 256 {
			nameLen = 255
		}

		// the file name may be shorter than the predefined d_name
		align := unsafe.Alignof(*(*C.struct_dirent)(nil))
		size := unsafe.Sizeof(*(*C.struct_dirent)(nil)) + uintptr(nameLen+1-256)
		reclen := C.int(math.Ceil(float64(size)/float64(align))) * C.int(align)
		if n+reclen > count {
			if n > 0 {
				return n
			} else {
				return statusEINVAL
			}
		}

		dp = (*C.struct_dirent)(unsafe.Pointer(uintptr(buf) + uintptr(n)))
		dp.d_ino = C.ino_t(dirp.dirents[dirp.pos].Inode)
		// the d_off is an opaque value in modern filesystems
		dp.d_off = 0
		dp.d_reclen = C.ushort(reclen)
		if proto.IsRegular(dirp.dirents[dirp.pos].Type) {
			dp.d_type = C.DT_REG
		} else if proto.IsDir(dirp.dirents[dirp.pos].Type) {
			dp.d_type = C.DT_DIR
		} else if proto.IsSymlink(dirp.dirents[dirp.pos].Type) {
			dp.d_type = C.DT_LNK
		} else {
			dp.d_type = C.DT_UNKNOWN
		}

		hdr := (*reflect.StringHeader)(unsafe.Pointer(&dirp.dirents[dirp.pos].Name))
		C.memcpy(unsafe.Pointer(&dp.d_name), unsafe.Pointer(hdr.Data), C.size_t(nameLen))
		dp.d_name[nameLen] = 0

		// advance cursor
		dirp.pos++
		n += C.int(dp.d_reclen)
	}

	return n
}

/*
 * Link operations
 */

//export cfs_link
func cfs_link(id C.int64_t, oldpath *C.char, newpath *C.char) (re C.int) {
	var (
		c   *client
		err error
	)
	defer func() {
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) oldpath(%v) newpath(%v) re(%v) err(%v)%s", id, C.GoString(oldpath), C.GoString(newpath), re, err, stack)
			handleError(c, "cfs_link", msg)
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_link").
		SetTag("volume", c.volName).
		SetTag("oldpath", C.GoString(oldpath)).
		SetTag("newpath", C.GoString(newpath))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_link))
	defer ump.AfterTPUs(tpObject, nil)

	inode, err := c.lookupPath(ctx, c.absPath(C.GoString(oldpath)))
	if err != nil {
		return errorToStatus(err)
	}

	absPath := c.absPath(C.GoString(newpath))
	dirPath, name := gopath.Split(absPath)
	dirInode, err := c.lookupPath(ctx, dirPath)
	if err != nil {
		return errorToStatus(err)
	}

	_, err = c.mw.Link(ctx, dirInode, name, inode)
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_linkat
func cfs_linkat(id C.int64_t, oldDirfd C.int, oldPath *C.char,
	newDirfd C.int, newPath *C.char, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absOldPath, err := c.absPathAt(oldDirfd, oldPath)
	if err != nil {
		return statusEINVAL
	}

	absNewPath, err := c.absPathAt(newDirfd, newPath)
	if err != nil {
		return statusEINVAL
	}
	return cfs_link(id, C.CString(absOldPath), C.CString(absNewPath))
}

//export cfs_symlink
func cfs_symlink(id C.int64_t, target *C.char, linkPath *C.char) (re C.int) {
	var (
		c   *client
		err error
	)
	defer func() {
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) target(%v) linkPath(%v) re(%v) err(%v)%s", id, C.GoString(target), C.GoString(linkPath), re, err, stack)
			handleError(c, "cfs_symlink", msg)
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_symlink").
		SetTag("volume", c.volName).
		SetTag("target", C.GoString(target)).
		SetTag("linkPath", C.GoString(linkPath))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_symlink))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(linkPath))
	dirpath, name := gopath.Split(absPath)
	dirInode, err := c.lookupPath(ctx, dirpath)
	if err != nil {
		return errorToStatus(err)
	}

	_, err = c.getDentry(ctx, dirInode, name)
	if err == nil {
		return statusEEXIST
	} else if err != syscall.ENOENT {
		return errorToStatus(err)
	}

	_, err = c.create(ctx, dirInode, name, proto.Mode(os.ModeSymlink|os.ModePerm), uint32(os.Getuid()), uint32(os.Getgid()), []byte(c.absPath(C.GoString(target))))
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_symlinkat
func cfs_symlinkat(id C.int64_t, target *C.char, dirfd C.int, linkPath *C.char) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, linkPath)
	if err != nil {
		return statusEINVAL
	}
	return cfs_symlink(id, target, C.CString(absPath))
}

//export cfs_unlink
func cfs_unlink(id C.int64_t, path *C.char) (re C.int) {
	var (
		c   *client
		ino uint64
		err error
	)
	defer func() {
		if r := recover(); r != nil || (re < 0 && re != errorToStatus(syscall.ENOENT)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) path(%v) ino(%v) re(%v) err(%v)", id, C.GoString(path), ino, re, err)
			handleError(c, "cfs_unlink", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) path(%v) ino(%v) re(%v) err(%v)", id, C.GoString(path), ino, re, err)
				log.LogDebugf("cfs_unlink: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_unlink").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_unlink))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(path))
	info, err := c.getInodeByPath(ctx, absPath)
	if err != nil {
		return errorToStatus(err)
	}
	ino = info.Inode
	if proto.IsDir(info.Mode) {
		return statusEPERM
	}

	dirpath, name := gopath.Split(absPath)
	dirInode, err := c.lookupPath(ctx, dirpath)
	if err != nil {
		return errorToStatus(err)
	}
	info, err = c.delete(ctx, dirInode, name, false)
	if err != nil {
		return errorToStatus(err)
	}

	if info != nil {
		c.mw.Evict(ctx, info.Inode, true)
	}
	return 0
}

//export cfs_unlinkat
func cfs_unlinkat(id C.int64_t, dirfd C.int, path *C.char, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}

	if uint32(flags)&uint32(C.AT_REMOVEDIR) != 0 {
		return cfs_rmdir(id, C.CString(absPath))
	} else {
		return cfs_unlink(id, C.CString(absPath))
	}
}

//export cfs_readlink
func cfs_readlink(id C.int64_t, path *C.char, buf *C.char, size C.size_t) (re C.ssize_t) {
	var (
		c   *client
		ino uint64
		err error
	)
	defer func() {
		msg := fmt.Sprintf("id(%v) path(%v) ino(%v) size(%v) re(%v) err(%v)", id, C.GoString(path), ino, size, re, err)
		if r := recover(); r != nil || (re < 0 && re != C.ssize_t(errorToStatus(syscall.ENOENT)) && re != C.ssize_t(statusEINVAL)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			handleError(c, "cfs_readlink", fmt.Sprintf("%s%s", msg, stack))
		} else {
			log.LogDebugf("cfs_readlink: %s", msg)
		}
	}()

	if int(size) < 0 {
		return C.ssize_t(statusEINVAL)
	}
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	var tracer = tracing.NewTracer("cfs_readlink").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path)).
		SetTag("size", int(size))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_readlink))
	defer ump.AfterTPUs(tpObject, nil)

	info, err := c.getInodeByPath(ctx, c.absPath(C.GoString(path)))
	if err != nil {
		return C.ssize_t(errorToStatus(err))
	}
	if !proto.IsSymlink(info.Mode) {
		return C.ssize_t(statusEINVAL)
	}
	ino = info.Inode

	if len(info.Target) < int(size) {
		size = C.size_t(len(info.Target))
	}
	hdr := (*reflect.StringHeader)(unsafe.Pointer(&info.Target))
	C.memcpy(unsafe.Pointer(buf), unsafe.Pointer(hdr.Data), size)
	return C.ssize_t(size)
}

//export cfs_readlinkat
func cfs_readlinkat(id C.int64_t, dirfd C.int, path *C.char, buf *C.char, size C.size_t) C.ssize_t {
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return C.ssize_t(statusEINVAL)
	}
	return cfs_readlink(id, C.CString(absPath), buf, size)
}

/*
 * Basic file attributes
 */

/*
 * Although there is no device belonging to CFS, value of stat.st_dev MUST be set.
 * Sometimes, this value may be used to determine the identity of a file.
 * (e.g. in Mysql initialization stage, storage\myisam\mi_open.c
 * mi_open_share() -> my_is_same_file())
 */

//export cfs_stat
func cfs_stat(id C.int64_t, path *C.char, stat *C.struct_stat) C.int {
	return _cfs_stat(id, path, stat, 0)
}

func _cfs_stat(id C.int64_t, path *C.char, stat *C.struct_stat, flags C.int) (re C.int) {
	var (
		c         *client
		ino, size uint64
		err       error
	)
	defer func() {
		msg := fmt.Sprintf("id(%v) path(%v) ino(%v) flags(%v) size(%v) re(%v) err(%v)", id, C.GoString(path), ino, flags, size, re, err)
		if r := recover(); r != nil || (re < 0 && re != errorToStatus(syscall.ENOENT)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			handleError(c, "cfs_stat", fmt.Sprintf("%s%s", msg, stack))
		} else {
			log.LogDebugf("cfs_stat: %s", msg)
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_stat").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_stat))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && (uint32(flags)&uint32(C.AT_SYMLINK_NOFOLLOW) == 0) && proto.IsSymlink(info.Mode); {
		absPath := c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return errorToStatus(err)
	}
	ino = info.Inode
	size = info.Size

	// fill up the stat
	stat.st_dev = 0
	stat.st_ino = C.ino_t(info.Inode)
	stat.st_size = C.off_t(info.Size)
	stat.st_nlink = C.nlink_t(info.Nlink)
	stat.st_blksize = C.blksize_t(defaultBlkSize)
	stat.st_uid = C.uid_t(info.Uid)
	stat.st_gid = C.gid_t(info.Gid)

	if info.Size%512 != 0 {
		stat.st_blocks = C.blkcnt_t(info.Size>>9) + 1
	} else {
		stat.st_blocks = C.blkcnt_t(info.Size >> 9)
	}
	// fill up the mode
	if proto.IsRegular(info.Mode) {
		stat.st_mode = C.mode_t(C.S_IFREG) | C.mode_t(info.Mode&0777)
	} else if proto.IsDir(info.Mode) {
		stat.st_mode = C.mode_t(C.S_IFDIR) | C.mode_t(info.Mode&0777)
	} else if proto.IsSymlink(info.Mode) {
		stat.st_mode = C.mode_t(C.S_IFLNK) | C.mode_t(info.Mode&0777)
	} else {
		stat.st_mode = C.mode_t(C.S_IFSOCK) | C.mode_t(info.Mode&0777)
	}

	// fill up the time struct
	var st_atim, st_mtim, st_ctim C.struct_timespec
	t := info.AccessTime.UnixNano()
	st_atim.tv_sec = C.time_t(t / 1e9)
	st_atim.tv_nsec = C.long(t % 1e9)
	stat.st_atim = st_atim

	t = info.ModifyTime.UnixNano()
	st_mtim.tv_sec = C.time_t(t / 1e9)
	st_mtim.tv_nsec = C.long(t % 1e9)
	stat.st_mtim = st_mtim

	t = info.CreateTime.UnixNano()
	st_ctim.tv_sec = C.time_t(t / 1e9)
	st_ctim.tv_nsec = C.long(t % 1e9)
	stat.st_ctim = st_ctim
	return statusOK
}

//export cfs_stat64
func cfs_stat64(id C.int64_t, path *C.char, stat *C.struct_stat64) C.int {
	return _cfs_stat64(id, path, stat, 0)
}

func _cfs_stat64(id C.int64_t, path *C.char, stat *C.struct_stat64, flags C.int) (re C.int) {
	var (
		c         *client
		ino, size uint64
		err       error
	)
	defer func() {
		msg := fmt.Sprintf("id(%v) path(%v) ino(%v) flags(%v) size(%v) re(%v) err(%v)", id, C.GoString(path), ino, flags, size, re, err)
		if r := recover(); r != nil || (re < 0 && re != errorToStatus(syscall.ENOENT)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			handleError(c, "cfs_stat64", fmt.Sprintf("%s%s", msg, stack))
		} else {
			log.LogDebugf("cfs_stat64: %s", msg)
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_stat64").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_stat64))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && (uint32(flags)&uint32(C.AT_SYMLINK_NOFOLLOW) == 0) && proto.IsSymlink(info.Mode); {
		absPath = c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return errorToStatus(err)
	}
	ino = info.Inode
	size = info.Size

	// fill up the stat
	stat.st_dev = 0
	stat.st_ino = C.ino64_t(info.Inode)
	stat.st_size = C.off64_t(info.Size)
	stat.st_nlink = C.nlink_t(info.Nlink)
	stat.st_blksize = C.blksize_t(defaultBlkSize)
	stat.st_uid = C.uid_t(info.Uid)
	stat.st_gid = C.gid_t(info.Gid)

	if info.Size%512 != 0 {
		stat.st_blocks = C.blkcnt64_t(info.Size>>9) + 1
	} else {
		stat.st_blocks = C.blkcnt64_t(info.Size >> 9)
	}
	// fill up the mode
	if proto.IsRegular(info.Mode) {
		stat.st_mode = C.mode_t(C.S_IFREG) | C.mode_t(info.Mode&0777)
	} else if proto.IsDir(info.Mode) {
		stat.st_mode = C.mode_t(C.S_IFDIR) | C.mode_t(info.Mode&0777)
	} else if proto.IsSymlink(info.Mode) {
		stat.st_mode = C.mode_t(C.S_IFLNK) | C.mode_t(info.Mode&0777)
	} else {
		stat.st_mode = C.mode_t(C.S_IFSOCK) | C.mode_t(info.Mode&0777)
	}

	// fill up the time struct
	var st_atim, st_mtim, st_ctim C.struct_timespec
	t := info.AccessTime.UnixNano()
	st_atim.tv_sec = C.time_t(t / 1e9)
	st_atim.tv_nsec = C.long(t % 1e9)
	stat.st_atim = st_atim

	t = info.ModifyTime.UnixNano()
	st_mtim.tv_sec = C.time_t(t / 1e9)
	st_mtim.tv_nsec = C.long(t % 1e9)
	stat.st_mtim = st_mtim

	t = info.CreateTime.UnixNano()
	st_ctim.tv_sec = C.time_t(t / 1e9)
	st_ctim.tv_nsec = C.long(t % 1e9)
	stat.st_ctim = st_ctim
	return statusOK
}

//export cfs_lstat
func cfs_lstat(id C.int64_t, path *C.char, stat *C.struct_stat) C.int {
	return _cfs_stat(id, path, stat, C.AT_SYMLINK_NOFOLLOW)
}

//export cfs_lstat64
func cfs_lstat64(id C.int64_t, path *C.char, stat *C.struct_stat64) C.int {
	return _cfs_stat64(id, path, stat, C.AT_SYMLINK_NOFOLLOW)
}

//export cfs_fstat
func cfs_fstat(id C.int64_t, fd C.int, stat *C.struct_stat) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}
	return _cfs_stat(id, C.CString(f.path), stat, 0)
}

//export cfs_fstat64
func cfs_fstat64(id C.int64_t, fd C.int, stat *C.struct_stat64) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}
	return _cfs_stat64(id, C.CString(f.path), stat, 0)
}

//export cfs_fstatat
func cfs_fstatat(id C.int64_t, dirfd C.int, path *C.char, stat *C.struct_stat, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}
	return _cfs_stat(id, C.CString(absPath), stat, flags)
}

//export cfs_fstatat64
func cfs_fstatat64(id C.int64_t, dirfd C.int, path *C.char, stat *C.struct_stat64, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}
	return _cfs_stat64(id, C.CString(absPath), stat, flags)
}

//export cfs_chmod
func cfs_chmod(id C.int64_t, path *C.char, mode C.mode_t) C.int {
	return _cfs_chmod(id, path, mode, 0)
}

func _cfs_chmod(id C.int64_t, path *C.char, mode C.mode_t, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_chmod").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path)).
		SetTag("mode", uint32(mode)).
		SetTag("flags", int(flags))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_chmod))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	var err error
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && (uint32(flags)&uint32(C.AT_SYMLINK_NOFOLLOW) == 0) && proto.IsSymlink(info.Mode); {
		absPath := c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return errorToStatus(err)
	}

	err = c.setattr(ctx, info, proto.AttrMode, uint32(mode), 0, 0, 0, 0)

	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_fchmod
func cfs_fchmod(id C.int64_t, fd C.int, mode C.mode_t) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}

	var tracer = tracing.NewTracer("cfs_fchmod").
		SetTag("volume", c.volName).
		SetTag("fd", int(fd)).
		SetTag("path", f.path).
		SetTag("mode", uint32(mode))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_fchmod))
	defer ump.AfterTPUs(tpObject, nil)

	info, err := c.getInode(ctx, f.ino)
	if err != nil {
		return errorToStatus(err)
	}

	err = c.setattr(ctx, info, proto.AttrMode, uint32(mode), 0, 0, 0, 0)
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_fchmodat
func cfs_fchmodat(id C.int64_t, dirfd C.int, path *C.char, mode C.mode_t, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}
	return _cfs_chmod(id, C.CString(absPath), mode, flags)
}

//export cfs_chown
func cfs_chown(id C.int64_t, path *C.char, uid C.uid_t, gid C.gid_t) C.int {
	return _cfs_chown(id, path, uid, gid, 0)
}

//export cfs_lchown
func cfs_lchown(id C.int64_t, path *C.char, uid C.uid_t, gid C.gid_t) C.int {
	return _cfs_chown(id, path, uid, gid, C.AT_SYMLINK_NOFOLLOW)
}

func _cfs_chown(id C.int64_t, path *C.char, uid C.uid_t, gid C.gid_t, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_chown").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path)).
		SetTag("uid", uint32(uid)).
		SetTag("gid", uint32(gid)).
		SetTag("flags", int(flags))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_chown))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	var err error
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && (uint32(flags)&uint32(C.AT_SYMLINK_NOFOLLOW) == 0) && proto.IsSymlink(info.Mode); {
		absPath := c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return errorToStatus(err)
	}

	err = c.setattr(ctx, info, proto.AttrUid|proto.AttrGid, 0, uint32(uid), uint32(gid), 0, 0)

	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_fchown
func cfs_fchown(id C.int64_t, fd C.int, uid C.uid_t, gid C.gid_t) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}

	var tracer = tracing.NewTracer("cfs_fchown").
		SetTag("volume", c.volName).
		SetTag("fd", int(fd)).
		SetTag("path", f.path).
		SetTag("uid", uint32(uid)).
		SetTag("gid", uint32(gid))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_fchown))
	defer ump.AfterTPUs(tpObject, nil)

	info, err := c.getInode(ctx, f.ino)
	if err != nil {
		return errorToStatus(err)
	}

	err = c.setattr(ctx, info, proto.AttrUid|proto.AttrGid, 0, uint32(uid), uint32(gid), 0, 0)

	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_fchownat
func cfs_fchownat(id C.int64_t, dirfd C.int, path *C.char, uid C.uid_t, gid C.gid_t, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}
	return _cfs_chown(id, C.CString(absPath), uid, gid, flags)
}

//export cfs_utimens
func cfs_utimens(id C.int64_t, path *C.char, times *C.struct_timespec, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_utimens").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_utimens))
	defer ump.AfterTPUs(tpObject, nil)

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	var err error
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && (uint32(flags)&uint32(C.AT_SYMLINK_NOFOLLOW) == 0) && proto.IsSymlink(info.Mode); {
		absPath := c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return errorToStatus(err)
	}

	var atime, mtime int64
	var ap, mp *C.struct_timespec
	var ts C.struct_timespec
	ap = times
	mp = (*C.struct_timespec)(unsafe.Pointer(uintptr(unsafe.Pointer(times)) + unsafe.Sizeof(ts)))
	// CFS time precision is second
	now := time.Now().Unix()
	if times == nil {
		atime = now
	} else if ap.tv_nsec == C.UTIME_NOW {
		atime = now
	} else if ap.tv_nsec == C.UTIME_OMIT {
		atime = 0
	} else {
		atime = int64(ap.tv_sec)
	}
	if times == nil {
		mtime = now
	} else if mp.tv_nsec == C.UTIME_NOW {
		mtime = now
	} else if mp.tv_nsec == C.UTIME_OMIT {
		mtime = 0
	} else {
		mtime = int64(mp.tv_sec)
	}
	err = c.setattr(ctx, info, proto.AttrAccessTime|proto.AttrModifyTime, 0, 0, 0, mtime, atime)

	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

//export cfs_futimens
func cfs_futimens(id C.int64_t, fd C.int, times *C.struct_timespec) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}

	return cfs_utimens(id, C.CString(f.path), times, 0)
}

//export cfs_utimensat
func cfs_utimensat(id C.int64_t, dirfd C.int, path *C.char, times *C.struct_timespec, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}

	return cfs_utimens(id, C.CString(absPath), times, flags)
}

/*
 * In access like functiuons, permission check is ignored, only existence check
 * is done. The responsibility of file permissions is left to upper applications.
 */

//export cfs_access
func cfs_access(id C.int64_t, path *C.char, mode C.int) C.int {
	return cfs_faccessat(id, C.AT_FDCWD, path, mode, 0)
}

//export cfs_faccessat
func cfs_faccessat(id C.int64_t, dirfd C.int, path *C.char, mode C.int, flags C.int) (re C.int) {
	var (
		c   *client
		err error
	)
	defer func() {
		if r := recover(); r != nil || (re < 0 && re != errorToStatus(syscall.ENOENT)) {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) dirfd(%v) path(%v) mode(%v) flags(%v) re(%v) err(%v)%s", id, dirfd, C.GoString(path), mode, flags, re, err, stack)
			handleError(c, "cfs_faccessat", msg)
		}
	}()

	re = statusOK
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_faccessat").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_faccessat))
	defer ump.AfterTPUs(tpObject, nil)

	absPath, err := c.absPathAt(dirfd, path)
	if err != nil {
		return statusEINVAL
	}
	inode, err := c.lookupPath(ctx, absPath)
	var info *proto.InodeInfo
	for err == nil && (uint32(flags)&uint32(C.AT_SYMLINK_NOFOLLOW) == 0) {
		info, err = c.getInode(ctx, inode)
		if err != nil {
			return errorToStatus(err)
		}
		if !proto.IsSymlink(info.Mode) {
			break
		}
		absPath = c.absPath(string(info.Target))
		inode, err = c.lookupPath(ctx, absPath)
	}
	if err != nil {
		return errorToStatus(err)
	}
	return statusOK
}

/*
 * Extended file attributes
 */

//export cfs_setxattr
func cfs_setxattr(id C.int64_t, path *C.char, name *C.char, value unsafe.Pointer, size C.size_t, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_setxattr").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	var err error
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && proto.IsSymlink(info.Mode); {
		absPath := c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return errorToStatus(err)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(value)
	hdr.Len = int(size)
	hdr.Cap = int(size)

	err = c.mw.XAttrSet_ll(ctx, info.Inode, []byte(C.GoString(name)), buffer)
	if err != nil {
		return statusEIO
	}

	return statusOK
}

//export cfs_lsetxattr
func cfs_lsetxattr(id C.int64_t, path *C.char, name *C.char, value unsafe.Pointer, size C.size_t, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_lsetxattr").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	absPath := c.absPath(C.GoString(path))
	inode, err := c.lookupPath(ctx, absPath)
	if err != nil {
		return errorToStatus(err)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(value)
	hdr.Len = int(size)
	hdr.Cap = int(size)

	err = c.mw.XAttrSet_ll(ctx, inode, []byte(C.GoString(name)), buffer)
	if err != nil {
		return statusEIO
	}

	return statusOK
}

//export cfs_fsetxattr
func cfs_fsetxattr(id C.int64_t, fd C.int, name *C.char, value unsafe.Pointer, size C.size_t, flags C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}

	var tracer = tracing.NewTracer("cfs_fsetxattr").
		SetTag("volume", c.volName).
		SetTag("path", f.path)
	defer tracer.Finish()
	var ctx = tracer.Context()

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(value)
	hdr.Len = int(size)
	hdr.Cap = int(size)

	err := c.mw.XAttrSet_ll(ctx, f.ino, []byte(C.GoString(name)), buffer)
	if err != nil {
		return statusEIO
	}

	return statusOK
}

//export cfs_getxattr
func cfs_getxattr(id C.int64_t, path *C.char, name *C.char, value unsafe.Pointer, size C.size_t) C.ssize_t {
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	var tracer = tracing.NewTracer("cfs_getxattr").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	var err error
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && proto.IsSymlink(info.Mode); {
		absPath := c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return C.ssize_t(errorToStatus(err))
	}

	xattr, err := c.mw.XAttrGet_ll(ctx, info.Inode, C.GoString(name))
	if err != nil {
		return C.ssize_t(statusEIO)
	}

	val, ok := xattr.XAttrs[C.GoString(name)]
	if !ok {
		return C.ssize_t(statusENODATA)
	}

	if int(size) == 0 {
		return C.ssize_t(len(val))
	} else if int(size) < len(val) {
		return C.ssize_t(statusERANGE)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(value)
	hdr.Len = int(size)
	hdr.Cap = int(size)
	copy(buffer, val)

	return C.ssize_t(len(val))
}

//export cfs_lgetxattr
func cfs_lgetxattr(id C.int64_t, path *C.char, name *C.char, value unsafe.Pointer, size C.size_t) C.ssize_t {
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	var tracer = tracing.NewTracer("cfs_lgetxattr").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	absPath := c.absPath(C.GoString(path))
	inode, err := c.lookupPath(ctx, absPath)
	if err != nil {
		return C.ssize_t(errorToStatus(err))
	}
	xattr, err := c.mw.XAttrGet_ll(ctx, inode, C.GoString(name))
	if err != nil {
		return C.ssize_t(statusEIO)
	}

	val, ok := xattr.XAttrs[C.GoString(name)]
	if !ok {
		return C.ssize_t(statusENODATA)
	}

	if int(size) == 0 {
		return C.ssize_t(len(val))
	} else if int(size) < len(val) {
		return C.ssize_t(statusERANGE)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(value)
	hdr.Len = int(size)
	hdr.Cap = int(size)
	copy(buffer, val)

	return C.ssize_t(len(val))
}

//export cfs_fgetxattr
func cfs_fgetxattr(id C.int64_t, fd C.int, name *C.char, value unsafe.Pointer, size C.size_t) C.ssize_t {
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return C.ssize_t(statusEBADFD)
	}

	var tracer = tracing.NewTracer("cfs_fgetxattr").
		SetTag("volume", c.volName).
		SetTag("path", f.path)
	defer tracer.Finish()
	var ctx = tracer.Context()

	xattr, err := c.mw.XAttrGet_ll(ctx, f.ino, C.GoString(name))
	if err != nil {
		return C.ssize_t(statusEIO)
	}

	val, ok := xattr.XAttrs[C.GoString(name)]
	if !ok {
		return C.ssize_t(statusENODATA)
	}

	if int(size) == 0 {
		return C.ssize_t(len(val))
	} else if int(size) < len(val) {
		return C.ssize_t(statusERANGE)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(value)
	hdr.Len = int(size)
	hdr.Cap = int(size)
	copy(buffer, val)

	return C.ssize_t(len(val))
}

//export cfs_listxattr
func cfs_listxattr(id C.int64_t, path *C.char, list *C.char, size C.size_t) C.ssize_t {
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	var tracer = tracing.NewTracer("cfs_listxattr").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	var err error
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && proto.IsSymlink(info.Mode); {
		absPath := c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return C.ssize_t(errorToStatus(err))
	}

	names, err := c.mw.XAttrsList_ll(ctx, info.Inode)
	if err != nil {
		return C.ssize_t(statusEIO)
	}

	total := 0
	for _, val := range names {
		total += len(val) + 1
	}
	if int(size) == 0 {
		return C.ssize_t(total)
	} else if int(size) < total {
		return C.ssize_t(statusERANGE)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(unsafe.Pointer(list))
	hdr.Len = int(size)
	hdr.Cap = int(size)
	offset := 0
	for _, val := range names {
		copy(buffer[offset:], val)
		offset += len(val)
		copy(buffer[offset:], "\x00")
		offset += 1
	}

	return C.ssize_t(total)
}

//export cfs_llistxattr
func cfs_llistxattr(id C.int64_t, path *C.char, list *C.char, size C.size_t) C.ssize_t {
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	var tracer = tracing.NewTracer("cfs_llistxattr").
		SetTag("volume", c.volName).
		SetTag("path", C.GoString(path))
	defer tracer.Finish()
	var ctx = tracer.Context()

	absPath := c.absPath(C.GoString(path))
	inode, err := c.lookupPath(ctx, absPath)
	if err != nil {
		return C.ssize_t(errorToStatus(err))
	}
	names, err := c.mw.XAttrsList_ll(ctx, inode)
	if err != nil {
		return C.ssize_t(statusEIO)
	}

	total := 0
	for _, val := range names {
		total += len(val) + 1
	}
	if int(size) == 0 {
		return C.ssize_t(total)
	} else if int(size) < total {
		return C.ssize_t(statusERANGE)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(unsafe.Pointer(list))
	hdr.Len = int(size)
	hdr.Cap = int(size)
	offset := 0
	for _, val := range names {
		copy(buffer[offset:], val)
		offset += len(val)
		copy(buffer[offset:], "\x00")
		offset += 1
	}

	return C.ssize_t(total)
}

//export cfs_flistxattr
func cfs_flistxattr(id C.int64_t, fd C.int, list *C.char, size C.size_t) C.ssize_t {
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return C.ssize_t(statusEBADFD)
	}

	info, err := c.getInode(context.Background(), f.ino)
	if err != nil {
		return C.ssize_t(errorToStatus(err))
	}

	names, err := c.mw.XAttrsList_ll(context.Background(), info.Inode)
	if err != nil {
		return C.ssize_t(statusEIO)
	}

	total := 0
	for _, val := range names {
		total += len(val) + 1
	}
	if int(size) == 0 {
		return C.ssize_t(total)
	} else if int(size) < total {
		return C.ssize_t(statusERANGE)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(unsafe.Pointer(list))
	hdr.Len = int(size)
	hdr.Cap = int(size)
	offset := 0
	for _, val := range names {
		copy(buffer[offset:], val)
		offset += len(val)
		copy(buffer[offset:], "\x00")
		offset += 1
	}

	return C.ssize_t(total)
}

//export cfs_removexattr
func cfs_removexattr(id C.int64_t, path *C.char, name *C.char) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	var tracer = tracing.NewTracer("cfs_removexattr").
		SetTag("path", C.GoString(path)).
		SetTag("name", C.GoString(name))
	defer tracer.Finish()
	var ctx = tracer.Context()

	absPath := c.absPath(C.GoString(path))
	var info *proto.InodeInfo
	var err error
	for info, err = c.getInodeByPath(ctx, absPath); err == nil && proto.IsSymlink(info.Mode); {
		absPath := c.absPath(string(info.Target))
		info, err = c.getInodeByPath(ctx, absPath)
	}
	if err != nil {
		return errorToStatus(err)
	}

	err = c.mw.XAttrDel_ll(context.Background(), info.Inode, C.GoString(name))
	if err != nil {
		return statusEIO
	}

	return statusOK
}

//export cfs_lremovexattr
func cfs_lremovexattr(id C.int64_t, path *C.char, name *C.char) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	absPath := c.absPath(C.GoString(path))
	inode, err := c.lookupPath(context.Background(), absPath)
	if err != nil {
		return errorToStatus(err)
	}

	err = c.mw.XAttrDel_ll(context.Background(), inode, C.GoString(name))
	if err != nil {
		return statusEIO
	}

	return statusOK
}

//export cfs_fremovexattr
func cfs_fremovexattr(id C.int64_t, fd C.int, name *C.char) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}

	err := c.mw.XAttrDel_ll(context.Background(), f.ino, C.GoString(name))
	if err != nil {
		return statusEIO
	}

	return statusOK
}

/*
 * File descriptor manipulations
 */

//export cfs_fcntl
func cfs_fcntl(id C.int64_t, fd C.int, cmd C.int, arg C.int) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}

	if cmd == C.F_DUPFD || cmd == C.F_DUPFD_CLOEXEC {
		newfd := c.copyFile(uint(fd), uint(arg))
		if newfd == 0 {
			return statusEINVAL
		}
		return C.int(newfd)
	} else if cmd == C.F_SETFL {
		// According to POSIX, F_SETFL will replace the flags with exactly
		// the provided, i.e. someone should call F_GETFL before F_SETFL.
		// But some applications (e.g. Mysql) don't call F_GETFL before F_SETFL.
		// We compromise with such applications here.
		f.flags |= uint32(arg) & uint32((C.O_APPEND | C.O_ASYNC | C.O_DIRECT | C.O_NOATIME | C.O_NONBLOCK))
		return statusOK
	}

	// unimplemented
	return statusEINVAL
}

//export cfs_fcntl_lock
func cfs_fcntl_lock(id C.int64_t, fd C.int, cmd C.int, lk *C.struct_flock) C.int {
	c, exist := getClient(int64(id))
	if !exist {
		return statusEINVAL
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return statusEBADFD
	}

	if (cmd == C.F_SETLK || cmd == C.F_SETLKW) && lk.l_whence == C.SEEK_SET && lk.l_start == 0 && lk.l_len == 0 {
		if lk.l_type == C.F_WRLCK {
			f.mu.Lock()
		} else if lk.l_type == C.F_UNLCK {
			f.mu.Unlock()
		} else {
			return statusEINVAL
		}
		return statusOK
	}

	// unimplemented
	return statusEINVAL
}

/*
 * Read & Write
 */

/*
 * https://man7.org/linux/man-pages/man2/pwrite.2.html
 * POSIX requires that opening a file with the O_APPEND flag should have
 * no effect on the location at which pwrite() writes data.  However, on
 * Linux, if a file is opened with O_APPEND, pwrite() appends data to
 * the end of the file, regardless of the value of offset.
 *
 * CFS complies with POSIX
 */

//export cfs_read
func cfs_read(id C.int64_t, fd C.int, buf unsafe.Pointer, size C.size_t) C.ssize_t {
	return _cfs_read(id, fd, buf, size, -1)
}

//export cfs_pread
func cfs_pread(id C.int64_t, fd C.int, buf unsafe.Pointer, size C.size_t, off C.off_t) C.ssize_t {
	return _cfs_read(id, fd, buf, size, off)
}

func _cfs_read(id C.int64_t, fd C.int, buf unsafe.Pointer, size C.size_t, off C.off_t) (re C.ssize_t) {
	var (
		c      *client
		path   string
		ino    uint64
		err    error
		offset int
	)
	defer func() {
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) offset(%v) re(%v) err(%v)", id, fd, path, ino, size, offset, re, err)
			handleError(c, "cfs_read", fmt.Sprintf("%s%s", msg, stack))
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) offset(%v) re(%v) err(%v)", id, fd, path, ino, size, offset, re, err)
				log.LogDebugf("cfs_read: %s", msg)
			}
		}
	}()

	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return C.ssize_t(statusEBADFD)
	}
	path = f.path
	ino = f.ino

	var tracer = tracing.NewTracer("cfs_read").
		SetTag("volume", c.volName).
		SetTag("fd", f.fd).
		SetTag("path", f.path)
	defer tracer.Finish()
	var ctx = tracer.Context()

	tpObject1 := ump.BeforeTP(c.umpFunctionKeyFast(ump_cfs_read))
	tpObject2 := ump.BeforeTP(c.umpFunctionGeneralKeyFast(ump_cfs_read))
	defer func() {
		ump.AfterTPUs(tpObject1, nil)
		ump.AfterTPUs(tpObject2, nil)
	}()

	accFlags := f.flags & uint32(C.O_ACCMODE)
	if accFlags == uint32(C.O_WRONLY) {
		return C.ssize_t(statusEACCES)
	}

	var buffer []byte

	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(buf)
	hdr.Len = int(size)
	hdr.Cap = int(size)

	// off >= 0 stands for pread
	offset = int(off)
	if off < 0 {
		offset = int(f.pos)
	}
	n, hasHole, err := c.ec.Read(ctx, f.ino, buffer, offset, len(buffer))
	extentNotExist := err != nil && strings.Contains(err.Error(), "extent does not exist")
	if err != nil && err != io.EOF && !extentNotExist {
		return C.ssize_t(statusEIO)
	}
	if extentNotExist || n < int(size) || hasHole {
		c.ec.RefreshExtentsCache(ctx, f.ino)
		n, _, err = c.ec.Read(ctx, f.ino, buffer, offset, len(buffer))
	}
	if err != nil && err != io.EOF {
		return C.ssize_t(statusEIO)
	}
	if err != nil && err != io.EOF {
		return C.ssize_t(statusEIO)
	}

	if off < 0 {
		f.pos += uint64(n)
	}
	return C.ssize_t(n)
}

//export cfs_readv
func cfs_readv(id C.int64_t, fd C.int, iov *C.struct_iovec, iovcnt C.int) C.ssize_t {
	return _cfs_readv(id, fd, iov, iovcnt, -1)
}

//export cfs_preadv
func cfs_preadv(id C.int64_t, fd C.int, iov *C.struct_iovec, iovcnt C.int, off C.off_t) C.ssize_t {
	return _cfs_readv(id, fd, iov, iovcnt, off)
}

func _cfs_readv(id C.int64_t, fd C.int, iov *C.struct_iovec, iovcnt C.int, off C.off_t) C.ssize_t {
	var iovbuf []C.struct_iovec
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&iovbuf))
	hdr.Data = uintptr(unsafe.Pointer(iov))
	hdr.Len = int(iovcnt)
	hdr.Cap = int(iovcnt)

	var size int
	for i := 0; i < int(iovcnt); i++ {
		size += int(iovbuf[i].iov_len)
	}
	buffer := make([]byte, size, size)
	hdr = (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	ssize := _cfs_read(id, fd, unsafe.Pointer(hdr.Data), C.size_t(size), off)
	if ssize < 0 {
		return ssize
	}

	var buf []byte
	var offset int
	for i := 0; i < int(iovcnt); i++ {
		buf = make([]byte, iovbuf[i].iov_len, iovbuf[i].iov_len)
		hdr = (*reflect.SliceHeader)(unsafe.Pointer(&buf))
		hdr.Data = uintptr(iovbuf[i].iov_base)
		copy(buf, buffer[offset:offset+int(iovbuf[i].iov_len)])
		offset += int(iovbuf[i].iov_len)
	}
	return ssize
}

//export cfs_write
func cfs_write(id C.int64_t, fd C.int, buf unsafe.Pointer, size C.size_t) C.ssize_t {
	return _cfs_write(id, fd, buf, size, -1)
}

//export cfs_pwrite
func cfs_pwrite(id C.int64_t, fd C.int, buf unsafe.Pointer, size C.size_t, off C.off_t) C.ssize_t {
	return _cfs_write(id, fd, buf, size, off)
}

func _cfs_write(id C.int64_t, fd C.int, buf unsafe.Pointer, size C.size_t, off C.off_t) (re C.ssize_t) {
	var (
		c       *client
		f       *file
		path    string
		ino     uint64
		err     error
		offset  int
		flagBuf bytes.Buffer
	)
	defer func() {
		var fileSize uint64 = 0
		if f != nil {
			fileSize = f.size
		}
		if r := recover(); r != nil || re < 0 {
			var stack string
			if r != nil {
				stack = fmt.Sprintf(" %v :\n%s", r, string(debug.Stack()))
			}
			msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) offset(%v) flag(%v) fileSize(%v) re(%v) err(%v)", id, fd, path, ino, size, offset, strings.Trim(flagBuf.String(), "|"), fileSize, re, err)
			handleError(c, "cfs_write", fmt.Sprintf("%s%s", msg, stack))
		} else if re < C.ssize_t(size) {
			msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) offset(%v) flag(%v) fileSize(%v) re(%v) err(%v)", id, fd, path, ino, size, offset, strings.Trim(flagBuf.String(), "|"), fileSize, re, err)
			log.LogWarnf("cfs_write: %s", msg)
		} else {
			if log.IsDebugEnabled() {
				msg := fmt.Sprintf("id(%v) fd(%v) path(%v) ino(%v) size(%v) offset(%v) flag(%v) fileSize(%v) re(%v) err(%v)", id, fd, path, ino, size, offset, strings.Trim(flagBuf.String(), "|"), fileSize, re, err)
				log.LogDebugf("cfs_write: %s", msg)
			}
		}
	}()

	once.Do(func() {
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
	})

	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	f = c.getFile(uint(fd))
	if f == nil {
		return C.ssize_t(statusEBADFD)
	}
	path = f.path
	ino = f.ino
	if log.IsDebugEnabled() {
		if f.flags&uint32(C.O_DIRECT) != 0 {
			flagBuf.WriteString("O_DIRECT|")
		} else if f.flags&uint32(C.O_SYNC) != 0 {
			flagBuf.WriteString("O_SYNC|")
		} else if f.flags&uint32(C.O_DSYNC) != 0 {
			flagBuf.WriteString("O_DSYNC|")
		}
	}
	//var tracer = tracing.NewTracer("cfs_write").
	//	SetTag("volume", c.volName).
	//	SetTag("fd", f.fd).
	//	SetTag("path", f.path)
	//defer tracer.Finish()
	//var ctx = tracer.Context()
	overWriteBuffer := false
	act := ump_cfs_write
	if f.logType == BinLogType {
		act = ump_cfs_write_binlog
	} else if f.logType == RedoLogType {
		act = ump_cfs_write_redolog
		if c.app == appMysql8 {
			overWriteBuffer = true
		}
	}
	tpObject1 := ump.BeforeTP(c.umpFunctionKeyFast(act))
	tpObject2 := ump.BeforeTP(c.umpFunctionGeneralKeyFast(act))
	defer func() {
		ump.AfterTPUs(tpObject1, nil)
		ump.AfterTPUs(tpObject2, nil)
	}()

	accFlags := f.flags & uint32(C.O_ACCMODE)
	if accFlags != uint32(C.O_WRONLY) && accFlags != uint32(C.O_RDWR) {
		return C.ssize_t(statusEACCES)
	}

	var buffer []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	hdr.Data = uintptr(buf)
	hdr.Len = int(size)
	hdr.Cap = int(size)

	var flush bool
	if f.flags&uint32(C.O_SYNC) != 0 || f.flags&uint32(C.O_DSYNC) != 0 {
		flush = true
	}

	// off >= 0 stands for pwrite
	offset = int(off)
	if off < 0 {
		if f.flags&uint32(C.O_APPEND) != 0 {
			f.pos = f.size
		}
		offset = int(f.pos)
	}

	n, isROW, err := c.ec.Write(nil, f.ino, offset, buffer, false, overWriteBuffer)
	if err != nil {
		return C.ssize_t(statusEIO)
	}

	if flush {
		if err = c.flush(nil, f.ino); err != nil {
			return C.ssize_t(statusEIO)
		}
	}

	if isROW && isMysql() {
		c.broadcastAllReadProcess(f.ino)
	}

	if off < 0 {
		f.pos += uint64(n)
		if f.size < f.pos {
			c.updateSizeByIno(f.ino, f.pos)
		}
	} else {
		if f.size < uint64(off)+uint64(n) {
			c.updateSizeByIno(f.ino, uint64(off)+uint64(n))
		}
	}
	info := c.inodeCache.Get(nil, f.ino)
	if info != nil {
		info.Size = f.size
		c.inodeCache.Put(info)
	}
	return C.ssize_t(n)
}

//export cfs_writev
func cfs_writev(id C.int64_t, fd C.int, iov *C.struct_iovec, iovcnt C.int) C.ssize_t {
	return _cfs_writev(id, fd, iov, iovcnt, -1)
}

//export cfs_pwritev
func cfs_pwritev(id C.int64_t, fd C.int, iov *C.struct_iovec, iovcnt C.int, off C.off_t) C.ssize_t {
	return _cfs_writev(id, fd, iov, iovcnt, off)
}

func _cfs_writev(id C.int64_t, fd C.int, iov *C.struct_iovec, iovcnt C.int, off C.off_t) C.ssize_t {
	c, exist := getClient(int64(id))
	if !exist {
		return C.ssize_t(statusEINVAL)
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return C.ssize_t(statusEBADFD)
	}

	accFlags := f.flags & uint32(C.O_ACCMODE)
	if accFlags != uint32(C.O_WRONLY) && accFlags != uint32(C.O_RDWR) {
		return C.ssize_t(statusEACCES)
	}

	var iovbuf []C.struct_iovec
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&iovbuf))
	hdr.Data = uintptr(unsafe.Pointer(iov))
	hdr.Len = int(iovcnt)
	hdr.Cap = int(iovcnt)

	var size int
	for i := 0; i < int(iovcnt); i++ {
		size += int(iovbuf[i].iov_len)
	}

	buffer := make([]byte, size, size)
	var buf []byte
	var offset int
	for i := 0; i < int(iovcnt); i++ {
		buf = make([]byte, iovbuf[i].iov_len, iovbuf[i].iov_len)
		hdr = (*reflect.SliceHeader)(unsafe.Pointer(&buf))
		hdr.Data = uintptr(iovbuf[i].iov_base)
		copy(buffer[offset:offset+int(iovbuf[i].iov_len)], buf)
		offset += int(iovbuf[i].iov_len)
	}
	hdr = (*reflect.SliceHeader)(unsafe.Pointer(&buffer))
	return _cfs_write(id, fd, unsafe.Pointer(hdr.Data), C.size_t(size), off)
}

//export cfs_lseek
func cfs_lseek(id C.int64_t, fd C.int, offset C.off64_t, whence C.int) (re C.off64_t) {
	var (
		path string
		ino  uint64
	)
	defer func() {
		if log.IsDebugEnabled() {
			log.LogDebugf("cfs_lseek: id(%v) fd(%v) path(%v) ino(%v) offset(%v) whence(%v) re(%v)", id, fd, path, ino, offset, whence, re)
		}
	}()
	c, exist := getClient(int64(id))
	if !exist {
		return C.off64_t(statusEINVAL)
	}

	f := c.getFile(uint(fd))
	if f == nil {
		return C.off64_t(statusEBADFD)
	}
	path = f.path
	ino = f.ino

	if whence == C.int(C.SEEK_SET) {
		f.pos = uint64(offset)
	} else if whence == C.int(C.SEEK_CUR) {
		f.pos += uint64(offset)
	} else if whence == C.int(C.SEEK_END) {
		f.pos = f.size + uint64(offset)
	}
	return C.off64_t(f.pos)
}

/*
 * internals
 */

func (c *client) absPath(path string) string {
	if !gopath.IsAbs(path) {
		path = gopath.Join(c.cwd, path)
	}
	return gopath.Clean(path)
}

func (c *client) absPathAt(dirfd C.int, path *C.char) (string, error) {
	useDirfd := !gopath.IsAbs(C.GoString(path)) && (dirfd != C.AT_FDCWD)
	var absPath string
	if useDirfd {
		f := c.getFile(uint(dirfd))
		if f == nil || f.path == "" {
			return "", fmt.Errorf("invalid dirfd: %d", dirfd)
		}
		absPath = gopath.Clean(gopath.Join(f.path, C.GoString(path)))
	} else {
		absPath = c.absPath(C.GoString(path))
	}

	return absPath, nil
}

func (c *client) start() (err error) {
	level := log.WarnLevel
	if c.logLevel == "debug" {
		level = log.DebugLevel
	} else if c.logLevel == "info" {
		level = log.InfoLevel
	} else if c.logLevel == "warn" {
		level = log.WarnLevel
	} else if c.logLevel == "error" {
		level = log.ErrorLevel
	}
	if len(c.logDir) == 0 {
		return fmt.Errorf("empty dir")
	}
	if _, err = log.InitLog(c.logDir, moduleName, level, nil); err != nil {
		fmt.Println(err)
		return
	}

	outputFilePath := gopath.Join(c.logDir, moduleName, "output.log")
	var outputFile *os.File
	if outputFile, err = os.OpenFile(outputFilePath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0666); err != nil {
		fmt.Println(err)
		return
	}
	syslog.SetOutput(outputFile)
	defer func() {
		if err != nil {
			syslog.Printf("start kernel bypass client failed: err(%v)\n", err)
		}
		outputFile.Sync()
		outputFile.Close()
	}()

	if c.profPort == 0 {
		err = fmt.Errorf("invalid prof port")
		fmt.Println(err)
		return
	}

	masters := strings.Split(c.masterAddr, ",")
	mc := master.NewMasterClient(masters, false)
	err = mc.ClientAPI().ApplyVolMutex(c.volName)
	if err == proto.ErrVolWriteMutexUnable {
		err = nil
	}
	if err != nil {
		fmt.Println(err)
		return
	}

	var mw *meta.MetaWrapper
	if mw, err = meta.NewMetaWrapper(&meta.MetaConfig{
		Volume:        c.volName,
		Masters:       masters,
		ValidateOwner: true,
		Owner:         c.owner,
		InfiniteRetry: true,
	}); err != nil {
		fmt.Println(err)
		return
	}

	var ec *data.ExtentClient
	if ec, err = data.NewExtentClient(&data.ExtentConfig{
		Volume:            c.volName,
		Masters:           masters,
		FollowerRead:      c.followerRead,
		OnInsertExtentKey: mw.InsertExtentKey,
		OnGetExtents:      mw.GetExtents,
		OnTruncate:        mw.Truncate,
		TinySize:          data.NoUseTinyExtent,
		AutoFlush:         c.autoFlush,
		MetaWrapper:       mw,
		ExtentMerge:       isMysql(),
	}); err != nil {
		fmt.Println(err)
		return
	}

	c.mc = mc
	c.mw = mw
	c.ec = ec

	// Init tracing, CAN'T be closed, or the tracing data will be lost
	tracing.TraceInit(moduleName, c.tracingSamplerType, c.tracingSamplerParam, c.tracingReportAddr)

	go func() {
		listenErr := http.ListenAndServe(fmt.Sprintf(":%v", c.profPort), nil)
		if listenErr != nil && isMysql() {
			fmt.Println(listenErr)
			os.Exit(1)
		}
	}()
	http.HandleFunc(log.GetLogPath, log.GetLog)
	http.HandleFunc("/version", GetVersionHandleFunc)
	http.HandleFunc(ControlReadProcessRegister, c.registerReadProcStatusHandleFunc)
	http.HandleFunc(ControlBroadcastRefreshExtents, c.broadcastRefreshExtentsHandleFunc)
	http.HandleFunc(ControlGetReadProcs, c.getReadProcs)

	// metric
	if err = ump.InitUmp(moduleName, "jdos_chubaofs-node"); err != nil {
		fmt.Println(err)
		return
	}
	c.initUmpKeys()
	exporter.InitRole(mw.Cluster(), moduleName)

	c.registerReadProcStatus(true)

	// version
	cmd, _ := os.Executable()
	versionInfo := fmt.Sprintf("ChubaoFS %s\nBranch: %s\nVersion: %s\nCommit: %s\nBuild: %s %s %s %s\nCMD: %s\n", moduleName, BranchName, proto.BaseVersion, CommitID, runtime.Version(), runtime.GOOS, runtime.GOARCH, BuildTime, cmd)
	syslog.Printf(versionInfo)
	cfgStr, _ := json.Marshal(struct {
		ClusterName string `json:"clusterName"`
	}{mw.Cluster()})
	cfg := config.LoadConfigString(string(cfgStr))
	go version.ReportVersionSchedule(cfg, masters, versionInfo)
	return
}

func (c *client) allocFD(ino uint64, flags, mode uint32, target []byte, fd int) *file {
	c.fdlock.Lock()
	defer c.fdlock.Unlock()
	var (
		ok      bool
		real_fd uint
	)
	if fd <= 0 {
		real_fd, ok = c.fdset.NextClear(0)
		if !ok || real_fd > maxFdNum {
			return nil
		}
	} else {
		real_fd = uint(fd)
		if c.fdset.Test(real_fd) {
			return nil
		}
	}
	c.fdset.Set(real_fd)
	f := &file{fd: real_fd, ino: ino, flags: flags, mode: mode, target: target}
	c.fdmap[real_fd] = f
	fdmap, ok := c.inomap[ino]
	if !ok {
		fdmap = make(map[uint]bool)
		c.inomap[ino] = fdmap
	}
	fdmap[real_fd] = true
	return f
}

func (c *client) getFile(fd uint) *file {
	c.fdlock.RLock()
	f := c.fdmap[fd]
	c.fdlock.RUnlock()
	return f
}

func (c *client) copyFile(fd uint, newfd uint) uint {
	c.fdlock.Lock()
	defer c.fdlock.Unlock()
	newfd, ok := c.fdset.NextClear(newfd)
	if !ok || newfd > maxFdNum {
		return 0
	}
	c.fdset.Set(newfd)
	f := c.fdmap[fd]
	if f == nil {
		return 0
	}
	newfile := &file{fd: f.fd, ino: f.ino, flags: f.flags, mode: f.mode, size: f.size, pos: f.pos, path: f.path, target: f.target, dirp: f.dirp}
	newfile.fd = newfd
	c.fdmap[newfd] = newfile
	return newfd
}

func (c *client) create(ctx context.Context, parentID uint64, name string, mode, uid, gid uint32, target []byte) (info *proto.InodeInfo, err error) {
	info, err = c.mw.Create_ll(ctx, parentID, name, mode, uid, gid, target)
	c.inodeCache.Delete(ctx, parentID)
	c.inodeCache.Put(info)
	return
}

func (c *client) delete(ctx context.Context, parentID uint64, name string, isDir bool) (info *proto.InodeInfo, err error) {
	info, err = c.mw.Delete_ll(ctx, parentID, name, isDir)
	c.inodeCache.Delete(ctx, parentID)
	c.invalidateDentry(parentID, name)
	return
}

func (c *client) truncate(ctx context.Context, inode uint64, len int) (err error) {
	err = c.ec.Truncate(ctx, inode, len)
	info := c.inodeCache.Get(ctx, inode)
	if info != nil {
		info.Size = uint64(len)
		c.inodeCache.Put(info)
	}
	c.updateSizeByIno(inode, uint64(len))
	return
}

func (c *client) flush(ctx context.Context, inode uint64) (err error) {
	err = c.ec.Flush(ctx, inode)
	//c.inodeCache.Delete(ctx, inode)
	return
}

func (c *client) updateSizeByIno(ino uint64, size uint64) {
	c.fdlock.Lock()
	defer c.fdlock.Unlock()
	fdmap, ok := c.inomap[ino]
	if !ok {
		return
	}
	for fd := range fdmap {
		file, ok := c.fdmap[fd]
		if !ok {
			continue
		}
		file.size = size
	}
}

func (c *client) releaseFD(fd uint) *file {
	c.fdlock.Lock()
	defer c.fdlock.Unlock()
	f, ok := c.fdmap[fd]
	if !ok {
		return nil
	}
	fdmap, ok := c.inomap[f.ino]
	if ok {
		delete(fdmap, fd)
	}
	if len(fdmap) == 0 {
		delete(c.inomap, f.ino)
	}
	delete(c.fdmap, fd)
	c.fdset.Clear(fd)
	return f
}

func (c *client) getInodeByPath(ctx context.Context, path string) (info *proto.InodeInfo, err error) {
	var ino uint64
	ino, err = c.lookupPath(ctx, path)
	if err != nil {
		return
	}
	info, err = c.getInode(ctx, ino)
	return
}

func (c *client) lookupPath(ctx context.Context, path string) (ino uint64, err error) {
	ino = proto.RootIno
	if path != "" && path != "/" {
		dirs := strings.Split(path, "/")
		var child uint64
		for _, dir := range dirs {
			if dir == "/" || dir == "" {
				continue
			}
			child, err = c.getDentry(ctx, ino, dir)
			if err != nil {
				ino = 0
				return
			}
			ino = child
		}
	}
	return
}

func (c *client) getInode(ctx context.Context, ino uint64) (info *proto.InodeInfo, err error) {
	info = c.inodeCache.Get(ctx, ino)
	if info != nil {
		return
	}
	info, err = c.mw.InodeGet_ll(ctx, ino)
	if err != nil {
		return
	}
	c.inodeCache.Put(info)
	return
}

func (c *client) getDentry(ctx context.Context, parentID uint64, name string) (ino uint64, err error) {
	c.inodeDentryCacheLock.Lock()
	defer c.inodeDentryCacheLock.Unlock()
	dentryCache, ok := c.inodeDentryCache[parentID]
	if ok {
		ino, ok = dentryCache.Get(name)
		if ok {
			return
		}
	} else {
		dentryCache = cache.NewDentryCache(dentryValidDuration, c.useMetaCache)
		c.inodeDentryCache[parentID] = dentryCache
	}
	ino, _, err = c.mw.Lookup_ll(ctx, parentID, name)
	if err != nil {
		return
	}
	dentryCache.Put(name, ino)
	return
}

func (c *client) invalidateDentry(parentID uint64, name string) {
	c.inodeDentryCacheLock.Lock()
	defer c.inodeDentryCacheLock.Unlock()
	dentryCache, parentOk := c.inodeDentryCache[parentID]
	if parentOk {
		dentryCache.Delete(name)
		if dentryCache.Count() == 0 {
			delete(c.inodeDentryCache, parentID)
		}
	}
}

func (c *client) setattr(ctx context.Context, info *proto.InodeInfo, valid uint32, mode, uid, gid uint32, mtime, atime int64) error {
	// Only rwx mode bit can be set
	if valid&proto.AttrMode != 0 {
		fuseMode := mode & uint32(0777)
		mode = info.Mode &^ uint32(0777) // clear rwx mode bit
		mode |= fuseMode
	}
	return c.mw.Setattr(ctx, info.Inode, valid, mode, uid, gid, atime, mtime)
}

func (c *client) closeStream(f *file) {
	_ = c.ec.CloseStream(context.Background(), f.ino)
	_ = c.ec.EvictStream(context.Background(), f.ino)
}

func (c *client) broadcastAllReadProcess(ino uint64) {
	c.readProcMapLock.Lock()
	log.LogInfof("broadcastAllReadProcess: readProcessMap(%v)", c.readProcErrMap)
	for readClient, errCount := range c.readProcErrMap {
		if errCount > 3 {
			log.LogInfof("broadcastAllReadProcess: unregister readClient: %s", readClient)
			delete(c.readProcErrMap, readClient)
			continue
		}
		c.broadcastRefreshExtents(readClient, ino)
	}
	c.readProcMapLock.Unlock()
}

func handleError(c *client, act, msg string) {
	log.LogErrorf("%s: %s", act, msg)
	log.LogFlush()

	if c != nil {
		key1 := fmt.Sprintf("%s_%s_warning", c.mw.Cluster(), c.volName)
		errmsg1 := fmt.Sprintf("act(%s) - %s", act, msg)
		ump.Alarm(key1, errmsg1)

		key2 := fmt.Sprintf("%s_%s_warning", c.mw.Cluster(), moduleName)
		errmsg2 := fmt.Sprintf("volume(%s) %s", c.volName, errmsg1)
		ump.Alarm(key2, errmsg2)
		ump.FlushAlarm()
	}
}

func isMysql() bool {
	processName := filepath.Base(os.Args[0])
	return strings.Contains(processName, "mysqld")
}

func main() {}
// Copyright 2018 The CubeFS Authors.
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

package storage

import (
	"encoding/binary"
	"fmt"
	"github.com/cubefs/cubefs/proto"
	"hash/crc32"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/log"
)

const (
	ExtentOpenOpt  = os.O_CREATE | os.O_RDWR | os.O_EXCL
	ExtentHasClose = -1
	SEEK_DATA      = 3
	SEEK_HOLE      = 4
)

const (
	ExtentMaxSize = 1024 * 1024 * 1024 * 1024 * 4 // 4TB
)

type ExtentInfo struct {
	FileID              uint64 `json:"fileId"`
	Size                uint64 `json:"size"`
	Crc                 uint32 `json:"Crc"`
	IsDeleted           bool   `json:"deleted"`
	ModifyTime          int64  `json:"modTime"` // random write not update modify time
	AccessTime          int64  `json:"accessTime"`
	Source              string `json:"src"`
	SnapshotDataOff     uint64 `json:"snapSize"`
	SnapPreAllocDataOff uint64 `json:"snapPreAllocSize"`
	ApplyID             uint64 `json:"applyID"`
}

func (ei *ExtentInfo) TotalSize() uint64 {
	return ei.Size + (ei.SnapshotDataOff - util.ExtentSize)
}

func (ei *ExtentInfo) String() (m string) {
	source := ei.Source
	if source == "" {
		source = "none"
	}
	return fmt.Sprintf("%v_%v_%v_%v_%v_%d_%d_%d", ei.FileID, ei.Size, ei.SnapshotDataOff, ei.IsDeleted, source, ei.ModifyTime, ei.AccessTime, ei.Crc)
}

// SortedExtentInfos defines an array sorted by AccessTime
type SortedExtentInfos []*ExtentInfo

func (extInfos SortedExtentInfos) Len() int {
	return len(extInfos)
}

func (extInfos SortedExtentInfos) Less(i, j int) bool {
	return extInfos[i].AccessTime < extInfos[j].AccessTime
}

func (extInfos SortedExtentInfos) Swap(i, j int) {
	extInfos[i], extInfos[j] = extInfos[j], extInfos[i]
}

// Extent is an implementation of Extent for local regular extent file data management.
// This extent implementation manages all header info and data body in one single entry file.
// Header of extent include inode value of this extent block and Crc blocks of data blocks.
type Extent struct {
	file            *os.File
	filePath        string
	extentID        uint64
	modifyTime      int64
	accessTime      int64
	dataSize        int64
	hasClose        int32
	header          []byte
	snapshotDataOff uint64
	sync.Mutex
}

// NewExtentInCore create and returns a new extent instance.
func NewExtentInCore(name string, extentID uint64) *Extent {
	e := new(Extent)
	e.extentID = extentID
	e.filePath = name
	e.snapshotDataOff = util.ExtentSize
	return e
}

func (e *Extent) String() string {
	return fmt.Sprintf("%v_%v_%v", e.filePath, e.dataSize, e.snapshotDataOff)
}

func (e *Extent) HasClosed() bool {
	return atomic.LoadInt32(&e.hasClose) == ExtentHasClose
}

// Close this extent and release FD.
func (e *Extent) Close() (err error) {
	if e.HasClosed() {
		return
	}
	if err = e.file.Close(); err != nil {
		return
	}
	return
}

func (e *Extent) Exist() (exsit bool) {
	_, err := os.Stat(e.filePath)
	if err != nil {
		if os.IsExist(err) {
			return true
		}
		return false
	}
	return true
}

// InitToFS init extent data info filesystem. If entry file exist and overwrite is true,
// this operation will clear all data of exist entry file and initialize extent header data.
func (e *Extent) InitToFS() (err error) {
	if e.file, err = os.OpenFile(e.filePath, ExtentOpenOpt, 0666); err != nil {
		return err
	}

	defer func() {
		if err != nil {
			e.file.Close()
			os.Remove(e.filePath)
		}
	}()

	if IsTinyExtent(e.extentID) {
		e.dataSize = 0
		return
	}
	atomic.StoreInt64(&e.modifyTime, time.Now().Unix())
	atomic.StoreInt64(&e.accessTime, time.Now().Unix())
	e.dataSize = 0
	return
}

func (e *Extent) GetDataSize(statSize int64) (dataSize int64) {
	var (
		dataStart int64
		holStart  int64
		curOff    int64
		err       error
	)
	if statSize <= util.ExtentSize {
		return statSize
	}
	for {
		// curOff if the hold start and the data end
		curOff, err = e.file.Seek(holStart, SEEK_DATA)
		if err != nil || curOff >= util.ExtentSize || (holStart > 0 && holStart == curOff) {
			log.LogDebugf("GetDataSize statSize %v curOff %v dataStart %v holStart %v, err %v,path %v", statSize, curOff, dataStart, holStart, err, e.filePath)
			break
		}
		log.LogDebugf("GetDataSize statSize %v curOff %v dataStart %v holStart %v, err %v,path %v", statSize, curOff, dataStart, holStart, err, e.filePath)
		dataStart = curOff

		curOff, err = e.file.Seek(dataStart, SEEK_HOLE)
		if err != nil || curOff >= util.ExtentSize || dataStart == curOff {
			log.LogDebugf("GetDataSize statSize %v curOff %v dataStart %v holStart %v, err %v,path %v", statSize, curOff, dataStart, holStart, err, e.filePath)
			break
		}
		log.LogDebugf("GetDataSize statSize %v curOff %v dataStart %v holStart %v, err %v,path %v", statSize, curOff, dataStart, holStart, err, e.filePath)
		holStart = curOff
	}
	log.LogDebugf("GetDataSize statSize %v curOff %v dataStart %v holStart %v, err %v,path %v", statSize, curOff, dataStart, holStart, err, e.filePath)
	dataSize = util.ExtentSize
	if holStart > dataStart { //data cann't move forward only hole include value util.ExtentSize
		dataSize = holStart
	}
	log.LogDebugf("GetDataSize statSize %v curOff %v dataStart %v holStart %v, err %v,path %v", statSize, curOff, dataStart, holStart, err, e.filePath)
	return
}

// RestoreFromFS restores the entity data and status from the file stored on the filesystem.
func (e *Extent) RestoreFromFS() (err error) {
	if e.file, err = os.OpenFile(e.filePath, os.O_RDWR, 0666); err != nil {
		if strings.Contains(err.Error(), syscall.ENOENT.Error()) {
			err = ExtentNotFoundError
		}
		return err
	}
	var (
		info os.FileInfo
	)
	if info, err = e.file.Stat(); err != nil {
		err = fmt.Errorf("stat file %v: %v", e.file.Name(), err)
		return
	}
	if IsTinyExtent(e.extentID) {
		watermark := info.Size()
		if watermark%util.PageSize != 0 {
			watermark = watermark + (util.PageSize - watermark%util.PageSize)
		}
		e.dataSize = watermark
		return
	}

	e.dataSize = e.GetDataSize(info.Size())
	e.snapshotDataOff = util.ExtentSize
	if info.Size() > util.ExtentSize {
		e.snapshotDataOff = uint64(info.Size())
	}

	atomic.StoreInt64(&e.modifyTime, info.ModTime().Unix())

	ts := info.Sys().(*syscall.Stat_t)
	atomic.StoreInt64(&e.accessTime, time.Unix(int64(ts.Atim.Sec), int64(ts.Atim.Nsec)).Unix())
	return
}

// Size returns length of the extent (not including the header).
func (e *Extent) Size() (size int64) {
	return e.dataSize
}

// ModifyTime returns the time when this extent was modified recently.
func (e *Extent) ModifyTime() int64 {
	return atomic.LoadInt64(&e.modifyTime)
}

func IsRandomWrite(writeType int) bool {
	return writeType == RandomWriteType
}

func IsAppendWrite(writeType int) bool {
	return writeType == AppendWriteType
}

func IsAppendRandomWrite(writeType int) bool {
	return writeType == AppendRandomWriteType
}

// WriteTiny performs write on a tiny extent.
func (e *Extent) WriteTiny(data []byte, offset, size int64, crc uint32, writeType int, isSync bool) (err error) {
	e.Lock()
	defer e.Unlock()
	index := offset + size
	if index >= ExtentMaxSize {
		return ExtentIsFullError
	}

	if IsAppendWrite(writeType) && offset != e.dataSize {
		return ParameterMismatchError
	}

	if _, err = e.file.WriteAt(data[:size], int64(offset)); err != nil {
		return
	}
	if isSync {
		if err = e.file.Sync(); err != nil {
			return
		}
	}

	if !IsAppendWrite(writeType) {
		return
	}
	if index%util.PageSize != 0 {
		index = index + (util.PageSize - index%util.PageSize)
	}
	e.dataSize = index

	return
}

// Write writes data to an extent.
func (e *Extent) Write(data []byte, offset, size int64, crc uint32, writeType int, isSync bool, crcFunc UpdateCrcFunc, ei *ExtentInfo) (status uint8, err error) {
	log.LogDebugf("action[Extent.Write] path %v offset %v size %v writeType %v", e.filePath, offset, size, writeType)
	status = proto.OpOk
	if IsTinyExtent(e.extentID) {
		err = e.WriteTiny(data, offset, size, crc, writeType, isSync)
		return
	}

	if err = e.checkWriteOffsetAndSize(writeType, offset, size); err != nil {
		log.LogErrorf("action[Extent.Write] checkWriteOffsetAndSize offset %v size %v writeType %v err %v",
			offset, size, writeType, err)
		return
	}

	log.LogDebugf("action[Extent.Write] path %v offset %v size %v writeType %v", e.filePath, offset, size, writeType)
	// Check if extent file size matches the write offset just in case
	// multiple clients are writing concurrently.
	e.Lock()
	defer e.Unlock()
	log.LogDebugf("action[Extent.Write] offset %v size %v writeType %v", offset, size, writeType)
	if IsAppendWrite(writeType) && e.dataSize != offset {
		err = NewParameterMismatchErr(fmt.Sprintf("extent current size = %v write offset=%v write size=%v", e.dataSize, offset, size))
		log.LogErrorf("action[Extent.Write] NewParameterMismatchErr path %v offset %v size %v writeType %v err %v", e.filePath,
			offset, size, writeType, err)
		status = proto.OpTryOtherExtent
		return
	}

	if _, err = e.file.WriteAt(data[:size], int64(offset)); err != nil {
		log.LogErrorf("action[Extent.Write] offset %v size %v writeType %v err %v", offset, size, writeType, err)
		return
	}

	blockNo := offset / util.BlockSize
	offsetInBlock := offset % util.BlockSize
	defer func() {
		log.LogDebugf("action[Extent.Write] offset %v size %v writeType %v", offset, size, writeType)
		if IsAppendWrite(writeType) {
			atomic.StoreInt64(&e.modifyTime, time.Now().Unix())
			e.dataSize = int64(math.Max(float64(e.dataSize), float64(offset+size)))
			log.LogDebugf("action[Extent.Write] e %v offset %v size %v writeType %v", e, offset, size, writeType)
		} else if IsAppendRandomWrite(writeType) {
			atomic.StoreInt64(&e.modifyTime, time.Now().Unix())
			index := int64(math.Max(float64(e.snapshotDataOff), float64(offset+size)))
			e.snapshotDataOff = uint64(index)
		}
		log.LogDebugf("action[Extent.Write] offset %v size %v writeType %v dataSize %v snapshotDataOff %v",
			offset, size, writeType, e.dataSize, e.snapshotDataOff)
	}()

	if isSync {
		if err = e.file.Sync(); err != nil {
			log.LogDebugf("action[Extent.Write] offset %v size %v writeType %v err %v",
				offset, size, writeType, err)
			return
		}
	}
	if offsetInBlock == 0 && size == util.BlockSize {
		err = crcFunc(e, int(blockNo), crc)
		log.LogDebugf("action[Extent.Write] offset %v size %v writeType %v err %v", offset, size, writeType, err)
		return
	}

	if offsetInBlock+size <= util.BlockSize {
		err = crcFunc(e, int(blockNo), 0)
		log.LogDebugf("action[Extent.Write]  offset %v size %v writeType %v err %v", offset, size, writeType, err)
		return
	}
	log.LogDebugf("action[Extent.Write] offset %v size %v writeType %v", offset, size, writeType)
	if err = crcFunc(e, int(blockNo), 0); err == nil {
		err = crcFunc(e, int(blockNo+1), 0)
	}
	return
}

// Read reads data from an extent.
func (e *Extent) Read(data []byte, offset, size int64, isRepairRead bool) (crc uint32, err error) {
	log.LogDebugf("action[Extent.read] offset %v size %v extent %v", offset, size, e)
	if IsTinyExtent(e.extentID) {
		return e.ReadTiny(data, offset, size, isRepairRead)
	}

	if err = e.checkReadOffsetAndSize(offset, size); err != nil {
		log.LogErrorf("action[Extent.Read] NewParameterMismatchErr offset %v size %v err %v",
			offset, size, err)
		return
	}

	var rSize int
	if rSize, err = e.file.ReadAt(data[:size], offset); err != nil {
		log.LogErrorf("action[Extent.Read] NewParameterMismatchErr offset %v size %v err %v realsize %v",
			offset, size, err, rSize)
		return
	}
	crc = crc32.ChecksumIEEE(data)
	return
}

// ReadTiny read data from a tiny extent.
func (e *Extent) ReadTiny(data []byte, offset, size int64, isRepairRead bool) (crc uint32, err error) {
	_, err = e.file.ReadAt(data[:size], offset)
	if isRepairRead && err == io.EOF {
		err = nil
	}
	crc = crc32.ChecksumIEEE(data[:size])

	return
}

func (e *Extent) checkReadOffsetAndSize(offset, size int64) error {
	if (e.snapshotDataOff == util.ExtentSize && offset > e.Size()) ||
		(e.snapshotDataOff > util.ExtentSize && uint64(offset) > e.snapshotDataOff) {
		return NewParameterMismatchErr(fmt.Sprintf("offset=%v size=%v snapshotDataOff=%v", offset, size, e.snapshotDataOff))
	}
	return nil
}

func (e *Extent) checkWriteOffsetAndSize(writeType int, offset, size int64) error {
	if IsAppendWrite(writeType) {
		if offset+size > util.ExtentSize {
			return NewParameterMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
		}
		if offset >= util.ExtentSize || size == 0 {
			return NewParameterMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
		}

		if size > util.BlockSize {
			return NewParameterMismatchErr(fmt.Sprintf("offset=%v size=%v", offset, size))
		}
	} else if writeType == AppendRandomWriteType {
		log.LogDebugf("action[checkOffsetAndSize] offset %v size %v", offset, size)
		if offset < util.ExtentSize || size == 0 {
			return NewParameterMismatchErr(fmt.Sprintf("writeType=%v offset=%v size=%v", writeType, offset, size))
		}
	}
	return nil
}

// Flush synchronizes data to the disk.
func (e *Extent) Flush() (err error) {
	err = e.file.Sync()
	return
}

func (e *Extent) autoComputeExtentCrc(crcFunc UpdateCrcFunc) (crc uint32, err error) {
	var blockCnt int
	extSize := e.Size()
	if e.snapshotDataOff > util.ExtentSize {
		extSize = int64(e.snapshotDataOff)
	}
	blockCnt = int(extSize / util.BlockSize)
	if extSize%util.BlockSize != 0 {
		blockCnt += 1
	}
	log.LogDebugf("autoComputeExtentCrc. path %v extent %v extent size %v,blockCnt %v", e.filePath, e.extentID, extSize, blockCnt)
	crcData := make([]byte, blockCnt*util.PerBlockCrcSize)
	for blockNo := 0; blockNo < blockCnt; blockNo++ {
		blockCrc := binary.BigEndian.Uint32(e.header[blockNo*util.PerBlockCrcSize : (blockNo+1)*util.PerBlockCrcSize])
		if blockCrc != 0 {
			binary.BigEndian.PutUint32(crcData[blockNo*util.PerBlockCrcSize:(blockNo+1)*util.PerBlockCrcSize], blockCrc)
			continue
		}
		bdata := make([]byte, util.BlockSize)
		offset := int64(blockNo * util.BlockSize)
		readN, err := e.file.ReadAt(bdata[:util.BlockSize], offset)
		if readN == 0 && err != nil {
			log.LogErrorf("autoComputeExtentCrc. path %v extent %v blockNo %v, readN %v err %v", e.filePath, e.extentID, blockNo, readN, err)
			break
		}
		blockCrc = crc32.ChecksumIEEE(bdata[:readN])
		err = crcFunc(e, blockNo, blockCrc)
		if err != nil {
			log.LogErrorf("autoComputeExtentCrc. path %v extent %v blockNo %v, err %v", e.filePath, e.extentID, blockNo, err)
			return 0, nil
		}
		log.LogDebugf("autoComputeExtentCrc. path %v extent %v blockCrc %v,blockNo %v", e.filePath, e.extentID, blockCrc, blockNo)
		binary.BigEndian.PutUint32(crcData[blockNo*util.PerBlockCrcSize:(blockNo+1)*util.PerBlockCrcSize], blockCrc)
	}
	crc = crc32.ChecksumIEEE(crcData)
	log.LogDebugf("autoComputeExtentCrc. path %v extent %v crc %v", e.filePath, e.extentID, crc)
	return crc, err
}

// DeleteTiny deletes a stiny extent.
func (e *Extent) punchDelete(offset, size int64) (hasDelete bool, err error) {
	log.LogDebugf("punchDelete extent %v offset %v, size %v", e, offset, size)
	if int(offset)%util.PageSize != 0 {
		return false, ParameterMismatchError
	}
	if int(size)%util.PageSize != 0 {
		size += int64(util.PageSize - int(size)%util.PageSize)
	}

	newOffset, err := e.file.Seek(offset, SEEK_DATA)
	if err != nil {
		if strings.Contains(err.Error(), syscall.ENXIO.Error()) {
			return true, nil
		}
		return false, err
	}
	if newOffset-offset >= size {
		hasDelete = true
		return true, nil
	}
	log.LogDebugf("punchDelete offset %v size %v", offset, size)
	err = fallocate(int(e.file.Fd()), util.FallocFLPunchHole|util.FallocFLKeepSize, offset, size)
	return
}

func (e *Extent) getRealBlockCnt() (blockNum int64) {
	stat := new(syscall.Stat_t)
	syscall.Stat(e.filePath, stat)
	return stat.Blocks
}

func (e *Extent) TinyExtentRecover(data []byte, offset, size int64, crc uint32, isEmptyPacket bool) (err error) {
	e.Lock()
	defer e.Unlock()
	if !IsTinyExtent(e.extentID) {
		return ParameterMismatchError
	}
	if offset%util.PageSize != 0 || offset != e.dataSize {
		return fmt.Errorf("error empty packet on (%v) offset(%v) size(%v)"+
			" isEmptyPacket(%v)  e.dataSize(%v)", e.file.Name(), offset, size, isEmptyPacket, e.dataSize)
	}
	log.LogDebugf("before file (%v) getRealBlockNo (%v) isEmptyPacket(%v)"+
		"offset(%v) size(%v) e.datasize(%v)", e.filePath, e.getRealBlockCnt(), isEmptyPacket, offset, size, e.dataSize)
	if isEmptyPacket {
		finfo, err := e.file.Stat()
		if err != nil {
			return err
		}
		if offset < finfo.Size() {
			return fmt.Errorf("error empty packet on (%v) offset(%v) size(%v)"+
				" isEmptyPacket(%v) filesize(%v) e.dataSize(%v)", e.file.Name(), offset, size, isEmptyPacket, finfo.Size(), e.dataSize)
		}
		if err = syscall.Ftruncate(int(e.file.Fd()), offset+size); err != nil {
			return err
		}
		err = fallocate(int(e.file.Fd()), util.FallocFLPunchHole|util.FallocFLKeepSize, offset, size)
	} else {
		_, err = e.file.WriteAt(data[:size], int64(offset))
	}
	if err != nil {
		return
	}
	watermark := offset + size
	if watermark%util.PageSize != 0 {
		watermark = watermark + (util.PageSize - watermark%util.PageSize)
	}
	e.dataSize = watermark
	log.LogDebugf("after file (%v) getRealBlockNo (%v) isEmptyPacket(%v)"+
		"offset(%v) size(%v) e.datasize(%v)", e.filePath, e.getRealBlockCnt(), isEmptyPacket, offset, size, e.dataSize)

	return
}

func (e *Extent) tinyExtentAvaliOffset(offset int64) (newOffset, newEnd int64, err error) {
	e.Lock()
	defer e.Unlock()
	newOffset, err = e.file.Seek(int64(offset), SEEK_DATA)
	if err != nil {
		return
	}
	newEnd, err = e.file.Seek(int64(newOffset), SEEK_HOLE)
	if err != nil {
		return
	}
	if newOffset-offset > util.BlockSize {
		newOffset = offset + util.BlockSize
	}
	if newEnd-newOffset > util.BlockSize {
		newEnd = newOffset + util.BlockSize
	}
	if newEnd < newOffset {
		err = fmt.Errorf("unavali TinyExtentAvaliOffset on SEEK_DATA or SEEK_HOLE   (%v) offset(%v) "+
			"newEnd(%v) newOffset(%v)", e.extentID, offset, newEnd, newOffset)
	}
	return
}

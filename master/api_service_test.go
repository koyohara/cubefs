// Copyright 2018 The CFS Authors.
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

package master

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cubefs/cubefs/master/mocktest"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/config"
	"github.com/cubefs/cubefs/util/log"
	"github.com/stretchr/testify/assert"
)

const (
	hostAddr          = "http://127.0.0.1:8080"
	ConfigKeyLogDir   = "logDir"
	ConfigKeyLogLevel = "logLevel"
	mds1Addr          = "127.0.0.1:9101"
	mds2Addr          = "127.0.0.1:9102"
	mds3Addr          = "127.0.0.1:9103"
	mds4Addr          = "127.0.0.1:9104"
	mds5Addr          = "127.0.0.1:9105"
	mds6Addr          = "127.0.0.1:9106"
	mds7Addr          = "127.0.0.1:9107"

	mms1Addr      = "127.0.0.1:8101"
	mms2Addr      = "127.0.0.1:8102"
	mms3Addr      = "127.0.0.1:8103"
	mms4Addr      = "127.0.0.1:8104"
	mms5Addr      = "127.0.0.1:8105"
	mms6Addr      = "127.0.0.1:8106"
	mms7Addr      = "127.0.0.1:8107"
	mms8Addr      = "127.0.0.1:8108"
	commonVolName = "commonVol"
	testZone1     = "zone1"
	testZone2     = "zone2"
	testZone3     = "zone3"

	testUserID  = "testUser"
	ak          = "0123456789123456"
	sk          = "01234567891234560123456789123456"
	description = "testUser"
)

var server = createDefaultMasterServerForTest()
var commonVol *Vol
var cfsUser *proto.UserInfo

func createDefaultMasterServerForTest() *Server {

	cfgJSON := `{
		"role": "master",
		"ip": "127.0.0.1",
		"listen": "8080",
		"prof":"10088",
		"id":"1",
		"peers": "1:127.0.0.1:8080",
		"retainLogs":"20000",
		"tickInterval":500,
		"electionTick":6,
		"logDir": "/tmp/cubefs/Logs",
		"logLevel":"DEBUG",
		"walDir":"/tmp/cubefs/raft",
		"storeDir":"/tmp/cubefs/rocksdbstore",
		"clusterName":"cubefs"
	}`

	testServer, err := createMasterServer(cfgJSON)
	if err != nil {
		panic(err)
	}
	//add data node
	addDataServer(mds1Addr, testZone1)
	addDataServer(mds2Addr, testZone1)
	addDataServer(mds3Addr, testZone2)
	addDataServer(mds4Addr, testZone2)
	addDataServer(mds5Addr, testZone2)
	addDataServer(mds6Addr, testZone2)

	// add meta node
	addMetaServer(mms1Addr, testZone1)
	addMetaServer(mms2Addr, testZone1)
	addMetaServer(mms3Addr, testZone2)
	addMetaServer(mms4Addr, testZone2)
	addMetaServer(mms5Addr, testZone2)
	addMetaServer(mms6Addr, testZone2)
	// we should wait 5 seoncds for master to prepare state
	time.Sleep(5 * time.Second)
	testServer.cluster.checkDataNodeHeartbeat()
	testServer.cluster.checkMetaNodeHeartbeat()
	time.Sleep(5 * time.Second)
	testServer.cluster.scheduleToUpdateStatInfo()
	// set load factor
	err = testServer.cluster.setClusterLoadFactor(100)
	if err != nil {
		panic("set load factor fail" + err.Error())
	}

	req := &createVolReq{
		name:             commonVolName,
		owner:            "cfs",
		size:             3,
		mpCount:          3,
		dpReplicaNum:     3,
		capacity:         300,
		followerRead:     false,
		authenticate:     false,
		crossZone:        false,
		normalZonesFirst: false,
		zoneName:         testZone2,
		description:      "",
		qosLimitArgs:     &qosArgs{},
	}

	vol, err := testServer.cluster.createVol(req)
	if err != nil {
		log.LogFlush()
		panic(err)
	}

	vol, err = testServer.cluster.getVol(commonVolName)
	if err != nil {
		panic(err)
	}

	commonVol = vol
	fmt.Printf("vol[%v] has created\n", newSimpleView(commonVol))

	if err = createUserWithPolicy(testServer); err != nil {
		panic(err)
	}

	return testServer
}

func createUserWithPolicy(testServer *Server) (err error) {
	param := &proto.UserCreateParam{ID: "cfs", Type: proto.UserTypeNormal}
	if cfsUser, err = testServer.user.createKey(param); err != nil {
		return
	}
	fmt.Printf("user[%v] has created\n", cfsUser.UserID)
	paramTransfer := &proto.UserTransferVolParam{Volume: commonVolName, UserSrc: "cfs", UserDst: "cfs", Force: false}
	if cfsUser, err = testServer.user.transferVol(paramTransfer); err != nil {
		return
	}
	return nil
}

func createMasterServer(cfgJSON string) (server *Server, err error) {
	cfg := config.LoadConfigString(cfgJSON)
	server = NewServer()
	useConnPool = false
	logDir := cfg.GetString(ConfigKeyLogDir)
	walDir := cfg.GetString(WalDir)
	storeDir := cfg.GetString(StoreDir)
	profPort := cfg.GetString("prof")
	os.RemoveAll(logDir)
	os.RemoveAll(walDir)
	os.RemoveAll(storeDir)
	os.Mkdir(walDir, 0755)
	os.Mkdir(storeDir, 0755)
	logLevel := cfg.GetString(ConfigKeyLogLevel)
	var level log.Level
	switch strings.ToLower(logLevel) {
	case "debug":
		level = log.DebugLevel
	case "info":
		level = log.InfoLevel
	case "warn":
		level = log.WarnLevel
	case "error":
		level = log.ErrorLevel
	default:
		level = log.ErrorLevel
	}
	if _, err = log.InitLog(logDir, "master", level, nil, log.DefaultLogLeftSpaceLimit); err != nil {
		fmt.Println("Fatal: failed to start the cubefs daemon - ", err)
		return
	}
	if profPort != "" {
		go func() {
			err := http.ListenAndServe(fmt.Sprintf(":%v", profPort), nil)
			if err != nil {
				panic(fmt.Sprintf("cannot listen pprof %v err %v", profPort, err.Error()))
			}
		}()
	}
	if err = server.Start(cfg); err != nil {
		return
	}
	time.Sleep(5 * time.Second)
	fmt.Println(server.config.peerAddrs, server.leaderInfo.addr)
	return server, nil
}

func addDataServer(addr, zoneName string) {
	mds := mocktest.NewMockDataServer(addr, zoneName)
	mds.Start()
}

func addMetaServer(addr, zoneName string) {
	mms := mocktest.NewMockMetaServer(addr, zoneName)
	mms.Start()
}

func TestSetMetaNodeThreshold(t *testing.T) {
	threshold := 0.5
	reqURL := fmt.Sprintf("%v%v?threshold=%v", hostAddr, proto.AdminSetMetaNodeThreshold, threshold)
	fmt.Println(reqURL)
	process(reqURL, t)
	if server.cluster.cfg.MetaNodeThreshold != float32(threshold) {
		t.Errorf("set metanode threshold to %v failed", threshold)
		return
	}
}

func TestSetDisableAutoAlloc(t *testing.T) {
	req := map[string]interface{}{"enable": true}
	processWithFatalV2(proto.AdminClusterFreeze, true, req, t)

	if !server.cluster.DisableAutoAllocate {
		t.Errorf("set disableAutoAlloc to %v failed", true)
		return
	}

	req = map[string]interface{}{"enable": false}
	processWithFatalV2(proto.AdminClusterFreeze, true, req, t)
	if server.cluster.DisableAutoAllocate {
		t.Errorf("set disableAutoAlloc to %v failed", false)
		return
	}
}

func TestGetCluster(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.AdminGetCluster)
	fmt.Println(reqURL)
	process(reqURL, t)
}

func TestGetIpAndClusterName(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.AdminGetIP)
	fmt.Println(reqURL)
	process(reqURL, t)
}

func fatal(t *testing.T, str string) {
	log.LogFlush()
	t.Fatal(str)
}

type httpReply struct {
	Code int32           `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func processWithFatalV2(url string, success bool, req map[string]interface{}, t *testing.T) (reply *httpReply) {
	reqURL := buildUrl(hostAddr, url, req)

	resp, err := http.Get(reqURL)
	assert.Nil(t, err)
	assert.True(t, resp.StatusCode == http.StatusOK)

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	assert.Nil(t, err)

	t.Log(string(body), reqURL)

	reply = &httpReply{}
	err = json.Unmarshal(body, reply)
	assert.Nil(t, err)

	if success {
		assert.True(t, reply.Code == proto.ErrCodeSuccess)
		return reply
	}

	assert.True(t, reply.Code != proto.ErrCodeSuccess)

	return
}

func process(reqURL string, t *testing.T) (reply *proto.HTTPReply) {
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Errorf("err is %v", err)
		return
	}
	fmt.Println(resp.StatusCode)
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("err is %v", err)
		return
	}
	fmt.Println(string(body))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code[%v]", resp.StatusCode)
		return
	}
	reply = &proto.HTTPReply{}
	if err = json.Unmarshal(body, reply); err != nil {
		t.Error(err)
		return
	}
	if reply.Code != 0 {
		t.Errorf("failed,msg[%v],data[%v]", reply.Msg, reply.Data)
		return
	}
	return
}

func TestDisk(t *testing.T) {
	addr := mds5Addr
	disk := "/cfs"
	decommissionDisk(addr, disk, t)
}

func decommissionDisk(addr, path string, t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?addr=%v&disk=%v",
		hostAddr, proto.DecommissionDisk, addr, path)
	fmt.Println(reqURL)
	resp, err := http.Get(reqURL)
	if err != nil {
		t.Errorf("err is %v", err)
		return
	}
	fmt.Println(resp.StatusCode)
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("err is %v", err)
		return
	}
	fmt.Println(string(body))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code[%v]", resp.StatusCode)
		return
	}
	reply := &proto.HTTPReply{}
	if err = json.Unmarshal(body, reply); err != nil {
		t.Error(err)
		return
	}
	server.cluster.checkDataNodeHeartbeat()
	time.Sleep(5 * time.Second)
	server.cluster.checkDiskRecoveryProgress()
}

func TestMarkDeleteVol(t *testing.T) {
	name := "delVol"
	createVol(map[string]interface{}{nameKey: name}, t)

	reqURL := fmt.Sprintf("%v%v?name=%v&authKey=%v", hostAddr, proto.AdminDeleteVol, name, buildAuthKey(testOwner))
	process(reqURL, t)

	userInfo, err := server.user.getUserInfo("cfs")
	if err != nil {
		t.Error(err)
		return
	}

	if contains(userInfo.Policy.OwnVols, name) {
		t.Errorf("expect no vol %v in own vols, but is exist", name)
		return
	}
}

func TestSetVolCapacity(t *testing.T) {
	setVolCapacity(600, proto.AdminVolExpand, t)
	setVolCapacity(300, proto.AdminVolShrink, t)
}

func TestPreloadDp(t *testing.T) {
	volName := "preloadVol"
	req := map[string]interface{}{}
	req[nameKey] = volName
	req[volTypeKey] = proto.VolumeTypeCold
	createVol(req, t)

	preCap := 60
	checkParam(cacheCapacity, proto.AdminCreatePreLoadDataPartition, req, -1, preCap, t)
	delVol(volName, t)
}

func TestUpdateVol(t *testing.T) {
	volName := "updateVol"
	req := map[string]interface{}{}
	req[nameKey] = volName
	req[volTypeKey] = proto.VolumeTypeCold

	createVol(req, t)

	view := getSimpleVol(volName, true, t)

	// name can't be empty
	checkParam(nameKey, proto.AdminUpdateVol, req, "", volName, t)
	// vol name not exist
	checkParam(nameKey, proto.AdminUpdateVol, req, "tt", volName, t)
	// auth key can't be empty
	checkParam(volAuthKey, proto.AdminUpdateVol, req, "", buildAuthKey(testOwner), t)

	view2 := getSimpleVol(volName, true, t)
	// if not set use default
	assert.True(t, view.Capacity == view2.Capacity)
	assert.True(t, view.Description == view2.Description)
	assert.True(t, view.ZoneName == view2.ZoneName)
	assert.True(t, view.Authenticate == view2.Authenticate)
	assert.True(t, view.FollowerRead == view2.FollowerRead)
	assert.True(t, view.ObjBlockSize == view2.ObjBlockSize)
	assert.True(t, view.CacheCapacity == view2.CacheCapacity)
	assert.True(t, view.CacheAction == view2.CacheAction)
	assert.True(t, view.CacheThreshold == view2.CacheThreshold)
	assert.True(t, view.CacheTtl == view2.CacheTtl)
	assert.True(t, view.CacheHighWater == view2.CacheHighWater)
	assert.True(t, view.CacheLowWater == view2.CacheLowWater)
	assert.True(t, view.CacheLruInterval == view2.CacheLruInterval)
	assert.True(t, view.CacheRule == view2.CacheRule)

	// update
	cap := 1024
	desc := "hello_test"
	blkSize := 6 * 1024
	cacheCap := 102
	threshold := 7 * 1024
	ttl := 7
	high := 70
	low := 40
	lru := 6
	rule := "test"

	checkParam(volCapacityKey, proto.AdminUpdateVol, req, "tt", cap, t)
	setParam(descriptionKey, proto.AdminUpdateVol, req, desc, t)
	checkParam(zoneNameKey, proto.AdminUpdateVol, req, "default", testZone1, t)
	checkParam(authenticateKey, proto.AdminUpdateVol, req, "tt", true, t)
	checkParam(followerReadKey, proto.AdminUpdateVol, req, "test", true, t)
	checkParam(ebsBlkSizeKey, proto.AdminUpdateVol, req, "-1", blkSize, t)
	checkParam(cacheActionKey, proto.AdminUpdateVol, req, "3", proto.RWCache, t)
	checkParam(cacheCapacity, proto.AdminUpdateVol, req, "1027", cacheCap, t)
	checkParam(cacheCapacity, proto.AdminUpdateVol, req, "-1", cacheCap, t)
	checkParam(volCapacityKey, proto.AdminUpdateVol, req, "101", cap, t)
	checkParam(cacheThresholdKey, proto.AdminUpdateVol, req, "-1", threshold, t)
	checkParam(cacheTTLKey, proto.AdminUpdateVol, req, "tt", ttl, t)
	checkParam(cacheHighWaterKey, proto.AdminUpdateVol, req, "high", high, t)
	checkParam(cacheHighWaterKey, proto.AdminUpdateVol, req, 91, high, t)
	checkParam(cacheLowWaterKey, proto.AdminUpdateVol, req, 78, low, t)
	checkParam(cacheLowWaterKey, proto.AdminUpdateVol, req, 93, low, t)
	checkParam(cacheLRUIntervalKey, proto.AdminUpdateVol, req, -1, lru, t)
	setParam(cacheRuleKey, proto.AdminUpdateVol, req, rule, t)

	view = getSimpleVol(volName, true, t)
	// check update result
	assert.True(t, int(view.Capacity) == cap)
	assert.True(t, view.Description == desc)
	assert.True(t, view.ZoneName == testZone1)
	assert.True(t, view.Authenticate)
	// LF vol always be true
	assert.True(t, view.FollowerRead)
	assert.True(t, view.ObjBlockSize == blkSize)
	assert.True(t, view.CacheCapacity == uint64(cacheCap))
	assert.True(t, view.CacheAction == proto.RWCache)
	assert.True(t, view.CacheThreshold == threshold)
	assert.True(t, view.CacheTtl == ttl)
	assert.True(t, view.CacheHighWater == high)
	assert.True(t, view.CacheLowWater == low)
	assert.True(t, view.CacheLruInterval == lru)
	assert.True(t, view.CacheRule == rule)

	// update cacheRule to empty
	setUpdateVolParm(emptyCacheRuleKey, req, true, t)
	view = getSimpleVol(volName, true, t)
	assert.True(t, view.CacheRule == "")

	delVol(volName, t)
	// can't update vol after delete
	checkParam(cacheLRUIntervalKey, proto.AdminUpdateVol, req, lru, lru, t)
}

func setUpdateVolParm(key string, req map[string]interface{}, val interface{}, t *testing.T) {
	setParam(key, proto.AdminUpdateVol, req, val, t)
}

func checkUpdateVolParm(key string, req map[string]interface{}, wrong, correct interface{}, t *testing.T) {
	checkParam(key, proto.AdminUpdateVol, req, wrong, correct, t)
}

func delVol(name string, t *testing.T) {
	req := map[string]interface{}{
		nameKey:    name,
		volAuthKey: buildAuthKey(testOwner),
	}

	processWithFatalV2(proto.AdminDeleteVol, true, req, t)

	vol, err := server.cluster.getVol(name)
	assert.True(t, err == nil)

	assert.True(t, vol.Status == markDelete)
}

func setVolCapacity(capacity uint64, url string, t *testing.T) {
	req := map[string]interface{}{
		"name":     commonVol.Name,
		"capacity": capacity,
		"authKey":  buildAuthKey(testOwner),
	}

	processWithFatalV2(url, true, req, t)

	vol, err := server.cluster.getVol(commonVolName)
	if err != nil {
		t.Error(err)
		return
	}

	if vol.Capacity != capacity {
		t.Errorf("expect capacity is %v, but is %v", capacity, vol.Capacity)
		return
	}

	fmt.Printf("update capacity to %d success\n", capacity)
}

func buildAuthKey(owner string) string {
	h := md5.New()
	h.Write([]byte(owner))
	cipherStr := h.Sum(nil)
	return hex.EncodeToString(cipherStr)
}

func TestGetVolSimpleInfo(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?name=%v", hostAddr, proto.AdminGetVol, commonVol.Name)
	process(reqURL, t)
}

func TestCreateVol(t *testing.T) {
	name := "test_create_vol"
	reqURL := fmt.Sprintf("%v%v?name=%v&replicas=3&type=extent&capacity=100&owner=cfstest&zoneName=%v", hostAddr, proto.AdminCreateVol, name, testZone2)
	fmt.Println(reqURL)
	process(reqURL, t)
	userInfo, err := server.user.getUserInfo("cfstest")
	if err != nil {
		t.Error(err)
		return
	}
	if !contains(userInfo.Policy.OwnVols, name) {
		t.Errorf("expect vol %v in own vols, but is not", name)
		return
	}
}

func TestCreateMetaPartition(t *testing.T) {
	server.cluster.checkMetaNodeHeartbeat()
	time.Sleep(5 * time.Second)
	commonVol.checkMetaPartitions(server.cluster)
	createMetaPartition(commonVol, t)
}

func TestCreateDataPartition(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?count=2&name=%v&type=extent",
		hostAddr, proto.AdminCreateDataPartition, commonVol.Name)
	process(reqURL, t)
}

func TestGetDataPartition(t *testing.T) {
	if len(commonVol.dataPartitions.partitions) == 0 {
		t.Errorf("no data partitions")
		return
	}
	partition := commonVol.dataPartitions.partitions[0]
	reqURL := fmt.Sprintf("%v%v?id=%v", hostAddr, proto.AdminGetDataPartition, partition.PartitionID)
	process(reqURL, t)

	reqURL = fmt.Sprintf("%v%v?id=%v&name=%v", hostAddr, proto.AdminGetDataPartition, partition.PartitionID, partition.VolName)
	process(reqURL, t)
}

func TestLoadDataPartition(t *testing.T) {
	if len(commonVol.dataPartitions.partitions) == 0 {
		t.Errorf("no data partitions")
		return
	}
	partition := commonVol.dataPartitions.partitions[0]
	reqURL := fmt.Sprintf("%v%v?id=%v&name=%v",
		hostAddr, proto.AdminLoadDataPartition, partition.PartitionID, commonVol.Name)
	process(reqURL, t)
}

func TestDataPartitionDecommission(t *testing.T) {
	if len(commonVol.dataPartitions.partitions) == 0 {
		t.Errorf("no data partitions")
		return
	}
	server.cluster.checkDataNodeHeartbeat()
	time.Sleep(5 * time.Second)
	partition := commonVol.dataPartitions.partitions[0]
	offlineAddr := partition.Hosts[0]
	reqURL := fmt.Sprintf("%v%v?name=%v&id=%v&addr=%v",
		hostAddr, proto.AdminDecommissionDataPartition, commonVol.Name, partition.PartitionID, offlineAddr)
	process(reqURL, t)
	if contains(partition.Hosts, offlineAddr) {
		t.Errorf("offlineAddr[%v],hosts[%v]", offlineAddr, partition.Hosts)
		return
	}
	partition.isRecover = false
}

//	func TestGetAllVols(t *testing.T) {
//		reqURL := fmt.Sprintf("%v%v", hostAddr, proto.GetALLVols)
//		process(reqURL, t)
//	}
func TestGetMetaPartitions(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?name=%v", hostAddr, proto.ClientMetaPartitions, commonVolName)
	process(reqURL, t)
}

func TestGetDataPartitions(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?name=%v", hostAddr, proto.ClientDataPartitions, commonVolName)
	process(reqURL, t)
}

func TestGetTopo(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.GetTopologyView)
	process(reqURL, t)
}

func TestGetMetaNode(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?addr=%v", hostAddr, proto.GetMetaNode, mms1Addr)
	process(reqURL, t)
}

func TestGetNodeInfo(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.AdminGetNodeInfo)
	process(reqURL, t)
}

func TestSetNodeMaxDpCntLimit(t *testing.T) {
	// change current max data partition count limit to 4000
	limit := uint32(4000)
	reqURL := fmt.Sprintf("%v%v?maxDpCntLimit=%v", hostAddr, proto.AdminSetNodeInfo, limit)
	process(reqURL, t)
	// query current settings
	reqURL = fmt.Sprintf("%v%v", hostAddr, proto.AdminGetNodeInfo)
	reply := process(reqURL, t)
	data := reply.Data.(map[string]interface{})
	limitStr := (data[maxDpCntLimitKey]).(string)
	assert.True(t, fmt.Sprint(limit) == limitStr)
	// query data node info
	reqURL = fmt.Sprintf("%v%v?addr=%v", hostAddr, proto.GetDataNode, mds1Addr)
	reply = process(reqURL, t)
	data = reply.Data.(map[string]interface{})
	dataNodeLimit := uint32((data[maxDpCntLimitKey]).(float64))
	assert.True(t, dataNodeLimit == limit)
}

func TestAddDataReplica(t *testing.T) {
	partition := commonVol.dataPartitions.partitions[0]
	dsAddr := mds7Addr
	addDataServer(dsAddr, "zone2")
	reqURL := fmt.Sprintf("%v%v?id=%v&addr=%v", hostAddr, proto.AdminAddDataReplica, partition.PartitionID, dsAddr)
	process(reqURL, t)
	partition.RLock()
	if !contains(partition.Hosts, dsAddr) {
		t.Errorf("hosts[%v] should contains dsAddr[%v]", partition.Hosts, dsAddr)
		partition.RUnlock()
		return
	}
	partition.RUnlock()
	server.cluster.BadDataPartitionIds.Range(
		func(key, value interface{}) bool {
			addr, ok := key.(string)
			if !ok {
				return true
			}
			if strings.HasPrefix(addr, dsAddr) {
				server.cluster.BadDataPartitionIds.Delete(key)
			}
			return true
		})
}

func TestRemoveDataReplica(t *testing.T) {
	partition := commonVol.dataPartitions.partitions[0]
	partition.isRecover = false
	dsAddr := mds7Addr
	reqURL := fmt.Sprintf("%v%v?id=%v&addr=%v", hostAddr, proto.AdminDeleteDataReplica, partition.PartitionID, dsAddr)
	process(reqURL, t)
	partition.RLock()
	if contains(partition.Hosts, dsAddr) {
		t.Errorf("hosts[%v] should not contains dsAddr[%v]", partition.Hosts, dsAddr)
		partition.RUnlock()
		return
	}
	partition.isRecover = false
	partition.RUnlock()
}

func TestAddMetaReplica(t *testing.T) {
	maxPartitionID := commonVol.maxPartitionID()
	partition := commonVol.MetaPartitions[maxPartitionID]
	if partition == nil {
		t.Error("no meta partition")
		return
	}
	addMetaServer(mms8Addr, testZone3)
	server.cluster.checkMetaNodeHeartbeat()
	time.Sleep(2 * time.Second)
	reqURL := fmt.Sprintf("%v%v?id=%v&addr=%v", hostAddr, proto.AdminAddMetaReplica, partition.PartitionID, mms8Addr)
	process(reqURL, t)
	partition.RLock()
	if !contains(partition.Hosts, mms8Addr) {
		t.Errorf("hosts[%v] should contains dsAddr[%v]", partition.Hosts, mms8Addr)
		partition.RUnlock()
		return
	}
	partition.RUnlock()
}

func TestRemoveMetaReplica(t *testing.T) {
	maxPartitionID := commonVol.maxPartitionID()
	partition := commonVol.MetaPartitions[maxPartitionID]
	if partition == nil {
		t.Error("no meta partition")
		return
	}
	partition.IsRecover = false
	msAddr := mms8Addr
	reqURL := fmt.Sprintf("%v%v?id=%v&addr=%v", hostAddr, proto.AdminDeleteMetaReplica, partition.PartitionID, msAddr)
	process(reqURL, t)
	partition.RLock()
	if contains(partition.Hosts, msAddr) {
		t.Errorf("hosts[%v] should contains dsAddr[%v]", partition.Hosts, msAddr)
		partition.RUnlock()
		return
	}
	partition.RUnlock()
}

func TestClusterStat(t *testing.T) {
	reqUrl := fmt.Sprintf("%v%v", hostAddr, proto.AdminClusterStat)
	fmt.Println(reqUrl)
	process(reqUrl, t)
}

func TestListVols(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?keywords=%v", hostAddr, proto.AdminListVols, commonVolName)
	fmt.Println(reqURL)
	process(reqURL, t)
}

func TestUpdateNodesetNodeSelector(t *testing.T) {
	zone, err := server.cluster.t.getZone(testZone2)
	if err != nil {
		t.Errorf("failed to get zone, %v", err)
		return
	}
	nsc := zone.getAllNodeSet()
	if nsc.Len() == 0 {
		t.Error("nodeset count could not be 0")
		return
	}
	ns := nsc[0]
	reqUrl := fmt.Sprintf("%v%v?zoneName=%v&id=%v", hostAddr, proto.AdminUpdateNodeSetNodeSelector, testZone2, ns.ID)
	updateDataSelectorUrl := fmt.Sprintf("%v&dataNodeSelector=%v", reqUrl, RoundRobinNodeSelectorName)
	updateMetaSelectorUrl := fmt.Sprintf("%v&metaNodeSelector=%v", reqUrl, RoundRobinNodeSelectorName)
	process(updateDataSelectorUrl, t)
	if ns.GetDataNodeSelector() != RoundRobinNodeSelectorName {
		t.Errorf("failed to change data node selector")
		return
	}
	process(updateMetaSelectorUrl, t)
	if ns.GetMetaNodeSelector() != RoundRobinNodeSelectorName {
		t.Errorf("failed to change meta node selector")
		return
	}
	updateDataSelectorUrl = fmt.Sprintf("%v&dataNodeSelector=%v", reqUrl, CarryWeightNodeSelectorName)
	updateMetaSelectorUrl = fmt.Sprintf("%v&metaNodeSelector=%v", reqUrl, CarryWeightNodeSelectorName)
	process(updateDataSelectorUrl, t)
	process(updateMetaSelectorUrl, t)
}

func TestUpdateZoneNodesetSelector(t *testing.T) {
	zone, err := server.cluster.t.getZone(testZone2)
	if err != nil {
		t.Errorf("failed to get zone, %v", err)
		return
	}
	nsc := zone.getAllNodeSet()
	if nsc.Len() == 0 {
		t.Error("nodeset count could not be 0")
		return
	}
	reqUrl := fmt.Sprintf("%v%v?name=%v&enable=1", hostAddr, proto.UpdateZone, testZone2)
	updateDataSelectorUrl := fmt.Sprintf("%v&dataNodesetSelector=%v", reqUrl, CarryWeightNodesetSelectorName)
	updateMetaSelectorUrl := fmt.Sprintf("%v&metaNodesetSelector=%v", reqUrl, CarryWeightNodesetSelectorName)
	process(updateDataSelectorUrl, t)
	if zone.GetDataNodesetSelector() != CarryWeightNodesetSelectorName {
		t.Errorf("failed to change data nodeset selector")
	}
	process(updateMetaSelectorUrl, t)
	if zone.GetMetaNodesetSelector() != CarryWeightNodesetSelectorName {
		t.Errorf("failed to change meta nodeset selector")
	}
	updateDataSelectorUrl = fmt.Sprintf("%v&dataNodesetSelector=%v", reqUrl, RoundRobinNodesetSelectorName)
	updateMetaSelectorUrl = fmt.Sprintf("%v&metaNodesetSelector=%v", reqUrl, RoundRobinNodesetSelectorName)
	process(updateDataSelectorUrl, t)
	process(updateMetaSelectorUrl, t)
}

func TestUpdateZoneNodeSelector(t *testing.T) {
	zone, err := server.cluster.t.getZone(testZone2)
	if err != nil {
		t.Errorf("failed to get zone, %v", err)
		return
	}
	nsc := zone.getAllNodeSet()
	if nsc.Len() == 0 {
		t.Error("nodeset count could not be 0")
		return
	}
	reqUrl := fmt.Sprintf("%v%v?name=%v&enable=1", hostAddr, proto.UpdateZone, testZone2)
	updateDataSelectorUrl := fmt.Sprintf("%v&dataNodeSelector=%v", reqUrl, TicketNodeSelectorName)
	updateMetaSelectorUrl := fmt.Sprintf("%v&metaNodeSelector=%v", reqUrl, TicketNodeSelectorName)
	process(updateDataSelectorUrl, t)
	failed := false
	func() {
		zone.nsLock.RLock()
		defer zone.nsLock.RUnlock()
		for _, ns := range zone.nodeSetMap {
			if ns.GetDataNodeSelector() != TicketNodeSelectorName {
				t.Errorf("failed to change data nodeset selector")
				failed = true
			}
		}
	}()
	if failed {
		return
	}
	process(updateMetaSelectorUrl, t)
	func() {
		zone.nsLock.RLock()
		defer zone.nsLock.RUnlock()
		for _, ns := range zone.nodeSetMap {
			if ns.GetMetaNodeSelector() != TicketNodeSelectorName {
				t.Errorf("failed to change data nodeset selector")
				failed = true
			}
		}
	}()
	if failed {
		return
	}
	updateDataSelectorUrl = fmt.Sprintf("%v&dataNodeSelector=%v", reqUrl, CarryWeightNodeSelectorName)
	updateMetaSelectorUrl = fmt.Sprintf("%v&metaNodeSelector=%v", reqUrl, CarryWeightNodeSelectorName)
	process(updateDataSelectorUrl, t)
	process(updateMetaSelectorUrl, t)
}

func TestUpdateZoneStatus(t *testing.T) {
	zone, err := server.cluster.t.getZone(testZone2)
	if err != nil {
		t.Errorf("failed to get zone, %v", err)
		return
	}
	nsc := zone.getAllNodeSet()
	if nsc.Len() == 0 {
		t.Error("nodeset count could not be 0")
		return
	}
	reqUrl := fmt.Sprintf("%v%v?name=%v", hostAddr, proto.UpdateZone, testZone2)
	enableUrl := fmt.Sprintf("%v&enable=1", reqUrl)
	disableUrl := fmt.Sprintf("%v&enable=0", reqUrl)
	process(disableUrl, t)
	if zone.getStatus() != unavailableZone {
		t.Errorf("failed to update zone status")
		return
	}
	process(enableUrl, t)
}

func post(reqURL string, data []byte, t *testing.T) (reply *proto.HTTPReply) {
	reader := bytes.NewReader(data)
	req, err := http.NewRequest(http.MethodPost, reqURL, reader)
	if err != nil {
		t.Errorf("generate request err: %v", err)
		return
	}
	var resp *http.Response
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Errorf("post err: %v", err)
		return
	}
	fmt.Println(resp.StatusCode)
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Errorf("err is %v", err)
		return
	}
	fmt.Println(string(body))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code[%v]", resp.StatusCode)
		return
	}
	reply = &proto.HTTPReply{}
	if err = json.Unmarshal(body, reply); err != nil {
		t.Error(err)
		return
	}
	if reply.Code != 0 {
		t.Errorf("failed,msg[%v],data[%v]", reply.Msg, reply.Data)
		return
	}
	return
}

func TestCreateUser(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.UserCreate)
	param := &proto.UserCreateParam{ID: testUserID, Type: proto.UserTypeNormal}
	data, err := json.Marshal(param)
	if err != nil {
		t.Error(err)
		return
	}
	fmt.Println(reqURL)
	post(reqURL, data, t)
}

func TestGetUser(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?user=%v", hostAddr, proto.UserGetInfo, testUserID)
	fmt.Println(reqURL)
	process(reqURL, t)
}

func TestUpdateUser(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.UserUpdate)
	param := &proto.UserUpdateParam{UserID: testUserID, AccessKey: ak, SecretKey: sk, Type: proto.UserTypeAdmin, Description: description}
	data, err := json.Marshal(param)
	if err != nil {
		t.Error(err)
		return
	}
	fmt.Println(reqURL)
	post(reqURL, data, t)
	userInfo, err := server.user.getUserInfo(testUserID)
	if err != nil {
		t.Error(err)
		return
	}
	if userInfo.AccessKey != ak {
		t.Errorf("expect ak[%v], real ak[%v]\n", ak, userInfo.AccessKey)
		return
	}
	if userInfo.SecretKey != sk {
		t.Errorf("expect sk[%v], real sk[%v]\n", sk, userInfo.SecretKey)
		return
	}
	if userInfo.UserType != proto.UserTypeAdmin {
		t.Errorf("expect ak[%v], real ak[%v]\n", proto.UserTypeAdmin, userInfo.UserType)
		return
	}
	if userInfo.Description != description {
		t.Errorf("expect description[%v], real description[%v]\n", description, userInfo.Description)
		return
	}
}

func TestGetAKInfo(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?ak=%v", hostAddr, proto.UserGetAKInfo, ak)
	fmt.Println(reqURL)
	process(reqURL, t)
}

func TestUpdatePolicy(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.UserUpdatePolicy)
	param := &proto.UserPermUpdateParam{UserID: testUserID, Volume: commonVolName, Policy: []string{proto.BuiltinPermissionWritable.String()}}
	data, err := json.Marshal(param)
	if err != nil {
		t.Error(err)
		return
	}
	fmt.Println(reqURL)
	post(reqURL, data, t)
	userInfo, err := server.user.getUserInfo(testUserID)
	if err != nil {
		t.Error(err)
		return
	}
	if _, exist := userInfo.Policy.AuthorizedVols[commonVolName]; !exist {
		t.Errorf("expect vol %v in authorized vols, but is not", commonVolName)
		return
	}
}

func TestRemovePolicy(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.UserRemovePolicy)
	param := &proto.UserPermRemoveParam{UserID: testUserID, Volume: commonVolName}
	data, err := json.Marshal(param)
	if err != nil {
		t.Error(err)
		return
	}
	fmt.Println(reqURL)
	post(reqURL, data, t)
	userInfo, err := server.user.getUserInfo(testUserID)
	if err != nil {
		t.Error(err)
		return
	}
	if _, exist := userInfo.Policy.AuthorizedVols[commonVolName]; exist {
		t.Errorf("expect no vol %v in authorized vols, but is exist", commonVolName)
		return
	}
}

func TestTransferVol(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.UserTransferVol)
	param := &proto.UserTransferVolParam{Volume: commonVolName, UserSrc: "cfs", UserDst: testUserID, Force: false}
	data, err := json.Marshal(param)
	if err != nil {
		t.Error(err)
		return
	}
	fmt.Println(reqURL)
	post(reqURL, data, t)
	userInfo1, err := server.user.getUserInfo(testUserID)
	if err != nil {
		t.Error(err)
		return
	}
	if !contains(userInfo1.Policy.OwnVols, commonVolName) {
		t.Errorf("expect vol %v in own vols, but is not", commonVolName)
		return
	}
	userInfo2, err := server.user.getUserInfo("cfs")
	if err != nil {
		t.Error(err)
		return
	}
	if contains(userInfo2.Policy.OwnVols, commonVolName) {
		t.Errorf("expect no vol %v in own vols, but is exist", commonVolName)
		return
	}
	vol, err := server.cluster.getVol(commonVolName)
	if err != nil {
		t.Error(err)
		return
	}
	if vol.Owner != testUserID {
		t.Errorf("expect owner is %v, but is %v", testUserID, vol.Owner)
		return
	}
}

func TestDeleteVolPolicy(t *testing.T) {
	param := &proto.UserPermUpdateParam{UserID: "cfs", Volume: commonVolName, Policy: []string{proto.BuiltinPermissionWritable.String()}}
	if _, err := server.user.updatePolicy(param); err != nil {
		t.Error(err)
		return
	}
	reqURL := fmt.Sprintf("%v%v?name=%v", hostAddr, proto.UserDeleteVolPolicy, commonVolName)
	fmt.Println(reqURL)
	process(reqURL, t)
	userInfo1, err := server.user.getUserInfo(testUserID)
	if err != nil {
		t.Error(err)
		return
	}
	if contains(userInfo1.Policy.OwnVols, commonVolName) {
		t.Errorf("expect no vol %v in own vols, but is not", commonVolName)
		return
	}
	userInfo2, err := server.user.getUserInfo("cfs")
	if err != nil {
		t.Error(err)
		return
	}
	if _, exist := userInfo2.Policy.AuthorizedVols[commonVolName]; exist {
		t.Errorf("expect no vols %v in authorized vol is 0, but is exist", commonVolName)
		return
	}
}

func TestListUser(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?keywords=%v", hostAddr, proto.UserList, "test")
	fmt.Println(reqURL)
	process(reqURL, t)
}

func TestDeleteUser(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?user=%v", hostAddr, proto.UserDelete, testUserID)
	fmt.Println(reqURL)
	process(reqURL, t)
	if _, err := server.user.getUserInfo(testUserID); err != proto.ErrUserNotExists {
		t.Errorf("expect err ErrUserNotExists, but err is %v", err)
		return
	}
}

func TestListUsersOfVol(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?name=%v", hostAddr, proto.UsersOfVol, "test_create_vol")
	fmt.Println(reqURL)
	process(reqURL, t)
}

func TestListNodeSets(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v", hostAddr, proto.GetAllNodeSets)
	fmt.Println(reqURL)
	process(reqURL, t)

	reqURL = fmt.Sprintf("%v%v?zoneName=%v", hostAddr, proto.GetAllNodeSets, testZone2)
	fmt.Println(reqURL)
	process(reqURL, t)
}

func TestGetNodeSets(t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?nodesetId=1", hostAddr, proto.GetNodeSet)
	fmt.Println(reqURL)
	process(reqURL, t)
}

func TestUpdateNodeSet(t *testing.T) {
	zone, err := server.cluster.t.getZone(testZone2)
	if err != nil {
		t.Errorf("failed to get zone, %v", err)
		return
	}
	nsc := zone.getAllNodeSet()
	if nsc.Len() == 0 {
		t.Error("nodeset count could not be 0")
		return
	}
	ns := nsc[0]
	reqUrl := fmt.Sprintf("%v%v?nodesetId=%v", hostAddr, proto.UpdateNodeSet, ns.ID)
	updateDataSelectorUrl := fmt.Sprintf("%v&dataNodeSelector=%v", reqUrl, TicketNodeSelectorName)
	updateMetaSelectorUrl := fmt.Sprintf("%v&metaNodeSelector=%v", reqUrl, TicketNodeSelectorName)
	process(updateDataSelectorUrl, t)
	if ns.GetDataNodeSelector() != TicketNodeSelectorName {
		t.Errorf("failed to change data nodeset selector")
		return
	}
	process(updateMetaSelectorUrl, t)
	if ns.GetMetaNodeSelector() != TicketNodeSelectorName {
		t.Errorf("failed to change data nodeset selector")
		return
	}
	updateDataSelectorUrl = fmt.Sprintf("%v&dataNodeSelector=%v", reqUrl, CarryWeightNodeSelectorName)
	updateMetaSelectorUrl = fmt.Sprintf("%v&metaNodeSelector=%v", reqUrl, CarryWeightNodeSelectorName)
	process(updateDataSelectorUrl, t)
	process(updateMetaSelectorUrl, t)
}

func TestUpdateClusterNodeSelector(t *testing.T) {
	zone, err := server.cluster.t.getZone(testZone2)
	if err != nil {
		t.Errorf("failed to get zone, %v", err)
		return
	}
	nsc := zone.getAllNodeSet()
	if nsc.Len() == 0 {
		t.Error("nodeset count could not be 0")
		return
	}
	reqUrl := fmt.Sprintf("%v%v", hostAddr, proto.AdminSetNodeInfo)
	updateDataSelectorUrl := fmt.Sprintf("%v?dataNodeSelector=%v", reqUrl, TicketNodeSelectorName)
	updateMetaSelectorUrl := fmt.Sprintf("%v?metaNodeSelector=%v", reqUrl, TicketNodeSelectorName)
	process(updateDataSelectorUrl, t)
	failed := false
	func() {
		zone.nsLock.RLock()
		defer zone.nsLock.RUnlock()
		for _, ns := range zone.nodeSetMap {
			if ns.GetDataNodeSelector() != TicketNodeSelectorName {
				t.Errorf("failed to change data nodeset selector")
				failed = true
			}
		}
	}()
	if failed {
		return
	}
	process(updateMetaSelectorUrl, t)
	func() {
		zone.nsLock.RLock()
		defer zone.nsLock.RUnlock()
		for _, ns := range zone.nodeSetMap {
			if ns.GetMetaNodeSelector() != TicketNodeSelectorName {
				t.Errorf("failed to change data nodeset selector")
				failed = true
			}
		}
	}()
	if failed {
		return
	}
	updateDataSelectorUrl = fmt.Sprintf("%v?dataNodeSelector=%v", reqUrl, CarryWeightNodeSelectorName)
	updateMetaSelectorUrl = fmt.Sprintf("%v?metaNodeSelector=%v", reqUrl, CarryWeightNodeSelectorName)
	process(updateDataSelectorUrl, t)
	process(updateMetaSelectorUrl, t)
}

func TestUpdateClusterNodesetSelector(t *testing.T) {
	zone, err := server.cluster.t.getZone(testZone2)
	if err != nil {
		t.Errorf("failed to get zone, %v", err)
		return
	}
	nsc := zone.getAllNodeSet()
	if nsc.Len() == 0 {
		t.Error("nodeset count could not be 0")
		return
	}
	reqUrl := fmt.Sprintf("%v%v", hostAddr, proto.AdminSetNodeInfo)
	updateDataSelectorUrl := fmt.Sprintf("%v?dataNodesetSelector=%v", reqUrl, CarryWeightNodesetSelectorName)
	updateMetaSelectorUrl := fmt.Sprintf("%v?metaNodesetSelector=%v", reqUrl, CarryWeightNodesetSelectorName)
	process(updateDataSelectorUrl, t)
	if zone.GetDataNodesetSelector() != CarryWeightNodesetSelectorName {
		t.Errorf("failed to change data nodeset selector")
	}
	process(updateMetaSelectorUrl, t)
	if zone.GetMetaNodesetSelector() != CarryWeightNodesetSelectorName {
		t.Errorf("failed to change meta nodeset selector")
	}
	updateDataSelectorUrl = fmt.Sprintf("%v?dataNodesetSelector=%v", reqUrl, RoundRobinNodesetSelectorName)
	updateMetaSelectorUrl = fmt.Sprintf("%v?metaNodesetSelector=%v", reqUrl, RoundRobinNodesetSelectorName)
	process(updateDataSelectorUrl, t)
	process(updateMetaSelectorUrl, t)
}

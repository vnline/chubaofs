// Copyright 2018 The Container File System Authors.
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
	"fmt"
	"github.com/juju/errors"
	"github.com/tiglabs/containerfs/proto"
	"github.com/tiglabs/containerfs/raftstore"
	"github.com/tiglabs/containerfs/util"
	"github.com/tiglabs/containerfs/util/log"
	"sync"
	"time"
)

// Cluster stores all the cluster-level information.
type Cluster struct {
	Name                string
	vols                map[string]*Vol
	dataNodes           sync.Map
	metaNodes           sync.Map
	dpMutex             sync.Mutex   // data partition mutex
	volMutex            sync.RWMutex // volume mutex
	mnMutex             sync.RWMutex // meta node mutex
	dnMutex             sync.RWMutex // data node mutex
	leaderInfo          *LeaderInfo
	cfg                 *clusterConfig
	retainLogs          uint64
	idAlloc             *IDAllocator
	t                   *topology
	dataNodeStatInfo    *nodeStatInfo
	metaNodeStatInfo    *nodeStatInfo
	volStatInfo         sync.Map
	BadDataPartitionIds *sync.Map
	ShouldAutoAllocate  bool // Yes: true, No: false
	fsm                 *MetadataFsm
	partition           raftstore.Partition
}

func newCluster(name string, leaderInfo *LeaderInfo, fsm *MetadataFsm, partition raftstore.Partition, cfg *clusterConfig) (c *Cluster) {
	c = new(Cluster)
	c.Name = name
	c.leaderInfo = leaderInfo
	c.vols = make(map[string]*Vol, 0)
	c.cfg = cfg
	c.t = newTopology()
	c.BadDataPartitionIds = new(sync.Map)
	c.dataNodeStatInfo = new(nodeStatInfo)
	c.metaNodeStatInfo = new(nodeStatInfo)
	c.fsm = fsm
	c.partition = partition
	c.idAlloc = newIDAllocator(c.fsm.store, c.partition)

	return
}

func (c *Cluster) scheduleTask() {
	c.scheduleToCheckDataPartitions()
	c.scheduleToLoadDataPartitions()
	c.scheduleToCheckReleaseDataPartitions()
	c.scheduleToCheckHeartbeat()
	c.scheduleToCheckMetaPartitions()
	c.scheduleToUpdateStatInfo()
	c.scheduleToCheckAutoDataPartitionCreation()
	c.scheduleToCheckVolStatus()
	c.scheduleToCheckDiskRecoveryProgress()
	c.startCheckLoadMetaPartitions()
}

func (c *Cluster) masterAddr() (addr string) {
	return c.leaderInfo.addr
}

func (c *Cluster) scheduleToUpdateStatInfo() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsLeader() {
				c.updateStatInfo()
			}
			time.Sleep(time.Second * defaultIntervalToCheckHeartbeat)
		}
	}()

}

func (c *Cluster) scheduleToCheckAutoDataPartitionCreation() {
	go func() {

		// check volumes after switching leader two minutes
		time.Sleep(2 * time.Minute)
		for {
			if c.partition != nil && c.partition.IsLeader() {
				vols := c.copyVols()
				for _, vol := range vols {
					vol.checkAutoDataPartitionCreation(c)
				}
			}
			time.Sleep(2 * time.Minute)
		}
	}()
}

func (c *Cluster) scheduleToCheckDataPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsLeader() {
				c.checkDataPartitions()
			}
			time.Sleep(time.Second * time.Duration(c.cfg.IntervalToCheckDataPartition))
		}
	}()
}

func (c *Cluster) scheduleToCheckVolStatus() {
	go func() {
		//check vols after switching leader two minutes
		for {
			if c.partition.IsLeader() {
				vols := c.copyVols()
				for _, vol := range vols {
					vol.checkStatus(c)
				}
			}
			time.Sleep(time.Second * time.Duration(c.cfg.IntervalToCheckDataPartition))
		}
	}()
}

// Check the replica status of each data partition.
func (c *Cluster) checkDataPartitions() {
	vols := c.allVols()
	for _, vol := range vols {
		readWrites := vol.checkDataPartitions(c)
		vol.dataPartitions.setReadWriteDataPartitions(readWrites, c.Name)
		vol.dataPartitions.updateResponseCache(true, 0)
		msg := fmt.Sprintf("action[checkDataPartitions],vol[%v] can readWrite partitions:%v  ", vol.Name, vol.dataPartitions.readableAndWritableCnt)
		log.LogInfo(msg)
	}
}

func (c *Cluster) scheduleToLoadDataPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsLeader() {
				c.doLoadDataPartitions()
			}
			time.Sleep(time.Second)
		}
	}()
}

func (c *Cluster) doLoadDataPartitions() {
	vols := c.allVols()
	for _, vol := range vols {
		vol.loadDataPartition(c)
	}
}

func (c *Cluster) scheduleToCheckReleaseDataPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsLeader() {
				c.releaseDataPartitionAfterLoad()
			}
			time.Sleep(time.Second * defaultIntervalToFreeDataPartition)
		}
	}()
}

// Release the memory used for loading the data partition.
func (c *Cluster) releaseDataPartitionAfterLoad() {
	vols := c.copyVols()
	for _, vol := range vols {
		vol.releaseDataPartitions(c.cfg.numberOfDataPartitionsToFree, c.cfg.secondsToFreeDataPartitionAfterLoad)
	}
}

func (c *Cluster) scheduleToCheckHeartbeat() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsLeader() {
				c.checkLeaderAddr()
				c.checkDataNodeHeartbeat()
			}
			time.Sleep(time.Second * defaultIntervalToCheckHeartbeat)
		}
	}()

	go func() {
		for {
			if c.partition != nil && c.partition.IsLeader() {
				c.checkMetaNodeHeartbeat()
			}
			time.Sleep(time.Second * defaultIntervalToCheckHeartbeat)
		}
	}()
}

func (c *Cluster) checkLeaderAddr() {
	leaderID, _ := c.partition.LeaderTerm()
	c.leaderInfo.addr = AddrDatabase[leaderID]
}

func (c *Cluster) checkDataNodeHeartbeat() {
	tasks := make([]*proto.AdminTask, 0)
	c.dataNodes.Range(func(addr, dataNode interface{}) bool {
		node := dataNode.(*DataNode)
		node.checkLiveness()
		task := node.createHeartbeatTask(c.masterAddr())
		tasks = append(tasks, task)
		return true
	})
	c.addDataNodeTasks(tasks)
}

func (c *Cluster) checkMetaNodeHeartbeat() {
	tasks := make([]*proto.AdminTask, 0)
	c.metaNodes.Range(func(addr, metaNode interface{}) bool {
		node := metaNode.(*MetaNode)
		node.checkHeartbeat()
		task := node.createHeartbeatTask(c.masterAddr())
		tasks = append(tasks, task)
		return true
	})
	c.addMetaNodeTasks(tasks)
}

func (c *Cluster) scheduleToCheckMetaPartitions() {
	go func() {
		for {
			if c.partition != nil && c.partition.IsLeader() {
				c.checkMetaPartitions()
			}
			time.Sleep(time.Second * time.Duration(c.cfg.IntervalToCheckDataPartition))
		}
	}()
}

func (c *Cluster) checkMetaPartitions() {
	vols := c.allVols()
	for _, vol := range vols {
		vol.checkMetaPartitions(c)
	}
}

func (c *Cluster) addMetaNode(nodeAddr string) (id uint64, err error) {
	c.mnMutex.Lock()
	defer c.mnMutex.Unlock()
	var metaNode *MetaNode
	if value, ok := c.metaNodes.Load(nodeAddr); ok {
		metaNode = value.(*MetaNode)
		return metaNode.ID, nil
	}
	metaNode = newMetaNode(nodeAddr, c.Name)
	ns := c.t.getAvailNodeSetForMetaNode()
	if ns == nil {
		if ns, err = c.createNodeSet(); err != nil {
			goto errHandler
		}
	}
	if id, err = c.idAlloc.allocateCommonID(); err != nil {
		goto errHandler
	}
	metaNode.ID = id
	metaNode.NodeSetID = ns.ID
	if err = c.syncAddMetaNode(metaNode); err != nil {
		goto errHandler
	}
	ns.increaseMetaNodeLen()
	if err = c.syncUpdateNodeSet(ns); err != nil {
		ns.decreaseMetaNodeLen()
		goto errHandler
	}
	c.metaNodes.Store(nodeAddr, metaNode)
	log.LogInfof("action[addMetaNode],clusterID[%v] metaNodeAddr:%v,nodeSetId[%v],dLen[%v],mLen[%v],capacity[%v]",
		c.Name, nodeAddr, ns.ID, ns.dataNodeLen, ns.metaNodeLen, ns.Capacity)
	return
errHandler:
	err = fmt.Errorf("action[addMetaNode],clusterID[%v] metaNodeAddr:%v err:%v ",
		c.Name, nodeAddr, err.Error())
	log.LogError(errors.ErrorStack(err))
	Warn(c.Name, err.Error())
	return
}

func (c *Cluster) createNodeSet() (ns *nodeSet, err error) {
	var id uint64
	if id, err = c.idAlloc.allocateCommonID(); err != nil {
		return
	}
	ns = newNodeSet(id, c.cfg.nodeSetCapacity)
	if err = c.syncAddNodeSet(ns); err != nil {
		return
	}
	c.t.putNodeSet(ns)
	return
}

func (c *Cluster) addDataNode(nodeAddr string) (id uint64, err error) {
	c.dnMutex.Lock()
	defer c.dnMutex.Unlock()
	var dataNode *DataNode
	if node, ok := c.dataNodes.Load(nodeAddr); ok {
		dataNode = node.(*DataNode)
		return dataNode.ID, nil
	}

	dataNode = newDataNode(nodeAddr, c.Name)
	ns := c.t.getAvailNodeSetForDataNode()
	if ns == nil {
		if ns, err = c.createNodeSet(); err != nil {
			goto errHandler
		}
	}
	// allocate dataNode id
	if id, err = c.idAlloc.allocateCommonID(); err != nil {
		goto errHandler
	}
	dataNode.ID = id
	dataNode.NodeSetID = ns.ID
	if err = c.syncAddDataNode(dataNode); err != nil {
		goto errHandler
	}
	ns.increaseDataNodeLen()
	if err = c.syncUpdateNodeSet(ns); err != nil {
		ns.decreaseDataNodeLen()
		goto errHandler
	}
	c.dataNodes.Store(nodeAddr, dataNode)
	log.LogInfof("action[addDataNode],clusterID[%v] dataNodeAddr:%v,nodeSetId[%v],dLen[%v],mLen[%v],capacity[%v]",
		c.Name, nodeAddr, ns.ID, ns.dataNodeLen, ns.metaNodeLen, ns.Capacity)
	return
errHandler:
	err = fmt.Errorf("action[addDataNode],clusterID[%v] dataNodeAddr:%v err:%v ", c.Name, nodeAddr, err.Error())
	log.LogError(errors.ErrorStack(err))
	Warn(c.Name, err.Error())
	return
}

func (c *Cluster) getDataPartitionByID(partitionID uint64) (dp *DataPartition, err error) {
	vols := c.copyVols()
	for _, vol := range vols {
		if dp, err = vol.getDataPartitionByID(partitionID); err == nil {
			return
		}
	}
	err = dataPartitionNotFound(partitionID)
	return
}

func (c *Cluster) getMetaPartitionByID(id uint64) (mp *MetaPartition, err error) {
	vols := c.copyVols()
	for _, vol := range vols {
		if mp, err = vol.metaPartition(id); err == nil {
			return
		}
	}
	err = metaPartitionNotFound(id)
	return
}

func (c *Cluster) putVol(vol *Vol) {
	c.volMutex.Lock()
	defer c.volMutex.Unlock()
	if _, ok := c.vols[vol.Name]; !ok {
		c.vols[vol.Name] = vol
	}
}

func (c *Cluster) getVol(volName string) (vol *Vol, err error) {
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	vol, ok := c.vols[volName]
	if !ok {
		err = errors.Annotatef(volNotFound(volName), "%v not found", volName)
	}
	return
}

func (c *Cluster) deleteVol(name string) {
	c.volMutex.Lock()
	defer c.volMutex.Unlock()
	delete(c.vols, name)
	return
}

func (c *Cluster) markDeleteVol(name string) (err error) {
	var vol *Vol
	if vol, err = c.getVol(name); err != nil {
		return
	}
	vol.Status = markDelete
	if err = c.syncUpdateVol(vol); err != nil {
		vol.Status = normal
		return
	}
	return
}

// Synchronously create a data partition.
// 1. Choose one of the available data nodes.
// 2. Assign it a partition ID.
// 3. Communicate with the data node to synchronously create a data partition.
// - If succeeded, replicate the data through raft and persist it to RocksDB.
// - Otherwise, throw errors
func (c *Cluster) createDataPartition(volName string) (dp *DataPartition, err error) {
	var (
		vol         *Vol
		partitionID uint64
		targetHosts []string
		targetPeers []proto.Peer
		wg          sync.WaitGroup
	)
	c.dpMutex.Lock()
	defer c.dpMutex.Unlock()
	if vol, err = c.getVol(volName); err != nil {
		return
	}
	errChannel := make(chan error, vol.dpReplicaNum)
	if targetHosts, targetPeers, err = c.chooseTargetDataNodes(int(vol.dpReplicaNum)); err != nil {
		goto errHandler
	}
	if partitionID, err = c.idAlloc.allocateDataPartitionID(); err != nil {
		goto errHandler
	}
	dp = newDataPartition(partitionID, vol.dpReplicaNum, volName, vol.ID, vol.IsRandomWrite)
	dp.Hosts = targetHosts
	dp.Peers = targetPeers
	for _, host := range targetHosts {
		wg.Add(1)
		go func(host string) {
			defer func() {
				wg.Done()
			}()
			if err = c.syncCreateDataPartitionToDataNode(host, vol.dataPartitionSize, dp); err != nil {
				errChannel <- err
				return
			}
			dp.Lock()
			defer dp.Unlock()
			if err = dp.postProcessingDataPartitionCreation(host, c); err != nil {
				errChannel <- err
			}
		}(host)
	}
	wg.Wait()
	select {
	case err = <-errChannel:
		goto errHandler
	default:
		dp.Status = proto.ReadWrite
	}
	if err = c.syncAddDataPartition(dp); err != nil {
		goto errHandler
	}
	vol.dataPartitions.put(dp)
	log.LogInfof("action[createDataPartition] success,volName[%v],partitionId[%v]", volName, partitionID)
	return
errHandler:
	err = fmt.Errorf("action[createDataPartition],clusterID[%v] vol[%v] Err:%v ", c.Name, volName, err.Error())
	log.LogError(errors.ErrorStack(err))
	Warn(c.Name, err.Error())
	return
}

func (c *Cluster) syncCreateDataPartitionToDataNode(host string, size uint64, dp *DataPartition) (err error) {
	task := dp.createTaskToCreateDataPartition(host, size)
	dataNode, err := c.dataNode(host)
	if err != nil {
		return
	}
	conn, err := dataNode.TaskManager.connPool.GetConnect(dataNode.Addr)
	if err != nil {
		return
	}
	if _, err = dataNode.TaskManager.syncSendAdminTask(task, conn); err != nil {
		return
	}
	dataNode.TaskManager.connPool.PutConnect(conn, false)
	return
}

func (c *Cluster) syncCreateMetaPartitionToMetaNode(host string, mp *MetaPartition) (err error) {
	hosts := make([]string, 0)
	hosts = append(hosts, host)
	tasks := mp.buildNewMetaPartitionTasks(hosts, mp.Peers, mp.volName)
	metaNode, err := c.metaNode(host)
	if err != nil {
		return
	}
	conn, err := metaNode.Sender.connPool.GetConnect(metaNode.Addr)
	if err != nil {
		return
	}
	if _, err = metaNode.Sender.syncSendAdminTask(tasks[0], conn); err != nil {
		return
	}
	metaNode.Sender.connPool.PutConnect(conn, false)
	return
}

func (c *Cluster) chooseTargetDataNodes(replicaNum int) (hosts []string, peers []proto.Peer, err error) {
	var (
		masterAddr  []string
		addrs       []string
		racks       []*Rack
		rack        *Rack
		masterPeers []proto.Peer
		slavePeers  []proto.Peer
	)
	hosts = make([]string, 0)
	peers = make([]proto.Peer, 0)
	ns, err := c.t.allocNodeSetForDataNode(uint8(replicaNum))
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	if ns.isSingleRack() {
		var newHosts []string
		if rack, err = ns.getRack(ns.racks[0]); err != nil {
			return nil, nil, errors.Trace(err)
		}
		if newHosts, peers, err = rack.getAvailDataNodeHosts(hosts, replicaNum); err != nil {
			return nil, nil, errors.Trace(err)
		}
		hosts = newHosts
		return
	}

	if racks, err = ns.allocRacks(replicaNum, nil); err != nil {
		return nil, nil, errors.Trace(err)
	}

	if len(racks) == 2 {
		masterRack := racks[0]
		slaveRack := racks[1]
		masterReplicaNum := replicaNum/2 + 1
		slaveReplicaNum := replicaNum - masterReplicaNum
		if masterAddr, masterPeers, err = masterRack.getAvailDataNodeHosts(hosts, masterReplicaNum); err != nil {
			return nil, nil, errors.Trace(err)
		}
		hosts = append(hosts, masterAddr...)
		peers = append(peers, masterPeers...)
		if addrs, slavePeers, err = slaveRack.getAvailDataNodeHosts(hosts, slaveReplicaNum); err != nil {
			return nil, nil, errors.Trace(err)
		}
		hosts = append(hosts, addrs...)
		peers = append(peers, slavePeers...)
	} else if len(racks) == replicaNum {
		for index := 0; index < replicaNum; index++ {
			rack := racks[index]
			var selectPeers []proto.Peer
			if addrs, selectPeers, err = rack.getAvailDataNodeHosts(hosts, 1); err != nil {
				return nil, nil, errors.Trace(err)
			}
			hosts = append(hosts, addrs...)
			peers = append(peers, selectPeers...)
		}
	}
	if len(hosts) != replicaNum {
		return nil, nil, ErrNoDataNodeToCreateDataPartition
	}
	return
}

func (c *Cluster) dataNode(addr string) (dataNode *DataNode, err error) {
	value, ok := c.dataNodes.Load(addr)
	if !ok {
		err = errors.Annotatef(dataNodeNotFound(addr), "%v not found", addr)
		return
	}
	dataNode = value.(*DataNode)
	return
}

func (c *Cluster) metaNode(addr string) (metaNode *MetaNode, err error) {
	value, ok := c.metaNodes.Load(addr)
	if !ok {
		err = errors.Annotatef(metaNodeNotFound(addr), "%v not found", addr)
		return
	}
	metaNode = value.(*MetaNode)
	return
}

func (c *Cluster) dataNodeOffLine(dataNode *DataNode) (err error) {
	msg := fmt.Sprintf("action[dataNodeOffLine], Node[%v] OffLine", dataNode.Addr)
	log.LogWarn(msg)
	safeVols := c.allVols()
	for _, vol := range safeVols {
		for _, dp := range vol.dataPartitions.partitions {
			if err = c.decommissionDataPartition(dataNode.Addr, dp, dataNodeOfflineErr); err != nil {
				return
			}
		}
	}
	if err = c.syncDeleteDataNode(dataNode); err != nil {
		msg = fmt.Sprintf("action[dataNodeOffLine],clusterID[%v] Node[%v] OffLine failed,err[%v]",
			c.Name, dataNode.Addr, err)
		Warn(c.Name, msg)
		return
	}
	c.delDataNodeFromCache(dataNode)
	msg = fmt.Sprintf("action[dataNodeOffLine],clusterID[%v] Node[%v] OffLine success",
		c.Name, dataNode.Addr)
	Warn(c.Name, msg)
	return
}

func (c *Cluster) delDataNodeFromCache(dataNode *DataNode) {
	c.dataNodes.Delete(dataNode.Addr)
	c.t.deleteDataNode(dataNode)
	go dataNode.clear()
}

// Decommission a data partition.
// 1. Check if we can decommission a data partition. In the following cases, we are not allowed to do so:
// - (a) a replica is not in the latest host list;
// - (b) there is already a replica been taken offline;
// - (c) the remaining number of replicas is less than the majority
// 2. Choose a new data node.
// 3. Persist the latest host list.
// 4. Generate an async task to delete the replica.
// 5. Synchronously create a data partition.
// 6. Set the data partition as readOnly.
func (c *Cluster) decommissionDataPartition(offlineAddr string, dp *DataPartition, errMsg string) (err error) {
	var (
		newHosts   []string
		newAddr    string
		newPeers   []proto.Peer
		msg        string
		tasks      []*proto.AdminTask
		task       *proto.AdminTask
		dataNode   *DataNode
		rack       *Rack
		vol        *Vol
		removePeer proto.Peer
		replica    *DataReplica
	)
	badPartitionIDs := make([]uint64, 0)
	badPartitionIDs = append(badPartitionIDs, dp.PartitionID)
	dp.Lock()
	defer dp.Unlock()
	if ok := dp.hasHost(offlineAddr); !ok {
		return
	}

	if vol, err = c.getVol(dp.VolName); err != nil {
		goto errHandler
	}

	if replica, err = dp.getReplica(offlineAddr); err != nil {
		goto errHandler
	}

	if err = dp.hasMissingOneReplica(int(vol.dpReplicaNum)); err != nil {
		goto errHandler
	}

	// if the partition can be offline or not
	if err = dp.canBeOffLine(offlineAddr); err != nil {
		goto errHandler
	}

	if dataNode, err = c.dataNode(offlineAddr); err != nil {
		goto errHandler
	}

	if dataNode.RackName == "" {
		return
	}
	if rack, err = c.t.getRack(dataNode); err != nil {
		goto errHandler
	}
	if newHosts, newPeers, err = rack.getAvailDataNodeHosts(dp.Hosts, 1); err != nil {
		// select data nodes from the node set
		if newHosts, newPeers, err = c.chooseTargetDataNodes(1); err != nil {
			goto errHandler
		}
	}
	newAddr = newHosts[0]
	for _, host := range dp.Hosts {
		if host == offlineAddr {
			removePeer = proto.Peer{ID: dataNode.ID, Addr: host}
			continue
		}
		if dataNode, err = c.dataNode(host); err != nil {
			goto errHandler
		}
		newPeers = append(newPeers, proto.Peer{ID: dataNode.ID, Addr: host})
	}

	for _, replica := range dp.Replicas {
		if replica.Addr == offlineAddr {
			removePeer = proto.Peer{ID: replica.dataNode.ID, Addr: replica.Addr}
		} else {
			newPeers = append(newPeers, proto.Peer{ID: replica.dataNode.ID, Addr: replica.Addr})
		}
	}

	if task, err = dp.createTaskToDecommissionDataPartition(removePeer, newPeers[0]); err != nil {
		goto errHandler
	}
	dp.logDecommissionedDataPartition(offlineAddr)

	if err = dp.updateForOffline(offlineAddr, newAddr, dp.VolName, newPeers, c); err != nil {
		goto errHandler
	}
	dp.removeReplicaByAddr(offlineAddr)
	dp.checkAndRemoveMissReplica(offlineAddr)
	tasks = make([]*proto.AdminTask, 0)
	tasks = append(tasks, task)
	c.addDataNodeTasks(tasks)
	if err = c.syncCreateDataPartitionToDataNode(newAddr, vol.dataPartitionSize, dp); err != nil {
		goto errHandler
	}
	if err = dp.postProcessingDataPartitionCreation(newAddr, c); err != nil {
		goto errHandler
	}
	dp.Status = proto.ReadOnly
	dp.isRecover = true
	c.BadDataPartitionIds.Store(fmt.Sprintf("%s:%s", offlineAddr, replica.DiskPath), badPartitionIDs)
	log.LogWarnf("clusterID[%v] partitionID:%v  on Node:%v offline success,newHost[%v],PersistenceHosts:[%v]",
		c.Name, dp.PartitionID, offlineAddr, newAddr, dp.Hosts)
	return
errHandler:
	msg = fmt.Sprintf(errMsg+" clusterID[%v] partitionID:%v  on Node:%v  "+
		"Then Fix It on newHost:%v   Err:%v , PersistenceHosts:%v  ",
		c.Name, dp.PartitionID, offlineAddr, newAddr, err, dp.Hosts)
	if err != nil {
		Warn(c.Name, msg)
	}
	return
}

func (c *Cluster) decommissionMetaNode(metaNode *MetaNode) {
	msg := fmt.Sprintf("action[decommissionMetaNode],clusterID[%v] Node[%v] OffLine", c.Name, metaNode.Addr)
	log.LogWarn(msg)

	safeVols := c.allVols()
	for _, vol := range safeVols {
		for _, mp := range vol.MetaPartitions {
			// err is not handled here.
			c.decommissionMetaPartition(metaNode.Addr, mp)
		}
	}
	if err := c.syncDeleteMetaNode(metaNode); err != nil {
		msg = fmt.Sprintf("action[decommissionMetaNode],clusterID[%v] Node[%v] OffLine failed,err[%v]",
			c.Name, metaNode.Addr, err)
		Warn(c.Name, msg)
		return
	}
	c.deleteMetaNodeFromCache(metaNode)
	msg = fmt.Sprintf("action[decommissionMetaNode],clusterID[%v] Node[%v] OffLine success", c.Name, metaNode.Addr)
	Warn(c.Name, msg)
}

func (c *Cluster) deleteMetaNodeFromCache(metaNode *MetaNode) {
	c.metaNodes.Delete(metaNode.Addr)
	c.t.deleteMetaNode(metaNode)
	go metaNode.clean()
}

func (c *Cluster) updateVol(name string, capacity int) (err error) {
	var vol *Vol
	if vol, err = c.getVol(name); err != nil {
		goto errHandler
	}
	if uint64(capacity) < vol.Capacity {
		err = fmt.Errorf("capacity[%v] less than old capacity[%v]", capacity, vol.Capacity)
		goto errHandler
	}
	vol.setCapacity(uint64(capacity))
	if err = c.syncUpdateVol(vol); err != nil {
		goto errHandler
	}
	return
errHandler:
	err = fmt.Errorf("action[updateVol], clusterID[%v] name:%v, err:%v ", c.Name, name, err.Error())
	log.LogError(errors.ErrorStack(err))
	Warn(c.Name, err.Error())
	return
}

// Create a new volume.
// By default we create 3 meta partitions and 10 data partitions during initialization.
func (c *Cluster) createVol(name string, replicaNum uint8, randomWrite bool, size, capacity int) (err error) {
	var (
		vol                     *Vol
		dataPartitionSize       uint64
		readWriteDataPartitions int
	)
	if size == 0 {
		dataPartitionSize = util.DefaultDataPartitionSize
	} else {
		dataPartitionSize = uint64(size) * util.GB
	}
	if err = c.doCreateVol(name, replicaNum, randomWrite, dataPartitionSize, uint64(capacity)); err != nil {
		goto errHandler
	}

	if vol, err = c.getVol(name); err != nil {
		goto errHandler
	}
	vol.initMetaPartitions(c)
	if len(vol.MetaPartitions) == 0 {
		vol.Status = markDelete
		if err = c.syncDeleteVol(vol); err != nil {
			log.LogErrorf("action[createVol] failed,vol[%v] err[%v]", vol.Name, err)
		}
		c.deleteVol(name)
		goto errHandler
	}
	for retryCount := 0; readWriteDataPartitions < defaultInitDataPartitionCnt && retryCount < 3; retryCount++ {
		vol.initDataPartitions(c)
		readWriteDataPartitions = vol.checkDataPartitionStatus(c)
	}
	vol.dataPartitions.readableAndWritableCnt = readWriteDataPartitions
	log.LogInfof("action[createVol] vol[%v],readableAndWritableCnt[%v]", name, readWriteDataPartitions)
	return

errHandler:
	err = fmt.Errorf("action[createVol], clusterID[%v] name:%v, err:%v ", c.Name, name, err)
	log.LogError(errors.ErrorStack(err))
	Warn(c.Name, err.Error())
	return
}

func (c *Cluster) doCreateVol(name string, replicaNum uint8, randomWrite bool, dpSize, capacity uint64) (err error) {
	var (
		id  uint64
		vol *Vol
	)
	if _, err = c.getVol(name); err == nil {
		err = exists(name)
		goto errHandler
	}

	if id, err = c.idAlloc.allocateCommonID(); err != nil {
		goto errHandler
	}
	vol = newVol(id, name, replicaNum, randomWrite, dpSize, capacity)
	if err = c.syncAddVol(vol); err != nil {
		goto errHandler
	}
	return
errHandler:
	err = fmt.Errorf("action[doCreateVol], clusterID[%v] name:%v, err:%v ", c.Name, name, err.Error())
	log.LogError(errors.ErrorStack(err))
	Warn(c.Name, err.Error())
	return
}

// Update the upper bound of the inode ids in a meta partition.
func (c *Cluster) updateInodeIDRange(volName string, start uint64) (err error) {

	var (
		maxPartitionID uint64
		vol            *Vol
		partition      *MetaPartition
	)

	if vol, err = c.getVol(volName); err != nil {
		return errors.Annotatef(err, "get vol [%v] err", volName)
	}
	maxPartitionID = vol.maxPartitionID()
	if partition, err = vol.metaPartition(maxPartitionID); err != nil {
		return errors.Annotatef(err, "get meta partition [%v] err", maxPartitionID)
	}
	if start < partition.MaxNodeID {
		err = errors.Errorf("next meta partition start must be larger than %v", partition.MaxNodeID)
		return
	}
	if _, err := partition.getMetaReplicaLeader(); err != nil {
		return errors.Annotate(err, "can't execute")
	}
	partition.Lock()
	defer partition.Unlock()
	partition.updateInodeIDRange(c, start)
	return
}

func (c *Cluster) createMetaPartition(volName string, start, end uint64) (err error) {
	var (
		vol         *Vol
		mp          *MetaPartition
		hosts       []string
		partitionID uint64
		peers       []proto.Peer
		wg          sync.WaitGroup
	)
	if vol, err = c.getVol(volName); err != nil {
		log.LogWarnf("action[createMetaPartition] get vol [%v] err", volName)
		return
	}
	errChannel := make(chan error, vol.mpReplicaNum)

	if hosts, peers, err = c.chooseTargetMetaHosts(int(vol.mpReplicaNum)); err != nil {
		return errors.Trace(err)
	}
	log.LogInfof("target meta hosts:%v,peers:%v", hosts, peers)
	if partitionID, err = c.idAlloc.allocateMetaPartitionID(); err != nil {
		return errors.Trace(err)
	}
	mp = newMetaPartition(partitionID, start, end, vol.mpReplicaNum, volName, vol.ID)
	mp.setHosts(hosts)
	mp.setPeers(peers)
	for _, host := range hosts {
		wg.Add(1)
		go func(host string) {
			defer func() {
				wg.Done()
			}()
			if err = c.syncCreateMetaPartitionToMetaNode(host, mp); err != nil {
				errChannel <- err
				return
			}
			mp.Lock()
			defer mp.Unlock()
			if err = mp.postProcessingPartitionCreation(host, c); err != nil {
				errChannel <- err
			}
		}(host)
	}
	wg.Wait()
	select {
	case err = <-errChannel:
		return errors.Trace(err)
	default:
		mp.Status = proto.ReadWrite
	}
	if err = c.syncAddMetaPartition(mp); err != nil {
		return errors.Trace(err)
	}
	vol.addMetaPartition(mp)
	log.LogInfof("action[createMetaPartition] success,volName[%v],partition[%v]", volName, partitionID)
	return
}

func (c *Cluster) hasEnoughWritableMetaHosts(replicaNum int, setID uint64) bool {
	ns, err := c.t.getNodeSet(setID)
	if err != nil {
		log.LogErrorf("nodeSet[%v] not exist", setID)
		return false
	}
	maxTotal := ns.getMetaNodeMaxTotal()
	excludeHosts := make([]string, 0)
	nodes, _ := ns.getAllCarryNodes(maxTotal, excludeHosts)
	if nodes != nil && len(nodes) >= replicaNum {
		return true
	}
	return false
}

// Choose the target hosts from the available node sets and meta nodes.
func (c *Cluster) chooseTargetMetaHosts(replicaNum int) (hosts []string, peers []proto.Peer, err error) {
	var (
		masterAddr []string
		slaveAddrs []string
		masterPeer []proto.Peer
		slavePeers []proto.Peer
		ns         *nodeSet
	)
	if ns, err = c.t.allocNodeSetForMetaNode(uint8(replicaNum)); err != nil {
		return nil, nil, errors.Trace(err)
	}

	hosts = make([]string, 0)
	if masterAddr, masterPeer, err = ns.getAvailMetaNodeHosts(hosts, 1); err != nil {
		return nil, nil, errors.Trace(err)
	}
	peers = append(peers, masterPeer...)
	hosts = append(hosts, masterAddr[0])
	otherReplica := replicaNum - 1
	if otherReplica == 0 {
		return
	}
	if slaveAddrs, slavePeers, err = ns.getAvailMetaNodeHosts(hosts, otherReplica); err != nil {
		return nil, nil, errors.Trace(err)
	}
	hosts = append(hosts, slaveAddrs...)
	peers = append(peers, slavePeers...)
	if len(hosts) != replicaNum {
		return nil, nil, ErrNoMetaNodeToCreateMetaPartition
	}
	return
}

func (c *Cluster) dataNodeCount() (len int) {
	c.dataNodes.Range(func(key, value interface{}) bool {
		len++
		return true
	})
	return
}

func (c *Cluster) allDataNodes() (dataNodes []NodeView) {
	dataNodes = make([]NodeView, 0)
	c.dataNodes.Range(func(addr, node interface{}) bool {
		dataNode := node.(*DataNode)
		dataNodes = append(dataNodes, NodeView{Addr: dataNode.Addr, Status: dataNode.isActive, ID: dataNode.ID})
		return true
	})
	return
}

// Percentage of active data nodes.
func (c *Cluster) liveDataNodesRate() (rate float32) {
	dataNodes := make([]NodeView, 0)
	liveDataNodes := make([]NodeView, 0)
	c.dataNodes.Range(func(addr, node interface{}) bool {
		dataNode := node.(*DataNode)
		view := NodeView{Addr: dataNode.Addr, Status: dataNode.isActive}
		dataNodes = append(dataNodes, view)
		if dataNode.isActive && time.Since(dataNode.ReportTime) < time.Second*time.Duration(2*defaultIntervalToCheckHeartbeat) {
			liveDataNodes = append(liveDataNodes, view)
		}
		return true
	})
	return float32(len(liveDataNodes)) / float32(len(dataNodes))
}

// Percentage of active meta nodes.
func (c *Cluster) liveMetaNodesRate() (rate float32) {
	metaNodes := make([]NodeView, 0)
	liveMetaNodes := make([]NodeView, 0)
	c.metaNodes.Range(func(addr, node interface{}) bool {
		metaNode := node.(*MetaNode)
		view := NodeView{Addr: metaNode.Addr, Status: metaNode.IsActive, ID: metaNode.ID}
		metaNodes = append(metaNodes, view)
		if metaNode.IsActive && time.Since(metaNode.ReportTime) < time.Second*time.Duration(2*defaultIntervalToCheckHeartbeat) {
			liveMetaNodes = append(liveMetaNodes, view)
		}
		return true
	})
	return float32(len(liveMetaNodes)) / float32(len(metaNodes))
}

func (c *Cluster) allMetaNodes() (metaNodes []NodeView) {
	metaNodes = make([]NodeView, 0)
	c.metaNodes.Range(func(addr, node interface{}) bool {
		metaNode := node.(*MetaNode)
		metaNodes = append(metaNodes, NodeView{ID: metaNode.ID, Addr: metaNode.Addr, Status: metaNode.IsActive})
		return true
	})
	return
}

func (c *Cluster) allVolNames() (vols []string) {
	vols = make([]string, 0)
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	for name := range c.vols {
		vols = append(vols, name)
	}
	return
}

func (c *Cluster) copyVols() (vols map[string]*Vol) {
	vols = make(map[string]*Vol, 0)
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	for name, vol := range c.vols {
		vols[name] = vol
	}
	return
}

// Return all the volumes except the ones that have been marked to be deleted.
func (c *Cluster) allVols() (vols map[string]*Vol) {
	vols = make(map[string]*Vol, 0)
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	for name, vol := range c.vols {
		if vol.Status == normal {
			vols[name] = vol
		}
	}
	return
}

func (c *Cluster) getDataPartitionCount() (count int) {
	c.volMutex.RLock()
	defer c.volMutex.RUnlock()
	for _, vol := range c.vols {
		count = count + len(vol.dataPartitions.partitions)
	}
	return
}

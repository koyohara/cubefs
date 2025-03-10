// Copyright 2023 The CubeFS Authors.
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
	"math/rand"
	"sort"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
)

const RoundRobinNodesetSelectorName = "RoundRobin"

const CarryWeightNodesetSelectorName = "CarryWeight"

const AvailableSpaceFirstNodesetSelectorName = "AvailableSpaceFirst"

const TicketNodesetSelectorName = "Ticket"

const DefaultNodesetSelectorName = RoundRobinNodeSelectorName

func (ns *nodeSet) getDataNodeTotalSpace() (toalSpace uint64) {
	ns.dataNodes.Range(func(key, value interface{}) bool {
		dataNode := value.(*DataNode)
		toalSpace += dataNode.Total
		return true
	})
	return
}

func (ns *nodeSet) getMetaNodeTotalSpace() (toalSpace uint64) {
	ns.metaNodes.Range(func(key, value interface{}) bool {
		metaNode := value.(*MetaNode)
		toalSpace += metaNode.Total
		return true
	})
	return
}

func (ns *nodeSet) getDataNodeTotalAvailableSpace() (space uint64) {
	ns.dataNodes.Range(func(key, value interface{}) bool {
		dataNode := value.(*DataNode)
		if !dataNode.ToBeOffline {
			space += dataNode.AvailableSpace
		}
		return true
	})
	return
}

func (ns *nodeSet) getMetaNodeTotalAvailableSpace() (space uint64) {
	ns.metaNodes.Range(func(key, value interface{}) bool {
		metaNode := value.(*MetaNode)
		if !metaNode.ToBeOffline {
			space += metaNode.Total - metaNode.Used
		}
		return true
	})
	return
}

func (ns *nodeSet) canWriteFor(nodeType NodeType, replica int) bool {
	switch nodeType {
	case DataNodeType:
		return ns.canWriteForDataNode(replica)
	case MetaNodeType:
		return ns.canWriteForMetaNode(replica)
	default:
		panic("unknow node type")
	}
}

func (ns *nodeSet) getTotalSpaceOf(nodeType NodeType) uint64 {
	switch nodeType {
	case DataNodeType:
		return ns.getDataNodeTotalSpace()
	case MetaNodeType:
		return ns.getMetaNodeTotalSpace()
	default:
		panic("unknow node type")
	}
}

func (ns *nodeSet) getTotalAvailableSpaceOf(nodeType NodeType) uint64 {
	switch nodeType {
	case DataNodeType:
		return ns.getDataNodeTotalAvailableSpace()
	case MetaNodeType:
		return ns.getMetaNodeTotalAvailableSpace()
	default:
		panic("unknow node type")
	}
}

type NodesetSelector interface {
	GetName() string

	Select(nsc nodeSetCollection, excludeNodeSets []uint64, replicaNum uint8) (ns *nodeSet, err error)
}

type RoundRobinNodesetSelector struct {
	index int

	nodeType NodeType
}

func (s *RoundRobinNodesetSelector) Select(nsc nodeSetCollection, excludeNodeSets []uint64, replicaNum uint8) (ns *nodeSet, err error) {
	// sort nodesets by id, so we can get a node list that is as stable as possible
	sort.Slice(nsc, func(i, j int) bool {
		return nsc[i].ID < nsc[j].ID
	})
	for i := 0; i < len(nsc); i++ {

		if s.index >= len(nsc) {
			s.index = 0
		}

		ns = nsc[s.index]
		s.index++

		if containsID(excludeNodeSets, ns.ID) {
			continue
		}
		if ns.canWriteFor(s.nodeType, int(replicaNum)) {
			return
		}
	}

	switch s.nodeType {
	case DataNodeType:
		err = errors.NewError(proto.ErrNoNodeSetToCreateDataPartition)
	case MetaNodeType:
		err = errors.NewError(proto.ErrNoNodeSetToCreateMetaPartition)
	default:
		panic("unknow node type")
	}
	return
}

func (s *RoundRobinNodesetSelector) GetName() string {
	return RoundRobinNodesetSelectorName
}

func NewRoundRobinNodesetSelector(nodeType NodeType) *RoundRobinNodesetSelector {
	return &RoundRobinNodesetSelector{
		nodeType: nodeType,
	}
}

type CarryWeightNodesetSelector struct {
	carrys map[uint64]float64

	nodeType NodeType
}

func (s *CarryWeightNodesetSelector) GetName() string {
	return CarryWeightNodesetSelectorName
}

func (s *CarryWeightNodesetSelector) getMaxTotal(nsc nodeSetCollection) uint64 {
	total := uint64(0)
	for i := 0; i < nsc.Len(); i++ {
		tmp := nsc[i].getTotalSpaceOf(s.nodeType)
		if tmp > total {
			total = tmp
		}
	}
	return total
}

func (s *CarryWeightNodesetSelector) prepareCarry(nsc nodeSetCollection, total uint64) {
	for _, nodeset := range nsc {
		id := nodeset.ID
		if _, ok := s.carrys[id]; !ok {
			// use total available space to calculate initial weight
			s.carrys[id] = float64(nodeset.getTotalAvailableSpaceOf(s.nodeType)) / float64(total)
		}
	}
}

func (s *CarryWeightNodesetSelector) Select(nsc nodeSetCollection, excludeNodeSets []uint64, replicaNum uint8) (ns *nodeSet, err error) {
	total := s.getMaxTotal(nsc)
	// prepare weight of evert nodesets
	s.prepareCarry(nsc, total)
	// sort nodesets by weight
	sort.Slice(nsc, func(i, j int) bool {
		return s.carrys[nsc[i].ID] > s.carrys[nsc[j].ID]
	})
	// pick the first nodeset than has N writable node
	for i := 0; i < nsc.Len(); i++ {
		ns = nsc[i]
		if ns.canWriteFor(s.nodeType, int(replicaNum)) && !containsID(excludeNodeSets, ns.ID) {
			if i != 0 {
				nsc[i], nsc[0] = nsc[0], nsc[i]
			}
			break
		}
	}
	// increase weight of other nodesets
	for i := 1; i < nsc.Len(); i++ {
		nset := nsc[i]
		weight := float64(nset.getTotalAvailableSpaceOf(s.nodeType)) / float64(total)
		s.carrys[nset.ID] += weight
		// limit the max value of weight
		if s.carrys[nset.ID] > 10.0 {
			s.carrys[nset.ID] = 10.0
		}
	}
	if ns != nil {
		// deerase nodeset weight
		s.carrys[ns.ID] -= 1.0
		if s.carrys[ns.ID] < 0 {
			s.carrys[ns.ID] = 0
		}
		return
	}
	switch s.nodeType {
	case DataNodeType:
		err = errors.NewError(proto.ErrNoNodeSetToCreateDataPartition)
	case MetaNodeType:
		err = errors.NewError(proto.ErrNoNodeSetToCreateMetaPartition)
	default:
		panic("unknow node type")
	}
	return
}

func NewCarryWeightNodesetSelector(nodeType NodeType) *CarryWeightNodesetSelector {
	return &CarryWeightNodesetSelector{
		carrys:   make(map[uint64]float64),
		nodeType: nodeType,
	}
}

type AvailableSpaceFirstNodesetSelector struct {
	nodeType NodeType
}

func (s *AvailableSpaceFirstNodesetSelector) GetName() string {
	return AvailableSpaceFirstNodesetSelectorName
}

func (s *AvailableSpaceFirstNodesetSelector) Select(nsc nodeSetCollection, excludeNodeSets []uint64, replicaNum uint8) (ns *nodeSet, err error) {
	// sort nodesets by available space
	sort.Slice(nsc, func(i, j int) bool {
		return nsc[i].getTotalAvailableSpaceOf(s.nodeType) > nsc[j].getTotalAvailableSpaceOf(s.nodeType)
	})
	// pick the first nodeset that has N writable nodes
	for i := 0; i < nsc.Len(); i++ {
		ns = nsc[i]
		if ns.canWriteFor(s.nodeType, int(replicaNum)) && !containsID(excludeNodeSets, ns.ID) {
			return
		}
	}
	switch s.nodeType {
	case DataNodeType:
		err = errors.NewError(proto.ErrNoNodeSetToCreateDataPartition)
	case MetaNodeType:
		err = errors.NewError(proto.ErrNoNodeSetToCreateMetaPartition)
	default:
		panic("unknow node type")
	}
	return
}

func NewAvailableSpaceFirstNodesetSelector(nodeType NodeType) *AvailableSpaceFirstNodesetSelector {
	return &AvailableSpaceFirstNodesetSelector{
		nodeType: nodeType,
	}
}

type TicketNodesetSelector struct {
	nodeType NodeType
	random   *rand.Rand
}

func (s *TicketNodesetSelector) GetName() string {
	return TicketNodesetSelectorName
}

func (s *TicketNodesetSelector) GetTicket(nsc nodeSetCollection, excludeNodeSets []uint64, replicaNum uint8) uint64 {
	total := uint64(0)
	for i := 0; i < len(nsc); i++ {
		nset := nsc[i]
		if nset.canWriteFor(s.nodeType, int(replicaNum)) && !containsID(excludeNodeSets, nset.ID) {
			total += nset.getTotalAvailableSpaceOf(s.nodeType)
		}
	}
	ticket := uint64(0)
	if total != 0 {
		ticket = s.random.Uint64() % total
	}
	return ticket
}

func (s *TicketNodesetSelector) GetNodesetByTicket(ticket uint64, nsc nodeSetCollection, excludeNodeSets []uint64, replicaNum uint8) (ns *nodeSet) {
	total := uint64(0)
	for i := 0; i < len(nsc); i++ {
		nset := nsc[i]
		if nset.canWriteFor(s.nodeType, int(replicaNum)) && !containsID(excludeNodeSets, nset.ID) {
			total += nset.getTotalAvailableSpaceOf(s.nodeType)
			if ticket <= total {
				ns = nset
				return
			}
		}
	}
	return
}

func (s *TicketNodesetSelector) Select(nsc nodeSetCollection, excludeNodeSets []uint64, replicaNum uint8) (ns *nodeSet, err error) {
	// sort nodesets by id, so we can get a node list that is as stable as possible
	sort.Slice(nsc, func(i, j int) bool {
		return nsc[i].ID < nsc[j].ID
	})
	ticket := s.GetTicket(nsc, excludeNodeSets, replicaNum)
	ns = s.GetNodesetByTicket(ticket, nsc, excludeNodeSets, replicaNum)
	if ns != nil {
		return
	}
	switch s.nodeType {
	case DataNodeType:
		err = errors.NewError(proto.ErrNoNodeSetToCreateDataPartition)
	case MetaNodeType:
		err = errors.NewError(proto.ErrNoNodeSetToCreateMetaPartition)
	default:
		panic("unknow node type")
	}
	return
}

func NewTicketNodesetSelector(nodeType NodeType) *TicketNodesetSelector {
	return &TicketNodesetSelector{
		nodeType: nodeType,
		random:   rand.New(rand.NewSource(time.Now().Unix())),
	}
}

func NewNodesetSelector(name string, nodeType NodeType) NodesetSelector {
	switch name {
	case CarryWeightNodesetSelectorName:
		return NewCarryWeightNodesetSelector(nodeType)
	case RoundRobinNodesetSelectorName:
		return NewRoundRobinNodesetSelector(nodeType)
	case TicketNodesetSelectorName:
		return NewTicketNodesetSelector(nodeType)
	case AvailableSpaceFirstNodesetSelectorName:
		return NewAvailableSpaceFirstNodesetSelector(nodeType)
	}
	return NewRoundRobinNodesetSelector(nodeType)
}

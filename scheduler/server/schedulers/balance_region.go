// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	"github.com/pingcap-incubator/tinykv/scheduler/server/core"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule/operator"
	"github.com/pingcap-incubator/tinykv/scheduler/server/schedule/opt"
	"sort"
)

func init() {
	schedule.RegisterSliceDecoderBuilder("balance-region", func(args []string) schedule.ConfigDecoder {
		return func(v interface{}) error {
			return nil
		}
	})
	schedule.RegisterScheduler("balance-region", func(opController *schedule.OperatorController, storage *core.Storage, decoder schedule.ConfigDecoder) (schedule.Scheduler, error) {
		return newBalanceRegionScheduler(opController), nil
	})
}

const (
	// balanceRegionRetryLimit is the limit to retry schedule for selected store.
	balanceRegionRetryLimit = 10
	balanceRegionName       = "balance-region-scheduler"
)

type balanceRegionScheduler struct {
	*baseScheduler
	name         string
	opController *schedule.OperatorController
}

// newBalanceRegionScheduler creates a scheduler that tends to keep regions on
// each store balanced.
func newBalanceRegionScheduler(opController *schedule.OperatorController, opts ...BalanceRegionCreateOption) schedule.Scheduler {
	base := newBaseScheduler(opController)
	s := &balanceRegionScheduler{
		baseScheduler: base,
		opController:  opController,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// BalanceRegionCreateOption is used to create a scheduler with an option.
type BalanceRegionCreateOption func(s *balanceRegionScheduler)

func (s *balanceRegionScheduler) GetName() string {
	if s.name != "" {
		return s.name
	}
	return balanceRegionName
}

func (s *balanceRegionScheduler) GetType() string {
	return "balance-region"
}

func (s *balanceRegionScheduler) IsScheduleAllowed(cluster opt.Cluster) bool {
	return s.opController.OperatorCount(operator.OpRegion) < cluster.GetRegionScheduleLimit()
}

func (s *balanceRegionScheduler) Schedule(cluster opt.Cluster) *operator.Operator {
	// Your Code Here (3C).
	stores, filtered := cluster.GetStores(), make([]*core.StoreInfo, 0)
	// Filter suitable stores
	// A suitable store should be up, the down time cannot
	// be longer than `MaxStoreDownTime` of the cluster
	for _, store := range stores {
		downTime, maxStoreDownTime := store.DownTime(), cluster.GetMaxStoreDownTime()
		if store.IsUp() && downTime.Milliseconds() <= maxStoreDownTime.Milliseconds() {
			filtered = append(filtered, store)
		}
	}
	stores = filtered
	// Sort according to their region size.
	sort.Slice(stores, func(i, j int) bool {
		return stores[i].GetRegionSize() > stores[j].GetRegionSize()
	})
	// Try to find regions to move from
	// the store with the biggest region size.
	var (
		result *core.RegionInfo
		source *core.StoreInfo
	)
	isLeader := false
	for _, store := range stores {
		// Try to select a pending region as
		// pending may mean the disk is overloaded.
		cluster.GetPendingRegionsWithLock(store.GetID(), func(rc core.RegionsContainer) {
			result = rc.RandomRegion([]byte(""), []byte(""))
		})
		if result != nil {
			source, isLeader = store, false
			break
		}
		// Try follower, leader, if all failed try next store
		cluster.GetFollowersWithLock(store.GetID(), func(rc core.RegionsContainer) {
			result = rc.RandomRegion([]byte(""), []byte(""))
		})
		if result != nil {
			source, isLeader = store, false
			break
		}
		cluster.GetLeadersWithLock(store.GetID(), func(rc core.RegionsContainer) {
			result = rc.RandomRegion([]byte(""), []byte(""))
		})
		if result != nil {
			source, isLeader = store, true
			break
		}
	}
	if result == nil {
		return nil
	}
	// Select a target store, with the smallest region size
	resultStores := make(map[uint64]struct{})
	for _, pr := range result.GetMeta().Peers {
		resultStores[pr.StoreId] = struct{}{}
	}
	if len(resultStores) != cluster.GetMaxReplicas() {
		// No need to balance region
		return nil
	}
	var target *core.StoreInfo
	for i := len(stores) - 1; i >= 0; i-- {
		// If result region has the store, skip
		if _, ok := resultStores[stores[i].GetID()]; !ok {
			target = stores[i]
			break
		}
	}
	if target == nil {
		// The region has all stores, no need to move
		return nil
	}
	// Judge whether this movement is valuable, difference is bigger than two times the approximate size of the region
	if source.GetRegionSize() > target.GetRegionSize()+result.GetApproximateSize()*2 {
		newPeer, err := cluster.AllocPeer(target.GetID())
		if err != nil {
			panic(err.Error())
		}
		kind := operator.OpRegion | operator.OpBalance
		if isLeader {
			kind |= operator.OpLeader
		}
		op, err := operator.CreateMovePeerOperator("balance-region", cluster, result, kind, source.GetID(), newPeer.StoreId, newPeer.Id)
		if err != nil {
			panic(err.Error())
		}
		return op
	}
	return nil
	return nil
}

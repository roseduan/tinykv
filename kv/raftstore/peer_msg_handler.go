package raftstore

import (
	"fmt"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	"github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
	"github.com/pingcap-incubator/tinykv/raft"
	"time"

	"github.com/Connor1996/badger/y"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/message"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/runner"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/snap"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/util"
	"github.com/pingcap-incubator/tinykv/log"
	"github.com/pingcap-incubator/tinykv/proto/pkg/metapb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/raft_cmdpb"
	rspb "github.com/pingcap-incubator/tinykv/proto/pkg/raft_serverpb"
	"github.com/pingcap-incubator/tinykv/scheduler/pkg/btree"
	"github.com/pingcap/errors"
)

type PeerTick int

const (
	PeerTickRaft               PeerTick = 0
	PeerTickRaftLogGC          PeerTick = 1
	PeerTickSplitRegionCheck   PeerTick = 2
	PeerTickSchedulerHeartbeat PeerTick = 3
)

type peerMsgHandler struct {
	*peer
	ctx *GlobalContext
}

func newPeerMsgHandler(peer *peer, ctx *GlobalContext) *peerMsgHandler {
	return &peerMsgHandler{
		peer: peer,
		ctx:  ctx,
	}
}

func (d *peerMsgHandler) HandleRaftReady() {
	if d.stopped {
		return
	}
	// Your Code Here (2B).
	// 没有 ready 的数据
	if !d.RaftGroup.HasReady() {
		return
	}

	ready := d.RaftGroup.Ready()

	// 保存已经 Ready 的数据
	result, err := d.peerStorage.SaveReadyState(&ready)
	if err != nil {
		log.Fatalf("save ready state err.[%+v]", err)
	}
	if result != nil {
		region := d.Region()
		d.updateStoreMeta(region)
		d.peerCache = make(map[uint64]*metapb.Peer)
		for _, pr := range region.Peers {
			d.insertPeerCache(pr)
		}
	}

	// 向其他节点发送已经 Ready 的消息
	d.Send(d.ctx.trans, ready.Messages)

	// 处理已经提交的日志，应用到状态机
	destoryed := d.applyCommittedEntries(&ready)
	// 节点已经被移除，直接返回
	if destoryed {
		return
	}

	// 保存applyState状态
	d.peerStorage.saveApplyState()

	// 告知 Ready 已经处理完毕
	d.RaftGroup.Advance(ready)
}

func (d *peerMsgHandler) updateStoreMeta(region *metapb.Region) {
	d.ctx.storeMeta.Lock()
	defer d.ctx.storeMeta.Unlock()
	d.ctx.storeMeta.regions[d.regionId] = region
	d.ctx.storeMeta.regionRanges.ReplaceOrInsert(&regionItem{region: region})
}

func (d *peerMsgHandler) applyCommittedEntries(ready *raft.Ready) bool {
	for _, ent := range ready.CommittedEntries {
		d.peerStorage.applyState.AppliedIndex = ent.Index
		if ent.Data == nil {
			continue
		}

		matchedPro := d.findMatchedPro(&ent)
		switch ent.EntryType {
		case eraftpb.EntryType_EntryNormal:
			d.handleNormalEntry(matchedPro, ent)
		case eraftpb.EntryType_EntryConfChange:
			if d.handleConfChangeEntry(matchedPro, ent) {
				return true
			}
		default:
			panic("unknown entry type")
		}
	}
	return false
}

func (d *peerMsgHandler) findMatchedPro(ent *eraftpb.Entry) *proposal {
	var matchedPro *proposal
	for i, pro := range d.proposals {
		if ent.Index == pro.index {
			if pro.term < d.Term() {
				pro.cb.Done(ErrRespStaleCommand(d.Term()))
			} else {
				matchedPro = pro
			}
			d.proposals = d.proposals[i+1:]
			break
		}
	}
	return matchedPro
}

func (d *peerMsgHandler) HandleMsg(msg message.Msg) {
	switch msg.Type {
	case message.MsgTypeRaftMessage:
		raftMsg := msg.Data.(*rspb.RaftMessage)
		if err := d.onRaftMsg(raftMsg); err != nil {
			log.Errorf("%s handle raft message error %v", d.Tag, err)
		}
	case message.MsgTypeRaftCmd:
		raftCMD := msg.Data.(*message.MsgRaftCmd)
		d.proposeRaftCommand(raftCMD.Request, raftCMD.Callback)
	case message.MsgTypeTick:
		d.onTick()
	case message.MsgTypeSplitRegion:
		split := msg.Data.(*message.MsgSplitRegion)
		log.Infof("%s on split with %v", d.Tag, split.SplitKey)
		d.onPrepareSplitRegion(split.RegionEpoch, split.SplitKey, split.Callback)
	case message.MsgTypeRegionApproximateSize:
		d.onApproximateRegionSize(msg.Data.(uint64))
	case message.MsgTypeGcSnap:
		gcSnap := msg.Data.(*message.MsgGCSnap)
		d.onGCSnap(gcSnap.Snaps)
	case message.MsgTypeStart:
		d.startTicker()
	}
}

func (d *peerMsgHandler) preProposeRaftCommand(req *raft_cmdpb.RaftCmdRequest) error {
	// Check store_id, make sure that the msg is dispatched to the right place.
	if err := util.CheckStoreID(req, d.storeID()); err != nil {
		return err
	}

	// Check whether the store has the right peer to handle the request.
	regionID := d.regionId
	leaderID := d.LeaderId()
	if !d.IsLeader() {
		leader := d.getPeerFromCache(leaderID)
		return &util.ErrNotLeader{RegionId: regionID, Leader: leader}
	}
	// peer_id must be the same as peer's.
	if err := util.CheckPeerID(req, d.PeerId()); err != nil {
		return err
	}
	// Check whether the term is stale.
	if err := util.CheckTerm(req, d.Term()); err != nil {
		return err
	}
	err := util.CheckRegionEpoch(req, d.Region(), true)
	if errEpochNotMatching, ok := err.(*util.ErrEpochNotMatch); ok {
		// Attach the region which might be split from the current region. But it doesn't
		// matter if the region is not split from the current region. If the region meta
		// received by the TiKV driver is newer than the meta cached in the driver, the meta is
		// updated.
		siblingRegion := d.findSiblingRegion()
		if siblingRegion != nil {
			errEpochNotMatching.Regions = append(errEpochNotMatching.Regions, siblingRegion)
		}
		return errEpochNotMatching
	}
	return err
}

func (d *peerMsgHandler) proposeRaftCommand(msg *raft_cmdpb.RaftCmdRequest, cb *message.Callback) {
	err := d.preProposeRaftCommand(msg)
	if err != nil {
		cb.Done(ErrResp(err))
		return
	}
	// Your Code Here (2B).
	buf, err := msg.Marshal()
	if err != nil {
		panic(fmt.Sprintf("raft msg marshal err.[%+v]", err))
	}

	// 记录消息的回调处理函数(在 HandleRaftReady 中进行处理)
	d.proposals = append(d.proposals, &proposal{
		index: d.nextProposalIndex(),
		term:  d.Term(),
		cb:    cb,
	})

	// 配置变更消息
	if msg.AdminRequest != nil && msg.AdminRequest.CmdType == raft_cmdpb.AdminCmdType_ChangePeer {
		adminReq := msg.AdminRequest
		var skip bool
		if adminReq.ChangePeer.ChangeType == eraftpb.ConfChangeType_AddNode {
			if peer := util.FindPeer(d.Region(), adminReq.ChangePeer.Peer.StoreId); peer != nil {
				skip = true
			}
		}
		if adminReq.ChangePeer.ChangeType == eraftpb.ConfChangeType_RemoveNode {
			if peer := util.FindPeer(d.Region(), adminReq.ChangePeer.Peer.StoreId); peer == nil {
				skip = true
			}
		}
		if skip {
			resp := newCmdResp()
			resp.AdminResponse = &raft_cmdpb.AdminResponse{}
			d.proposals = d.proposals[:len(d.proposals)-1]
			cb.Done(resp)
			return
		}

		req := adminReq.ChangePeer
		cc := eraftpb.ConfChange{
			ChangeType: req.ChangeType,
			NodeId:     req.Peer.Id,
			Context:    buf,
		}
		if err := d.RaftGroup.ProposeConfChange(cc); err != nil {
			log.Fatalf("propose conf change err.[%+v]", err)
		}
	} else {
		// 处理普通日志消息
		if err = d.RaftGroup.Propose(buf); err != nil {
			log.Fatalf("raft proprose msg err.[%+v]", err)
		}
	}
}

func (d *peerMsgHandler) onTick() {
	if d.stopped {
		return
	}
	d.ticker.tickClock()
	if d.ticker.isOnTick(PeerTickRaft) {
		d.onRaftBaseTick()
	}
	if d.ticker.isOnTick(PeerTickRaftLogGC) {
		d.onRaftGCLogTick()
	}
	if d.ticker.isOnTick(PeerTickSchedulerHeartbeat) {
		d.onSchedulerHeartbeatTick()
	}
	if d.ticker.isOnTick(PeerTickSplitRegionCheck) {
		d.onSplitRegionCheckTick()
	}
	d.ctx.tickDriverSender <- d.regionId
}

func (d *peerMsgHandler) startTicker() {
	d.ticker = newTicker(d.regionId, d.ctx.cfg)
	d.ctx.tickDriverSender <- d.regionId
	d.ticker.schedule(PeerTickRaft)
	d.ticker.schedule(PeerTickRaftLogGC)
	d.ticker.schedule(PeerTickSplitRegionCheck)
	d.ticker.schedule(PeerTickSchedulerHeartbeat)
}

func (d *peerMsgHandler) onRaftBaseTick() {
	d.RaftGroup.Tick()
	d.ticker.schedule(PeerTickRaft)
}

func (d *peerMsgHandler) ScheduleCompactLog(truncatedIndex uint64) {
	raftLogGCTask := &runner.RaftLogGCTask{
		RaftEngine: d.ctx.engine.Raft,
		RegionID:   d.regionId,
		StartIdx:   d.LastCompactedIdx,
		EndIdx:     truncatedIndex + 1,
	}
	d.LastCompactedIdx = raftLogGCTask.EndIdx
	d.ctx.raftLogGCTaskSender <- raftLogGCTask
}

func (d *peerMsgHandler) onRaftMsg(msg *rspb.RaftMessage) error {
	log.Debugf("%s handle raft message %s from %d to %d",
		d.Tag, msg.GetMessage().GetMsgType(), msg.GetFromPeer().GetId(), msg.GetToPeer().GetId())
	if !d.validateRaftMessage(msg) {
		return nil
	}
	if d.stopped {
		return nil
	}
	if msg.GetIsTombstone() {
		// we receive a message tells us to remove self.
		d.handleGCPeerMsg(msg)
		return nil
	}
	if d.checkMessage(msg) {
		return nil
	}
	key, err := d.checkSnapshot(msg)
	if err != nil {
		return err
	}
	if key != nil {
		// If the snapshot file is not used again, then it's OK to
		// delete them here. If the snapshot file will be reused when
		// receiving, then it will fail to pass the check again, so
		// missing snapshot files should not be noticed.
		s, err1 := d.ctx.snapMgr.GetSnapshotForApplying(*key)
		if err1 != nil {
			return err1
		}
		d.ctx.snapMgr.DeleteSnapshot(*key, s, false)
		return nil
	}
	d.insertPeerCache(msg.GetFromPeer())
	err = d.RaftGroup.Step(*msg.GetMessage())
	if err != nil {
		return err
	}
	if d.AnyNewPeerCatchUp(msg.FromPeer.Id) {
		d.HeartbeatScheduler(d.ctx.schedulerTaskSender)
	}
	return nil
}

// return false means the message is invalid, and can be ignored.
func (d *peerMsgHandler) validateRaftMessage(msg *rspb.RaftMessage) bool {
	regionID := msg.GetRegionId()
	from := msg.GetFromPeer()
	to := msg.GetToPeer()
	log.Debugf("[region %d] handle raft message %s from %d to %d", regionID, msg, from.GetId(), to.GetId())
	if to.GetStoreId() != d.storeID() {
		log.Warnf("[region %d] store not match, to store id %d, mine %d, ignore it",
			regionID, to.GetStoreId(), d.storeID())
		return false
	}
	if msg.RegionEpoch == nil {
		log.Errorf("[region %d] missing epoch in raft message, ignore it", regionID)
		return false
	}
	return true
}

/// Checks if the message is sent to the correct peer.
///
/// Returns true means that the message can be dropped silently.
func (d *peerMsgHandler) checkMessage(msg *rspb.RaftMessage) bool {
	fromEpoch := msg.GetRegionEpoch()
	isVoteMsg := util.IsVoteMessage(msg.Message)
	fromStoreID := msg.FromPeer.GetStoreId()

	// Let's consider following cases with three nodes [1, 2, 3] and 1 is leader:
	// a. 1 removes 2, 2 may still send MsgAppendResponse to 1.
	//  We should ignore this stale message and let 2 remove itself after
	//  applying the ConfChange log.
	// b. 2 is isolated, 1 removes 2. When 2 rejoins the cluster, 2 will
	//  send stale MsgRequestVote to 1 and 3, at this time, we should tell 2 to gc itself.
	// c. 2 is isolated but can communicate with 3. 1 removes 3.
	//  2 will send stale MsgRequestVote to 3, 3 should ignore this message.
	// d. 2 is isolated but can communicate with 3. 1 removes 2, then adds 4, remove 3.
	//  2 will send stale MsgRequestVote to 3, 3 should tell 2 to gc itself.
	// e. 2 is isolated. 1 adds 4, 5, 6, removes 3, 1. Now assume 4 is leader.
	//  After 2 rejoins the cluster, 2 may send stale MsgRequestVote to 1 and 3,
	//  1 and 3 will ignore this message. Later 4 will send messages to 2 and 2 will
	//  rejoin the raft group again.
	// f. 2 is isolated. 1 adds 4, 5, 6, removes 3, 1. Now assume 4 is leader, and 4 removes 2.
	//  unlike case e, 2 will be stale forever.
	// TODO: for case f, if 2 is stale for a long time, 2 will communicate with scheduler and scheduler will
	// tell 2 is stale, so 2 can remove itself.
	region := d.Region()
	if util.IsEpochStale(fromEpoch, region.RegionEpoch) && util.FindPeer(region, fromStoreID) == nil {
		// The message is stale and not in current region.
		handleStaleMsg(d.ctx.trans, msg, region.RegionEpoch, isVoteMsg)
		return true
	}
	target := msg.GetToPeer()
	if target.Id < d.PeerId() {
		log.Infof("%s target peer ID %d is less than %d, msg maybe stale", d.Tag, target.Id, d.PeerId())
		return true
	} else if target.Id > d.PeerId() {
		if d.MaybeDestroy() {
			log.Infof("%s is stale as received a larger peer %s, destroying", d.Tag, target)
			d.destroyPeer()
			d.ctx.router.sendStore(message.NewMsg(message.MsgTypeStoreRaftMessage, msg))
		}
		return true
	}
	return false
}

func handleStaleMsg(trans Transport, msg *rspb.RaftMessage, curEpoch *metapb.RegionEpoch,
	needGC bool) {
	regionID := msg.RegionId
	fromPeer := msg.FromPeer
	toPeer := msg.ToPeer
	msgType := msg.Message.GetMsgType()

	if !needGC {
		log.Infof("[region %d] raft message %s is stale, current %v ignore it",
			regionID, msgType, curEpoch)
		return
	}
	gcMsg := &rspb.RaftMessage{
		RegionId:    regionID,
		FromPeer:    toPeer,
		ToPeer:      fromPeer,
		RegionEpoch: curEpoch,
		IsTombstone: true,
	}
	if err := trans.Send(gcMsg); err != nil {
		log.Errorf("[region %d] send message failed %v", regionID, err)
	}
}

func (d *peerMsgHandler) handleGCPeerMsg(msg *rspb.RaftMessage) {
	fromEpoch := msg.RegionEpoch
	if !util.IsEpochStale(d.Region().RegionEpoch, fromEpoch) {
		return
	}
	if !util.PeerEqual(d.Meta, msg.ToPeer) {
		log.Infof("%s receive stale gc msg, ignore", d.Tag)
		return
	}
	log.Infof("%s peer %s receives gc message, trying to remove", d.Tag, msg.ToPeer)
	if d.MaybeDestroy() {
		d.destroyPeer()
	}
}

// Returns `None` if the `msg` doesn't contain a snapshot or it contains a snapshot which
// doesn't conflict with any other snapshots or regions. Otherwise a `snap.SnapKey` is returned.
func (d *peerMsgHandler) checkSnapshot(msg *rspb.RaftMessage) (*snap.SnapKey, error) {
	if msg.Message.Snapshot == nil {
		return nil, nil
	}
	regionID := msg.RegionId
	snapshot := msg.Message.Snapshot
	key := snap.SnapKeyFromRegionSnap(regionID, snapshot)
	snapData := new(rspb.RaftSnapshotData)
	err := snapData.Unmarshal(snapshot.Data)
	if err != nil {
		return nil, err
	}
	snapRegion := snapData.Region
	peerID := msg.ToPeer.Id
	var contains bool
	for _, peer := range snapRegion.Peers {
		if peer.Id == peerID {
			contains = true
			break
		}
	}
	if !contains {
		log.Infof("%s %s doesn't contains peer %d, skip", d.Tag, snapRegion, peerID)
		return &key, nil
	}
	meta := d.ctx.storeMeta
	meta.Lock()
	defer meta.Unlock()
	if !util.RegionEqual(meta.regions[d.regionId], d.Region()) {
		if !d.isInitialized() {
			log.Infof("%s stale delegate detected, skip", d.Tag)
			return &key, nil
		} else {
			panic(fmt.Sprintf("%s meta corrupted %s != %s", d.Tag, meta.regions[d.regionId], d.Region()))
		}
	}

	existRegions := meta.getOverlapRegions(snapRegion)
	for _, existRegion := range existRegions {
		if existRegion.GetId() == snapRegion.GetId() {
			continue
		}
		log.Infof("%s region overlapped %s %s", d.Tag, existRegion, snapRegion)
		return &key, nil
	}

	// check if snapshot file exists.
	_, err = d.ctx.snapMgr.GetSnapshotForApplying(key)
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (d *peerMsgHandler) destroyPeer() {
	log.Infof("%s starts destroy", d.Tag)
	regionID := d.regionId
	// We can't destroy a peer which is applying snapshot.
	meta := d.ctx.storeMeta
	meta.Lock()
	defer meta.Unlock()
	isInitialized := d.isInitialized()
	if err := d.Destroy(d.ctx.engine, false); err != nil {
		// If not panic here, the peer will be recreated in the next restart,
		// then it will be gc again. But if some overlap region is created
		// before restarting, the gc action will delete the overlap region's
		// data too.
		panic(fmt.Sprintf("%s destroy peer %v", d.Tag, err))
	}
	d.ctx.router.close(regionID)
	d.stopped = true
	if isInitialized && meta.regionRanges.Delete(&regionItem{region: d.Region()}) == nil {
		panic(d.Tag + " meta corruption detected")
	}
	if _, ok := meta.regions[regionID]; !ok {
		panic(d.Tag + " meta corruption detected")
	}
	delete(meta.regions, regionID)
}

func (d *peerMsgHandler) findSiblingRegion() (result *metapb.Region) {
	meta := d.ctx.storeMeta
	meta.RLock()
	defer meta.RUnlock()
	item := &regionItem{region: d.Region()}
	meta.regionRanges.AscendGreaterOrEqual(item, func(i btree.Item) bool {
		result = i.(*regionItem).region
		return true
	})
	return
}

func (d *peerMsgHandler) onRaftGCLogTick() {
	d.ticker.schedule(PeerTickRaftLogGC)
	if !d.IsLeader() {
		return
	}

	appliedIdx := d.peerStorage.AppliedIndex()
	firstIdx, _ := d.peerStorage.FirstIndex()
	var compactIdx uint64
	if appliedIdx > firstIdx && appliedIdx-firstIdx >= d.ctx.cfg.RaftLogGcCountLimit {
		compactIdx = appliedIdx
	} else {
		return
	}

	y.Assert(compactIdx > 0)
	compactIdx -= 1
	if compactIdx < firstIdx {
		// In case compact_idx == first_idx before subtraction.
		return
	}

	term, err := d.RaftGroup.Raft.RaftLog.Term(compactIdx)
	if err != nil {
		log.Fatalf("appliedIdx: %d, firstIdx: %d, compactIdx: %d", appliedIdx, firstIdx, compactIdx)
		panic(err)
	}

	// Create a compact log request and notify directly.
	regionID := d.regionId
	request := newCompactLogRequest(regionID, d.Meta, compactIdx, term)
	d.proposeRaftCommand(request, nil)
}

func (d *peerMsgHandler) onSplitRegionCheckTick() {
	d.ticker.schedule(PeerTickSplitRegionCheck)
	// To avoid frequent scan, we only add new scan tasks if all previous tasks
	// have finished.
	if len(d.ctx.splitCheckTaskSender) > 0 {
		return
	}

	if !d.IsLeader() {
		return
	}
	if d.ApproximateSize != nil && d.SizeDiffHint < d.ctx.cfg.RegionSplitSize/8 {
		return
	}
	d.ctx.splitCheckTaskSender <- &runner.SplitCheckTask{
		Region: d.Region(),
	}
	d.SizeDiffHint = 0
}

func (d *peerMsgHandler) onPrepareSplitRegion(regionEpoch *metapb.RegionEpoch, splitKey []byte, cb *message.Callback) {
	if err := d.validateSplitRegion(regionEpoch, splitKey); err != nil {
		cb.Done(ErrResp(err))
		return
	}
	region := d.Region()
	d.ctx.schedulerTaskSender <- &runner.SchedulerAskSplitTask{
		Region:   region,
		SplitKey: splitKey,
		Peer:     d.Meta,
		Callback: cb,
	}
}

func (d *peerMsgHandler) validateSplitRegion(epoch *metapb.RegionEpoch, splitKey []byte) error {
	if len(splitKey) == 0 {
		err := errors.Errorf("%s split key should not be empty", d.Tag)
		log.Error(err)
		return err
	}

	if !d.IsLeader() {
		// region on this store is no longer leader, skipped.
		log.Infof("%s not leader, skip", d.Tag)
		return &util.ErrNotLeader{
			RegionId: d.regionId,
			Leader:   d.getPeerFromCache(d.LeaderId()),
		}
	}

	region := d.Region()
	latestEpoch := region.GetRegionEpoch()

	// This is a little difference for `check_region_epoch` in region split case.
	// Here we just need to check `version` because `conf_ver` will be update
	// to the latest value of the peer, and then send to Scheduler.
	if latestEpoch.Version != epoch.Version {
		log.Infof("%s epoch changed, retry later, prev_epoch: %s, epoch %s",
			d.Tag, latestEpoch, epoch)
		return &util.ErrEpochNotMatch{
			Message: fmt.Sprintf("%s epoch changed %s != %s, retry later", d.Tag, latestEpoch, epoch),
			Regions: []*metapb.Region{region},
		}
	}
	return nil
}

func (d *peerMsgHandler) onApproximateRegionSize(size uint64) {
	d.ApproximateSize = &size
}

func (d *peerMsgHandler) onSchedulerHeartbeatTick() {
	d.ticker.schedule(PeerTickSchedulerHeartbeat)

	if !d.IsLeader() {
		return
	}
	d.HeartbeatScheduler(d.ctx.schedulerTaskSender)
}

func (d *peerMsgHandler) onGCSnap(snaps []snap.SnapKeyWithSending) {
	compactedIdx := d.peerStorage.truncatedIndex()
	compactedTerm := d.peerStorage.truncatedTerm()
	for _, snapKeyWithSending := range snaps {
		key := snapKeyWithSending.SnapKey
		if snapKeyWithSending.IsSending {
			snap, err := d.ctx.snapMgr.GetSnapshotForSending(key)
			if err != nil {
				log.Errorf("%s failed to load snapshot for %s %v", d.Tag, key, err)
				continue
			}
			if key.Term < compactedTerm || key.Index < compactedIdx {
				log.Infof("%s snap file %s has been compacted, delete", d.Tag, key)
				d.ctx.snapMgr.DeleteSnapshot(key, snap, false)
			} else if fi, err1 := snap.Meta(); err1 == nil {
				modTime := fi.ModTime()
				if time.Since(modTime) > 4*time.Hour {
					log.Infof("%s snap file %s has been expired, delete", d.Tag, key)
					d.ctx.snapMgr.DeleteSnapshot(key, snap, false)
				}
			}
		} else if key.Term <= compactedTerm &&
			(key.Index < compactedIdx || key.Index == compactedIdx) {
			log.Infof("%s snap file %s has been applied, delete", d.Tag, key)
			a, err := d.ctx.snapMgr.GetSnapshotForApplying(key)
			if err != nil {
				log.Errorf("%s failed to load snapshot for %s %v", d.Tag, key, err)
				continue
			}
			d.ctx.snapMgr.DeleteSnapshot(key, a, false)
		}
	}
}

func (d *peerMsgHandler) handleNormalEntry(pro *proposal, ent eraftpb.Entry) {
	resp := newCmdResp()
	BindRespTerm(resp, d.Term())
	req := new(raft_cmdpb.RaftCmdRequest)
	if err := req.Unmarshal(ent.Data); err != nil {
		log.Fatalf("req unmarshal err.[%+v]", err)
	}

	region := d.Region()
	if err := util.CheckRegionEpoch(req, region, true); err != nil {
		if pro != nil {
			pro.cb.Done(ErrResp(err))
		}
		return
	}

	if req.AdminRequest != nil {
		d.handleAdminRequest(resp, req.AdminRequest, region, pro)
	}

	var needTxn bool
	var kvwb engine_util.WriteBatch
	checkInRegion := func(key []byte) (err error) {
		if err = util.CheckKeyInRegion(key, d.Region()); err != nil {
			if pro != nil {
				pro.cb.Done(ErrResp(err))
			}
		}
		return
	}
	for _, v := range req.Requests {
		switch v.CmdType {
		case raft_cmdpb.CmdType_Put:
			if err := checkInRegion(v.Put.Key); err != nil {
				return
			}
			kvwb.SetCF(v.Put.Cf, v.Put.Key, v.Put.Value)
			resp.Responses = append(resp.Responses, &raft_cmdpb.Response{
				CmdType: raft_cmdpb.CmdType_Put,
				Put:     &raft_cmdpb.PutResponse{},
			})
		case raft_cmdpb.CmdType_Get:
			if err := checkInRegion(v.Get.Key); err != nil {
				return
			}
			val, err := engine_util.GetCF(d.peerStorage.Engines.Kv, v.Get.Cf, v.Get.Key)
			if err != nil {
				if pro != nil {
					pro.cb.Done(ErrResp(err))
				}
				return
			}
			resp.Responses = append(resp.Responses, &raft_cmdpb.Response{
				CmdType: raft_cmdpb.CmdType_Get,
				Get:     &raft_cmdpb.GetResponse{Value: val},
			})
		case raft_cmdpb.CmdType_Delete:
			if err := checkInRegion(v.Delete.Key); err != nil {
				return
			}
			kvwb.DeleteCF(v.Delete.Cf, v.Delete.Key)
			resp.Responses = append(resp.Responses, &raft_cmdpb.Response{
				CmdType: raft_cmdpb.CmdType_Delete,
				Delete:  &raft_cmdpb.DeleteResponse{},
			})
		case raft_cmdpb.CmdType_Snap:
			if req.Header.RegionEpoch.Version != d.Region().RegionEpoch.Version {
				if pro != nil {
					pro.cb.Done(ErrResp(&util.ErrEpochNotMatch{}))
				}
				return
			}
			needTxn = true
			resp.Responses = append(resp.Responses, &raft_cmdpb.Response{
				CmdType: raft_cmdpb.CmdType_Snap,
				Snap:    &raft_cmdpb.SnapResponse{Region: d.Region()},
			})
		}
	}

	// 持久化
	if err := kvwb.WriteToDB(d.peerStorage.Engines.Kv); err != nil {
		if pro != nil {
			pro.cb.Done(ErrResp(err))
		}
		return
	}
	if needTxn && pro != nil {
		pro.cb.Txn = d.peerStorage.Engines.Kv.NewTransaction(false)
	}
	if pro != nil {
		pro.cb.Done(resp)
	}
}

func (d *peerMsgHandler) handleAdminRequest(resp *raft_cmdpb.RaftCmdResponse, adminReq *raft_cmdpb.AdminRequest, region *metapb.Region, pro *proposal) {
	resp.AdminResponse = &raft_cmdpb.AdminResponse{
		CmdType: adminReq.CmdType,
	}
	switch adminReq.CmdType {
	case raft_cmdpb.AdminCmdType_CompactLog:
		resp.AdminResponse.CompactLog = &raft_cmdpb.CompactLogResponse{}

		appliedIndex := d.peerStorage.applyState.AppliedIndex
		if appliedIndex < adminReq.CompactLog.CompactIndex {
			term := d.LogTerm(appliedIndex)
			adminReq.CompactLog.CompactIndex = appliedIndex
			adminReq.CompactLog.CompactTerm = term
		}

		if d.peerStorage.applyState.TruncatedState.Index < adminReq.CompactLog.CompactIndex {
			d.peerStorage.applyState.TruncatedState = &rspb.RaftTruncatedState{
				Index: adminReq.CompactLog.CompactIndex,
				Term:  adminReq.CompactLog.CompactTerm,
			}
			d.ScheduleCompactLog(adminReq.CompactLog.CompactIndex)
		}
	case raft_cmdpb.AdminCmdType_TransferLeader:
		resp.AdminResponse.TransferLeader = &raft_cmdpb.TransferLeaderResponse{}

		id := adminReq.TransferLeader.Peer.Id
		d.RaftGroup.TransferLeader(id)
	case raft_cmdpb.AdminCmdType_Split:
		if err := util.CheckKeyInRegion(adminReq.Split.SplitKey, region); err != nil {
			if pro != nil {
				pro.cb.Done(ErrResp(err))
				return
			}
		} else {

		}
	}
}

func (d *peerMsgHandler) handleConfChangeEntry(pro *proposal, ent eraftpb.Entry) bool {
	resp := newCmdResp()
	BindRespTerm(resp, d.Term())

	var (
		cc          = &eraftpb.ConfChange{}
		msg         = &raft_cmdpb.RaftCmdRequest{}
		regionLocal = &rspb.RegionLocalState{State: rspb.PeerState_Normal}
		newRegion   = &metapb.Region{}
		kvwb        = engine_util.WriteBatch{}
		removeSelf  bool
	)

	if err := cc.Unmarshal(ent.Data); err != nil {
		panic(err.Error())
	}
	if err := msg.Unmarshal(cc.Context); err != nil {
		panic(err.Error())
	}
	if cc.ChangeType == eraftpb.ConfChangeType_RemoveNode && cc.NodeId == d.PeerId() {
		removeSelf = true
	}

	_ = util.CloneMsg(d.Region(), newRegion)
	newRegion.RegionEpoch.ConfVer++

	switch cc.ChangeType {
	case eraftpb.ConfChangeType_AddNode:
		newRegion.Peers = append(newRegion.Peers, msg.AdminRequest.ChangePeer.Peer)
	case eraftpb.ConfChangeType_RemoveNode:
		util.RemovePeer(newRegion, msg.AdminRequest.ChangePeer.Peer.StoreId)
		d.removePeerCache(cc.NodeId)
	}

	// 保存 region 相关状态
	d.SetRegion(newRegion)
	d.ctx.storeMeta.Lock()
	d.ctx.storeMeta.regions[d.regionId] = newRegion
	d.ctx.storeMeta.Unlock()

	// 保存 region local state
	regionLocal.Region = newRegion
	if removeSelf {
		regionLocal.State = rspb.PeerState_Tombstone
	}
	d.peerStorage.saveRegionLocalState(regionLocal, &kvwb)

	// 发送到 raft 中
	d.RaftGroup.ApplyConfChange(*cc)

	if pro != nil {
		resp.AdminResponse = &raft_cmdpb.AdminResponse{
			CmdType:    raft_cmdpb.AdminCmdType_ChangePeer,
			ChangePeer: &raft_cmdpb.ChangePeerResponse{Region: newRegion},
		}
		pro.cb.Done(resp)
	}
	if removeSelf {
		d.destroyPeer()
		return true
	}
	return false
}

func (d *peerMsgHandler) handleSplitRegionCmd(resp *raft_cmdpb.RaftCmdResponse, adminReq *raft_cmdpb.AdminRequest, region *metapb.Region) {
	firstRegion, secondRegion := &metapb.Region{}, &metapb.Region{}
	util.CloneMsg(region, firstRegion)
	util.CloneMsg(region, secondRegion)

	firstRegion.EndKey = adminReq.Split.SplitKey
	firstRegion.RegionEpoch.Version++
	secondRegion.StartKey = adminReq.Split.SplitKey
	secondRegion.Id = adminReq.Split.NewRegionId
	secondRegion.RegionEpoch.Version++
	for i, v := range adminReq.Split.NewPeerIds {
		secondRegion.Peers[i].Id = v
	}

	d.SetRegion(firstRegion)
	d.ctx.storeMeta.Lock()
	d.ctx.storeMeta.regionRanges.Delete(&regionItem{region})
	d.ctx.storeMeta.regionRanges.ReplaceOrInsert(&regionItem{firstRegion})
	d.ctx.storeMeta.regionRanges.ReplaceOrInsert(&regionItem{secondRegion})
	d.ctx.storeMeta.regions[firstRegion.Id] = firstRegion
	d.ctx.storeMeta.regions[secondRegion.Id] = secondRegion
	d.ctx.storeMeta.Unlock()
	resp.AdminResponse.Split = &raft_cmdpb.SplitResponse{
		Regions: []*metapb.Region{firstRegion, secondRegion},
	}
	kvRegionWB := engine_util.WriteBatch{}
	d.peerStorage.saveRegionLocalState(&rspb.RegionLocalState{
		State:  rspb.PeerState_Normal,
		Region: firstRegion,
	}, &kvRegionWB)
	kvRegionWB.Reset()
	d.peerStorage.saveRegionLocalState(&rspb.RegionLocalState{
		State:  rspb.PeerState_Normal,
		Region: secondRegion,
	}, &kvRegionWB)
	d.SizeDiffHint = 0
	d.ApproximateSize = new(uint64)
	d.onReadySplitRegion(secondRegion)
}

func newAdminRequest(regionID uint64, peer *metapb.Peer) *raft_cmdpb.RaftCmdRequest {
	return &raft_cmdpb.RaftCmdRequest{
		Header: &raft_cmdpb.RaftRequestHeader{
			RegionId: regionID,
			Peer:     peer,
		},
	}
}

func newCompactLogRequest(regionID uint64, peer *metapb.Peer, compactIndex, compactTerm uint64) *raft_cmdpb.RaftCmdRequest {
	req := newAdminRequest(regionID, peer)
	req.AdminRequest = &raft_cmdpb.AdminRequest{
		CmdType: raft_cmdpb.AdminCmdType_CompactLog,
		CompactLog: &raft_cmdpb.CompactLogRequest{
			CompactIndex: compactIndex,
			CompactTerm:  compactTerm,
		},
	}
	return req
}

func (d *peerMsgHandler) onReadySplitRegion(second *metapb.Region) {
	peer, err := createPeer(d.storeID(), d.ctx.cfg, d.ctx.regionTaskSender, d.ctx.engine, second)
	if err != nil {
		panic(err.Error())
	}
	d.ctx.router.register(peer)
	d.ctx.router.send(second.Id, message.Msg{RegionID: second.Id, Type: message.MsgTypeStart})
	if d.IsLeader() {
		d.HeartbeatScheduler(d.ctx.schedulerTaskSender)
	}
}

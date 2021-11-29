// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"errors"
	"fmt"
	"github.com/pingcap-incubator/tinykv/log"
	"math/rand"
	"sort"
	"time"

	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// None is a placeholder node ID used when there is no leader.
const None uint64 = 0

// StateType represents the role of a node in a cluster.
type StateType uint64

const (
	StateFollower StateType = iota
	StateCandidate
	StateLeader
)

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
}

func (st StateType) String() string {
	return stmap[uint64(st)]
}

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
var ErrProposalDropped = errors.New("raft proposal dropped")

// Config contains the parameters to start a raft.
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.
	peers []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.
	ElectionTick int
	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.
	Storage Storage
	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.
	Applied uint64
}

func (c *Config) validate() error {
	if c.ID == None {
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	return nil
}

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
type Progress struct {
	Match, Next uint64
}

type Raft struct {
	id uint64

	Term uint64
	Vote uint64

	// the log
	RaftLog *RaftLog

	// log replication progress of each peers
	Prs map[uint64]*Progress

	// this peer's role
	State StateType

	// votes records
	votes map[uint64]bool

	// msgs need to send
	msgs []pb.Message

	// the leader id
	Lead uint64

	// heartbeat interval, should send
	heartbeatTimeout int
	// baseline of election interval
	electionTimeout int
	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.
	heartbeatElapsed int
	// Ticks since it reached last electionTimeout when it is leader or candidate.
	// Number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.
	electionElapsed int

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in section 3.10 of Raft phd thesis.
	// (https://web.stanford.edu/~ouster/cgi-bin/papers/OngaroPhD.pdf)
	// (Used in 3A leader transfer)
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via PendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.
	// (Used in 3A conf change)
	PendingConfIndex uint64
}

// newRaft return a raft peer with the given config
func newRaft(c *Config) *Raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	// Your Code Here (2A).

	hardState, confState, err := c.Storage.InitialState()
	if err != nil {
		panic(fmt.Sprintf("init state err: %+v", err))
	}
	if len(confState.Nodes) == 0 {
		confState.Nodes = c.peers
	}

	rand.Seed(time.Now().UnixNano())
	raft := &Raft{
		id:               c.ID,
		Term:             hardState.Term,
		Vote:             hardState.Vote,
		State:            StateFollower,
		electionTimeout:  c.ElectionTick * 2,
		electionElapsed:  rand.Intn(c.ElectionTick) + 1,
		heartbeatTimeout: c.HeartbeatTick,
		heartbeatElapsed: 0,
	}

	// 初始化日志复制进度
	raft.initProgress(confState.Nodes)

	// raft log
	raftLog := newLog(c.Storage)
	if hardState.Commit != 0 {
		raftLog.committed = hardState.Commit
	}
	if c.Applied != 0 {
		raftLog.applied = c.Applied
	}
	raft.RaftLog = raftLog

	//ret.updatePengingConfIdx() todo
	return raft
}

func (r *Raft) hardState() pb.HardState {
	return pb.HardState{
		Term:   r.Term,
		Vote:   r.Vote,
		Commit: r.RaftLog.committed,
	}
}

func (r *Raft) softState() *SoftState {
	return &SoftState{
		Lead:      r.Lead,
		RaftState: r.State,
	}
}

func (r *Raft) initProgress(peers []uint64) {
	prs := make(map[uint64]*Progress)
	for _, id := range peers {
		prs[id] = &Progress{}
	}
	r.Prs = prs
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer. Returns true if a message was sent.
func (r *Raft) sendAppend(to uint64) bool {
	// Your Code Here (2A).
	var msg pb.Message
	// 如果 Follower 节点的下一个需要发送的日志比leader的firstIndex还小，则直接发送快照信息
	if r.Prs[to].Next <= r.RaftLog.firstLogIndex {
		snapshot, err := r.RaftLog.storage.Snapshot()
		if err != nil {
			if err == ErrSnapshotTemporarilyUnavailable {
				log.Warnf("snapshot is not available")
				return true
			}
			panic(err)
		}
		msg = pb.Message{
			MsgType:  pb.MessageType_MsgSnapshot,
			From:     r.id,
			To:       to,
			Term:     r.Term,
			Snapshot: &snapshot,
		}
	} else {
		prevLogIndex := r.Prs[to].Next - 1
		prevLogTerm, err := r.RaftLog.Term(prevLogIndex)
		if err != nil {
			panic(err)
		}
		entries, err := r.RaftLog.Entries(r.Prs[to].Next, r.RaftLog.LastIndex()+1)
		if err != nil {
			panic(err)
		}

		msg = pb.Message{
			MsgType: pb.MessageType_MsgAppend,
			From:    r.id,
			To:      to,
			Term:    r.Term,
			LogTerm: prevLogTerm,
			Index:   prevLogIndex,
			Entries: entries,
			Commit:  r.RaftLog.committed,
		}
	}
	r.msgs = append(r.msgs, msg)
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
func (r *Raft) sendHeartbeat(to uint64) {
	// Your Code Here (2A).
	msg := pb.Message{
		MsgType: pb.MessageType_MsgHeartbeat,
		From:    r.id,
		To:      to,
		Term:    r.Term,
	}
	r.msgs = append(r.msgs, msg)
}

func (r *Raft) sendRequestVote(id uint64) {
	// 需要将节点最新的 Index 和 Term 发送过去，方便 Follower 进行判断
	lastLogTerm, err := r.RaftLog.Term(r.RaftLog.LastIndex())
	if err != nil {
		log.Error("get log term err: ", err)
		panic(err)
	}

	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVote,
		From:    r.id,
		To:      id,
		Term:    r.Term,
		Index:   r.RaftLog.LastIndex(),
		LogTerm: lastLogTerm,
	}
	r.msgs = append(r.msgs, msg)
}

// 广播心跳消息
func (r *Raft) broadHeartbeat() {
	for id := range r.Prs {
		if id != r.id {
			r.sendHeartbeat(id)
		}
	}
}

// 广播日志追加消息
func (r *Raft) broadAppend() {
	for id := range r.Prs {
		if id != r.id {
			r.sendAppend(id)
		}
	}
}

// tick advances the internal logical clock by a single tick.
func (r *Raft) tick() {
	// Your Code Here (2A).
	switch r.State {
	case StateLeader:
		r.tickLeader()
	case StateFollower, StateCandidate:
		r.tickNonLeader()
	}
}

func (r *Raft) tickLeader() {
	r.heartbeatElapsed++
	if r.heartbeatElapsed >= r.heartbeatTimeout {
		r.heartbeatElapsed = 0
		// 发送心跳信息
		r.broadHeartbeat()
	}

	if r.leadTransferee != None {
		r.electionElapsed++
		if r.electionElapsed >= r.electionTimeout {
			r.leadTransferee = None
			r.electionElapsed = 0
		}
	}
}

func (r *Raft) tickNonLeader() {
	r.electionElapsed++
	if r.electionElapsed >= r.electionTimeout {
		// 发起新的选举
		r.startElection()
	}
}

// becomeFollower transform this peer's state to Follower
func (r *Raft) becomeFollower(term uint64, lead uint64) {
	// Your Code Here (2A).
	r.Term = term
	r.Lead = lead
	r.Vote = None
	r.electionElapsed = rand.Intn(r.electionTimeout/2) + 1
	r.leadTransferee = None
	r.State = StateFollower
	log.Debugf("id [%d] become follower at term [%d]", r.id, r.Term)
}

// becomeCandidate transform this peer's state to candidate
func (r *Raft) becomeCandidate() {
	// Your Code Here (2A).
	// 任期编号+1
	r.Term++
	// 推选自己为候选人, 并给自己投票
	r.Vote = r.id
	r.votes = make(map[uint64]bool)
	r.votes[r.id] = true

	r.electionElapsed = rand.Intn(r.electionTimeout/2) + 1
	r.leadTransferee = None

	r.State = StateCandidate
	log.Debugf("id [%d] become candidate at term [%d]", r.id, r.Term)
}

// becomeLeader transform this peer's state to leader
func (r *Raft) becomeLeader() {
	// Your Code Here (2A).
	// NOTE: Leader should propose a noop entry on its term
	r.heartbeatElapsed = 0

	//r.updatePengingConfIdx() todo

	// 初始化各个节点的 Progress
	for id := range r.Prs {
		r.Prs[id] = &Progress{
			Match: 0,
			Next:  r.RaftLog.LastIndex() + 1,
		}
	}
	// leader 自身的 match 是确定的
	r.Prs[r.id].Match = r.Prs[r.id].Next - 1

	// 竞选成功之后发送一个空的消息
	noop := &pb.Entry{}
	msg := pb.Message{
		MsgType: pb.MessageType_MsgPropose,
		From:    r.id,
		To:      r.id,
		Term:    r.Term,
		Entries: []*pb.Entry{noop},
	}
	r.handleMsgPropose(msg)

	r.Lead = r.id
	r.State = StateLeader
	log.Debugf("id [%d] become leader at term [%d]", r.id, r.Term)
}

// Step the entrance of handle message, see `MessageType`
// on `eraftpb.proto` for what msgs should be handled
func (r *Raft) Step(m pb.Message) error {
	// Your Code Here (2A).
	switch r.State {
	case StateFollower:
		return r.stepFollower(m)
	case StateCandidate:
		return r.stepCandidate(m)
	case StateLeader:
		return r.stepLeader(m)
	}
	return nil
}

func (r *Raft) stepFollower(m pb.Message) error {
	switch m.MsgType {
	case pb.MessageType_MsgBeat:
	case pb.MessageType_MsgHeartbeat:
		r.handleMsgHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
	case pb.MessageType_MsgHup:
		r.startElection()
	case pb.MessageType_MsgRequestVote:
		r.handleMsgRequestVote(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgRequestVoteResponse:
	case pb.MessageType_MsgAppendResponse:
	case pb.MessageType_MsgPropose:
	default:
		log.Errorf("unknown message type %+v", m.MsgType.String())
	}
	return nil
}

func (r *Raft) stepCandidate(m pb.Message) error {
	switch m.MsgType {
	case pb.MessageType_MsgBeat:
	case pb.MessageType_MsgHeartbeat:
		r.handleMsgHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
	case pb.MessageType_MsgHup:
		r.startElection()
	case pb.MessageType_MsgRequestVote:
		r.handleMsgRequestVote(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgRequestVoteResponse:
		r.handleMsgRequestVoteResp(m)
	case pb.MessageType_MsgSnapshot:
		r.handleSnapshot(m)
	case pb.MessageType_MsgAppendResponse:
	case pb.MessageType_MsgPropose:
	default:
		log.Errorf("unknown message type %+v", m.MsgType.String())
	}
	return nil
}

func (r *Raft) stepLeader(m pb.Message) error {
	switch m.MsgType {
	case pb.MessageType_MsgBeat:
		r.broadHeartbeat()
	case pb.MessageType_MsgHeartbeat:
		r.handleMsgHeartbeat(m)
	case pb.MessageType_MsgHeartbeatResponse:
		r.handleMsgHeartbeatResp(m)
	case pb.MessageType_MsgHup:
	case pb.MessageType_MsgRequestVote:
		r.handleMsgRequestVote(m)
	case pb.MessageType_MsgAppend:
		r.handleAppendEntries(m)
	case pb.MessageType_MsgAppendResponse:
		r.handleMsgAppendResp(m)
	case pb.MessageType_MsgPropose:
		r.handleMsgPropose(m)
	case pb.MessageType_MsgRequestVoteResponse:
	default:
		log.Errorf("unknown message type %+v", m.MsgType.String())
	}
	return nil
}

// startElection handle the MsgHup RPC request.Start a new election.
func (r *Raft) startElection() {
	if r.State == StateLeader {
		return
	}

	// 成为候选者
	r.becomeCandidate()

	//	计算投票是否已经满足
	var votesApprove int
	for _, ok := range r.votes {
		if ok {
			votesApprove++
		}
	}
	if votesApprove*2 > len(r.Prs) && len(r.Prs) > 0 {
		// 竞选成功
		r.becomeLeader()
		return
	}

	// 否则的话，发送请求投票消息
	for id := range r.Prs {
		if r.id != id {
			r.sendRequestVote(id)
		}
	}
}

func (r *Raft) handleMsgPropose(m pb.Message) {
	// 正在进行 leader 迁移，不能提交数据变更操作
	if r.leadTransferee != None {
		return
	}

	for _, entry := range m.Entries {
		// 如果是配置变更的消息
		if entry.EntryType == pb.EntryType_EntryConfChange {
			if r.PendingConfIndex > r.RaftLog.applied {
				// 无法提交消息变更，置空
				entry.EntryType = pb.EntryType_EntryNormal
				entry.Data = nil
			} else {
				r.PendingConfIndex = r.RaftLog.LastIndex() + 1
			}
		}
		r.RaftLog.LeaderAppendLogEntry(entry, r.Term)
		r.Prs[r.id].Next++
		r.Prs[r.id].Match++
	}

	// 如果只有一个节点，直接更新 log committed 信息
	if len(r.Prs) < 2 {
		r.updateLogCommitted()
		return
	}

	// 向其他节点广播消息
	r.broadAppend()
}

func (r *Raft) handleMsgRequestVote(m pb.Message) {
	msg := pb.Message{
		MsgType: pb.MessageType_MsgRequestVoteResponse,
		From:    r.id,
		To:      m.From,
		Term:    r.Term,
		Reject:  true,
	}

	defer func(msg *pb.Message) {
		r.msgs = append(r.msgs, *msg)
	}(&msg)

	// 收到小于当前节点任期的请求投票消息，直接拒绝
	if m.Term < r.Term {
		return
	}
	// 收到大于当前节点任期的请求投票消息，当前节点变为Follower
	if m.Term > r.Term {
		r.becomeFollower(m.Term, None)
		msg.Term = r.Term
	}

	// 判断日志是否是最新的
	if !r.RaftLog.IsUpToDate(m.LogTerm, m.Index) {
		return
	}

	if r.Vote == None || r.Vote == m.From {
		r.Vote = m.From
		msg.Reject = false
		r.electionElapsed = rand.Intn(r.electionTimeout/2) + 1
	}
}

func (r *Raft) handleMsgRequestVoteResp(m pb.Message) {
	// 计算投票是否满足
	if m.Reject {
		if m.Term > r.Term {
			r.becomeFollower(m.Term, m.From)
		} else {
			r.votes[m.From] = false
		}
	} else {
		r.votes[m.From] = true
	}

	yes, no := 0, 0
	for _, ok := range r.votes {
		if ok {
			yes++
		} else {
			no++
		}
	}

	// 竞选失败
	if len(r.Prs) == 0 || no*2 > len(r.Prs) {
		r.becomeFollower(m.Term, None)
	}

	// 竞选成功
	if yes*2 > len(r.Prs) {
		r.becomeLeader()
	}
}

// handleAppendEntries handle MsgAppend RPC request.
func (r *Raft) handleAppendEntries(m pb.Message) {
	msg := &pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		From:    m.To,
		To:      m.From,
		Term:    r.Term,
		Index:   m.Index + uint64(len(m.Entries)),
		Reject:  false,
	}
	defer func(msg *pb.Message) {
		r.msgs = append(r.msgs, *msg)
	}(msg)

	// 任期比自己低的消息，直接拒绝
	if m.Term < r.Term {
		msg.Reject = true
		return
	}

	// 如果收到了Term比自己高的消息
	// 或者当前是候选者，但是 Term 一样，那么节点需要变更自己的状态
	if m.Term > r.Term || (m.Term == r.Term && r.State == StateCandidate) {
		r.becomeFollower(m.Term, m.From)
	}

	msg.Term = r.Term
	r.Lead = m.From
	// 重置选举超时，防止发起新的选举
	r.electionElapsed = rand.Intn(r.electionTimeout/2) + 1

	// 校验消息的Index和Term
	if hint, reject, err := r.RaftLog.CheckIndexAndTerm(m); err != nil {
		panic(err)
	} else {
		msg.Reject = reject
		if reject {
			msg.Commit = r.RaftLog.committed
			msg.Hint = hint
			return
		}
	}

	// 添加日志
	r.RaftLog.AppendLogEntry(m)

	// 更新committed字段信息
	newCommitted := min(m.Commit, msg.Index)
	if newCommitted > r.RaftLog.committed {
		r.RaftLog.committed = newCommitted
	}
}

func (r *Raft) handleMsgAppendResp(m pb.Message) {
	if m.Reject {
		if m.Term > r.Term {
			r.becomeFollower(m.Term, None)
		} else {
			if m.Hint.HasXTerm {
				next := r.findConflict(m)
				if term, _ := r.RaftLog.Term(next); next != r.RaftLog.firstLogIndex && term == m.Hint.XTerm {
					r.Prs[m.From].Next = next
				} else {
					r.Prs[m.From].Next = m.Hint.XIndex
				}
			} else {
				r.Prs[m.From].Next = m.Hint.XLen
			}
			r.Prs[m.From].Next = max(r.Prs[m.From].Next, m.Commit+1)
			r.sendAppend(m.From)
			return
		}
	}

	if m.Index != 0 {
		if m.Index > r.Prs[m.From].Match {
			r.Prs[m.From].Match = m.Index
			r.Prs[m.From].Next = m.Index + 1
		}
		if m.From == r.leadTransferee && r.Prs[m.From].Match == r.RaftLog.LastIndex() {
			// todo
		}
		r.updateLogCommitted()
	}
}

func (r *Raft) findConflict(m pb.Message) uint64 {
	next := r.Prs[m.From].Next - 1
	for ; next > r.RaftLog.firstLogIndex; next-- {
		if term, err := r.RaftLog.Term(next); err != nil {
			panic(err)
		} else if term == m.Hint.XTerm {
			break
		}
	}
	return next
}

func (r *Raft) handleMsgHeartbeat(m pb.Message) {
	msg := &pb.Message{
		MsgType: pb.MessageType_MsgHeartbeatResponse,
		From:    m.To,
		To:      m.From,
		Term:    r.Term,
		Index:   r.RaftLog.LastIndex(),
		Reject:  false,
	}
	defer func(msg *pb.Message) {
		r.msgs = append(r.msgs, *msg)
	}(msg)

	if m.Term < r.Term {
		msg.Reject = true
		return
	}

	if m.Term > r.Term || (m.Term == r.Term && r.State == StateCandidate) {
		r.becomeFollower(m.Term, m.From)
	}
	msg.Term = r.Term
	r.Lead = m.From
	r.electionElapsed = rand.Intn(r.electionTimeout/2) + 1
}

func (r *Raft) handleMsgHeartbeatResp(m pb.Message) {
	if m.Index < r.RaftLog.LastIndex() {
		r.sendAppend(m.From)
	}
}

// handleSnapshot handle Snapshot RPC request
func (r *Raft) handleSnapshot(m pb.Message) {
	// Your Code Here (2C).
	msg := &pb.Message{
		MsgType: pb.MessageType_MsgAppendResponse,
		From:    m.To,
		To:      m.From,
		Term:    r.Term,
		Reject:  true,
		Index:   m.Snapshot.Metadata.Index,
	}
	defer func(m *pb.Message) {
		r.msgs = append(r.msgs, *msg)
	}(msg)

	// 判断任期号
	if m.Term < r.Term {
		return
	}
	if m.Term > r.Term || (m.Term == r.Term && r.State == StateCandidate) {
		r.becomeFollower(m.Term, m.From)
	}

	// 重置状态
	msg.Term = r.Term
	r.Lead = m.From
	r.electionElapsed = rand.Intn(r.electionTimeout/2) + 1

	r.RaftLog.AddSnapshot(m.Snapshot)

	// 更新节点数
	for _, n := range m.Snapshot.Metadata.ConfState.Nodes {
		r.Prs[n] = &Progress{}
	}
	msg.Reject = false
}

// addNode add a new node to raft group
func (r *Raft) addNode(id uint64) {
	// Your Code Here (3A).
}

// removeNode remove a node from raft group
func (r *Raft) removeNode(id uint64) {
	// Your Code Here (3A).
}

// updateLogCommitted update leader`s log committed.
func (r *Raft) updateLogCommitted() {
	if len(r.Prs) == 0 {
		return
	}

	matchIndexes, i := make([]uint64, len(r.Prs)), 0
	for _, p := range r.Prs {
		matchIndexes[i] = p.Match
		i++
	}

	sort.Sort(uint64Slice(matchIndexes))
	newCommitted := matchIndexes[len(r.Prs)/2]
	if len(r.Prs)%2 == 0 {
		newCommitted = matchIndexes[len(r.Prs)/2-1]
	}
	if newCommitted <= r.RaftLog.committed {
		return
	}

	if term, err := r.RaftLog.Term(newCommitted); err != nil {
		panic(err)
	} else if term == r.Term {
		r.RaftLog.committed = newCommitted
		r.broadAppend()
	}
}

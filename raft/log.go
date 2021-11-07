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
	"github.com/pingcap-incubator/tinykv/log"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
)

// RaftLog manage the log entries, its struct look like:
//
//  snapshot/first.....applied....committed....stabled.....last
//  --------|------------------------------------------------|
//                            log entries
//
// for simplify the RaftLog implement should manage all log entries
// that not truncated
type RaftLog struct {
	// storage contains all stable entries since the last snapshot.
	storage Storage

	// committed is the highest log position that is known to be in
	// stable storage on a quorum of nodes.
	committed uint64

	// applied is the highest log position that the application has
	// been instructed to apply to its state machine.
	// Invariant: applied <= committed
	applied uint64

	// log entries with index <= stabled are persisted to storage.
	// It is used to record the logs that are not persisted by storage yet.
	// Everytime handling `Ready`, the unstabled logs will be included.
	stabled uint64

	// all entries that have not yet compact.
	entries []pb.Entry

	// the incoming unstable snapshot, if any.
	// (Used in 2C)
	pendingSnapshot *pb.Snapshot

	// Your Data Here (2A).
	// index at first log.
	firstLogIndex uint64
	// term at first log.
	firstLogTerm uint64
}

// newLog returns log using the given storage. It recovers the log
// to the state that it just commits and applies the latest snapshot.
func newLog(storage Storage) *RaftLog {
	// Your Code Here (2A).
	firstIndex, err := storage.FirstIndex()
	if err != nil {
		log.Debug("get first index err: ", err)
		panic(err)
	}
	lastIndex, err := storage.LastIndex()
	if err != nil {
		log.Debug("get last index err: ", err)
		panic(err)
	}

	term, err := storage.Term(firstIndex - 1)
	if err != nil {
		log.Debug("get first index term err: ", err)
		panic(err)
	}

	raftLog := &RaftLog{
		storage:       storage,
		firstLogIndex: firstIndex - 1,
		firstLogTerm:  term,
		stabled:       lastIndex,
	}

	entries, err := storage.Entries(firstIndex, lastIndex+1)
	if err != nil {
		panic(err)
	}
	raftLog.entries = append(raftLog.entries, entries...)
	return raftLog
}

// We need to compact the log entries in some point of time like
// storage compact stabled log entries prevent the log entries
// grow unlimitedly in memory
func (l *RaftLog) maybeCompact() {
	// Your Code Here (2C).
}

// unstableEntries return all the unstable entries
func (l *RaftLog) unstableEntries() []pb.Entry {
	// Your Code Here (2A).
	if len(l.entries) == 0 {
		return nil
	}
	offset := l.entries[0].Index
	return l.entries[l.stabled-offset+1:]
}

// nextEnts returns all the committed but not applied entries
func (l *RaftLog) nextEnts() (ents []pb.Entry) {
	// Your Code Here (2A).
	if len(l.entries) == 0 {
		return nil
	}
	offset := l.entries[0].Index
	return l.entries[l.applied-offset+1 : l.committed-offset+1]
}

// LastIndex return the last index of the log entries
func (l *RaftLog) LastIndex() uint64 {
	// Your Code Here (2A).
	if len(l.entries) == 0 {
		return l.firstLogIndex
	}
	return l.entries[len(l.entries)-1].Index
}

// Term return the term of the entry in the given index
func (l *RaftLog) Term(i uint64) (uint64, error) {
	// Your Code Here (2A).
	if i == l.firstLogIndex {
		return l.firstLogTerm, nil
	}
	if len(l.entries) == 0 {
		return 0, ErrUnavailable
	}
	first := l.entries[0].Index
	if i < first {
		return 0, ErrCompacted
	}
	if int(i-first) >= len(l.entries) {
		return 0, ErrUnavailable
	}
	return l.entries[i-first].Term, nil
}

// LeaderAppendLogEntry add new log entry for a leader.
func (l *RaftLog) LeaderAppendLogEntry(e *pb.Entry, term uint64) {
	lastIndex := l.LastIndex()
	l.entries = append(l.entries, pb.Entry{
		EntryType: e.EntryType,
		Term:      term,
		Index:     lastIndex + 1,
		Data:      e.Data,
	})
}

// AppendLogEntry add new log entry.
func (l *RaftLog) AppendLogEntry(m pb.Message) {
	i, firstIndex, lastIndex, msgEntriesLen := uint64(0), m.Index+1, l.LastIndex(), uint64(len(m.Entries))
	if len(l.entries) > 0 {
		offset := l.entries[0].Index
		for ; i < msgEntriesLen && i+firstIndex <= lastIndex; i++ {
			logTerm, err := l.Term(i + firstIndex)
			if err != nil {
				panic(err)
			}
			if m.Entries[i].Index != i+firstIndex {
				panic("m.Entries[i].Index != i + firstIndex")
			}
			if m.Entries[i].Term != logTerm {
				l.entries = l.entries[:i+firstIndex-offset]
				break
			}
		}
	}

	m.Entries = m.Entries[i:]
	if storageLastIndex, err := l.storage.LastIndex(); err != nil {
		panic(err)
	} else {
		l.stabled = min(l.LastIndex(), storageLastIndex)
	}
	for _, e := range m.Entries {
		l.entries = append(l.entries, *e)
	}
}

func (l *RaftLog) CheckIndexAndTerm(m pb.Message) (bool, error) {
	// 检查索引号，索引号比目前日志的最大索引号还大直接返回
	if m.Index > l.LastIndex() {
		return true, nil
	}

	prevLogTerm, err := l.Term(m.Index)
	if err != nil && err != ErrUnavailable {
		panic(err)
	}

	// 检查任期号
	if prevLogTerm != m.LogTerm {
		return true, nil
	}
	return false, nil
}

func (l *RaftLog) Entries(lo uint64, hi uint64) ([]*pb.Entry, error) {
	offset := l.entries[0].Index
	if lo < offset {
		return nil, ErrCompacted
	}
	if hi > l.LastIndex()+1 {
		log.Panicf("entries' hi(%d) is out of bound lastindex(%d)", hi, l.LastIndex())
	}
	ents := l.entries[lo-offset : hi-offset]
	ret := make([]*pb.Entry, len(ents))
	for i := range ents {
		ret[i] = &ents[i]
	}
	return ret, nil
}

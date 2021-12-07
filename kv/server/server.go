package server

import (
	"bytes"
	"context"
	"github.com/pingcap-incubator/tinykv/kv/coprocessor"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/storage/raft_storage"
	"github.com/pingcap-incubator/tinykv/kv/transaction/latches"
	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"
	coppb "github.com/pingcap-incubator/tinykv/proto/pkg/coprocessor"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/tinykvpb"
	"github.com/pingcap/tidb/kv"
)

var _ tinykvpb.TinyKvServer = new(Server)

// Server is a TinyKV server, it 'faces outwards', sending and receiving messages from clients such as TinySQL.
type Server struct {
	storage storage.Storage

	// (Used in 4A/4B)
	Latches *latches.Latches

	// coprocessor API handler, out of course scope
	copHandler *coprocessor.CopHandler
}

func NewServer(storage storage.Storage) *Server {
	return &Server{
		storage: storage,
		Latches: latches.NewLatches(),
	}
}

// The below functions are Server's gRPC API (implements TinyKvServer).

// Raft commands (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Raft(stream tinykvpb.TinyKv_RaftServer) error {
	return server.storage.(*raft_storage.RaftStorage).Raft(stream)
}

// Snapshot stream (tinykv <-> tinykv)
// Only used for RaftStorage, so trivially forward it.
func (server *Server) Snapshot(stream tinykvpb.TinyKv_SnapshotServer) error {
	return server.storage.(*raft_storage.RaftStorage).Snapshot(stream)
}

// Transactional API.
func (server *Server) KvGet(_ context.Context, req *kvrpcpb.GetRequest) (resp *kvrpcpb.GetResponse, err error) {
	// Your Code Here (4B).
	resp = &kvrpcpb.GetResponse{}

	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return resp, err
	}
	defer reader.Close()

	// 先获取锁
	txn := mvcc.NewMvccTxn(reader, req.Version)
	lock, err := txn.GetLock(req.Key)
	if err != nil {
		return resp, err
	}
	if lock != nil && lock.Ts <= req.Version {
		resp.Error = &kvrpcpb.KeyError{
			Locked: &kvrpcpb.LockInfo{
				PrimaryLock: lock.Primary,
				LockVersion: lock.Ts,
				LockTtl:     lock.Ttl,
				Key:         req.Key,
			},
		}
		return
	}

	// 没有冲突，获取 value
	value, err := txn.GetValue(req.Key)
	if err != nil {
		return resp, err
	}
	resp.Value = value
	return
}

func (server *Server) KvPrewrite(_ context.Context, req *kvrpcpb.PrewriteRequest) (resp *kvrpcpb.PrewriteResponse, err error) {
	// Your Code Here (4B).
	resp = &kvrpcpb.PrewriteResponse{}

	getKind := func(op kvrpcpb.Op) mvcc.WriteKind {
		switch op {
		case kvrpcpb.Op_Put:
			return mvcc.WriteKindPut
		case kvrpcpb.Op_Del:
			return mvcc.WriteKindDelete
		case kvrpcpb.Op_Rollback:
			return mvcc.WriteKindRollback
		}
		return -1
	}

	var k mvcc.WriteKind
	for _, mut := range req.Mutations {
		if bytes.Compare(mut.Key, req.PrimaryLock) == 0 {
			k = getKind(mut.Op)
		}
	}
	// 先加锁
	lock := &mvcc.Lock{
		Primary: req.PrimaryLock,
		Ts:      req.StartVersion,
		Ttl:     req.LockTtl,
		Kind:    k,
	}

	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return resp, err
	}
	defer reader.Close()

	// 加锁
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	txn.PutLock(req.PrimaryLock, lock)

	// 写数据
	for _, mut := range req.Mutations {
		txn.PutWrite(mut.Key, req.StartVersion, &mvcc.Write{
			StartTS: req.StartVersion,
			Kind:    getKind(mut.Op),
		})
	}
	return
}

func (server *Server) KvCommit(_ context.Context, req *kvrpcpb.CommitRequest) (*kvrpcpb.CommitResponse, error) {
	// Your Code Here (4B).
	return nil, nil
}

func (server *Server) KvScan(_ context.Context, req *kvrpcpb.ScanRequest) (*kvrpcpb.ScanResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

func (server *Server) KvCheckTxnStatus(_ context.Context, req *kvrpcpb.CheckTxnStatusRequest) (*kvrpcpb.CheckTxnStatusResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

func (server *Server) KvBatchRollback(_ context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

func (server *Server) KvResolveLock(_ context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	// Your Code Here (4C).
	return nil, nil
}

// SQL push down commands.
func (server *Server) Coprocessor(_ context.Context, req *coppb.Request) (*coppb.Response, error) {
	resp := new(coppb.Response)
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		if regionErr, ok := err.(*raft_storage.RegionError); ok {
			resp.RegionError = regionErr.RequestErr
			return resp, nil
		}
		return nil, err
	}
	switch req.Tp {
	case kv.ReqTypeDAG:
		return server.copHandler.HandleCopDAGRequest(reader, req), nil
	case kv.ReqTypeAnalyze:
		return server.copHandler.HandleCopAnalyzeRequest(reader, req), nil
	}
	return nil, nil
}

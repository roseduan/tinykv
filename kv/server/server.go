package server

import (
	"context"
	"github.com/pingcap-incubator/tinykv/kv/coprocessor"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/storage/raft_storage"
	"github.com/pingcap-incubator/tinykv/kv/transaction/latches"
	"github.com/pingcap-incubator/tinykv/kv/transaction/mvcc"
	"github.com/pingcap-incubator/tinykv/log"
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

	if lock != nil {
		if lock.Ts <= req.Version && lock.Kind != mvcc.WriteKindDelete {
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
	}

	// 没有被阻塞，获取 value
	value, err := txn.GetValue(req.Key)
	if err != nil {
		return resp, err
	}
	if len(value) == 0 {
		resp.NotFound = true
	}
	resp.Value = value
	return
}

func (server *Server) KvPrewrite(_ context.Context, req *kvrpcpb.PrewriteRequest) (resp *kvrpcpb.PrewriteResponse, err error) {
	// Your Code Here (4B).
	resp = &kvrpcpb.PrewriteResponse{}

	var keys [][]byte
	for _, mut := range req.Mutations {
		keys = append(keys, mut.Key)
	}

	// 锁保护，避免多个client的竞争
	if wg := server.Latches.AcquireLatches(keys); wg != nil {
		resp.Errors = []*kvrpcpb.KeyError{
			{Retryable: "please retry"},
		}
		return
	}
	defer server.Latches.ReleaseLatches(keys)

	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return resp, err
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	for _, mut := range req.Mutations {
		// 获取锁信息
		lock, err := txn.GetLock(mut.Key)
		if err != nil {
			log.Fatalf("get lock err: %+v", err)
		}

		// 其他事务还没有提交
		if lock != nil && lock.Ts <= req.StartVersion {
			lockInfo := &kvrpcpb.LockInfo{
				PrimaryLock: lock.Primary,
				LockVersion: lock.Ts,
				Key:         mut.Key,
				LockTtl:     lock.Ttl,
			}
			resp.Errors = append(resp.Errors, &kvrpcpb.KeyError{Locked: lockInfo})
			continue
		}

		_, commitTs, err := txn.MostRecentWrite(mut.Key)
		if err != nil {
			log.Fatalf("get most recent write err: %+v", err)
		}
		// 存在冲突
		if commitTs >= req.StartVersion {
			wc := &kvrpcpb.WriteConflict{
				StartTs:    req.StartVersion,
				ConflictTs: commitTs,
				Key:        mut.Key,
				Primary:    req.PrimaryLock,
			}
			resp.Errors = append(resp.Errors, &kvrpcpb.KeyError{Conflict: wc})
			continue
		}

		// 写入锁信息
		txn.PutLock(mut.Key, &mvcc.Lock{
			Primary: req.PrimaryLock,
			Ts:      req.StartVersion,
			Ttl:     req.LockTtl,
			Kind:    mvcc.WriteKindFromProto(mut.Op),
		})

		// 写入kv数据信息
		if mut.Op == kvrpcpb.Op_Put {
			txn.PutValue(mut.Key, mut.Value)
		}
		if mut.Op == kvrpcpb.Op_Del {
			txn.DeleteValue(mut.Key)
		}
	}

	// 将数据写到storage当中
	err = server.storage.Write(req.Context, txn.Writes())
	return
}

func (server *Server) KvCommit(_ context.Context, req *kvrpcpb.CommitRequest) (resp *kvrpcpb.CommitResponse, err error) {
	// Your Code Here (4B).
	resp = &kvrpcpb.CommitResponse{}

	// 锁保护，避免多个client的竞争
	if wg := server.Latches.AcquireLatches(req.Keys); wg != nil {
		resp.Error = &kvrpcpb.KeyError{
			Retryable: "please retry",
		}
		return
	}
	defer server.Latches.ReleaseLatches(req.Keys)

	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return resp, err
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.StartVersion)
	for _, key := range req.Keys {
		// 获取锁标记
		var lock *mvcc.Lock
		if lock, err = txn.GetLock(key); err != nil {
			log.Fatalf("get lock err: %+v", err)
		}

		if lock == nil {
			continue
		}

		// 被其他的事务锁住了
		if lock.Ts != req.StartVersion {
			lockInfo := &kvrpcpb.LockInfo{
				PrimaryLock: lock.Primary,
				LockVersion: lock.Ts,
				Key:         key,
				LockTtl:     lock.Ttl,
			}
			resp.Error = &kvrpcpb.KeyError{Locked: lockInfo, Retryable: "please retry"}
			continue
		}

		// 写入Write数据
		w := &mvcc.Write{StartTS: req.StartVersion, Kind: lock.Kind}
		txn.PutWrite(key, req.CommitVersion, w)

		// 清除锁
		txn.DeleteLock(key)
	}
	err = server.storage.Write(req.Context, txn.Writes())
	return
}

func (server *Server) KvScan(_ context.Context, req *kvrpcpb.ScanRequest) (resp *kvrpcpb.ScanResponse, err error) {
	// Your Code Here (4C).
	resp = &kvrpcpb.ScanResponse{}

	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return resp, err
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.Version)
	scanner := mvcc.NewScanner(req.StartKey, txn)
	defer scanner.Close()

	var (
		pairs []*kvrpcpb.KvPair
		limit = req.Limit
	)
	for limit > 0 {
		k, v, err := scanner.Next()
		if err == nil && len(v) == 0 {
			break
		}

		pair := &kvrpcpb.KvPair{}
		if err != nil {
			pair.Error = &kvrpcpb.KeyError{}
		} else {
			pair.Key = k
			pair.Value = v
		}
		limit--
		pairs = append(pairs, pair)
	}
	resp.Pairs = pairs
	return
}

func (server *Server) KvCheckTxnStatus(_ context.Context, req *kvrpcpb.CheckTxnStatusRequest) (*kvrpcpb.CheckTxnStatusResponse, error) {
	// Your Code Here (4C).
	resp := &kvrpcpb.CheckTxnStatusResponse{}
	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return resp, nil
	}
	defer reader.Close()

	txn := mvcc.NewMvccTxn(reader, req.LockTs)
	lock, err := txn.GetLock(req.PrimaryKey)
	if err != nil {
		log.Fatalf("get lock err.[%+v]", err)
	}
	// 不是当前事务的锁，直接返回
	if lock != nil && lock.Ts != req.LockTs {
		return resp, nil
	}

	if lock == nil {
		// 获取最近一次提交的数据
		write, commit, err := txn.MostRecentWrite(req.PrimaryKey)
		if err != nil {
			log.Fatalf("get most recent write err.[%+v]", err)
		}
		if write != nil {
			if write.Kind == mvcc.WriteKindRollback {
				resp.LockTtl = 0
				resp.CommitVersion = 0
			} else {
				resp.CommitVersion = commit
			}
			return resp, nil
		}
	}

	if lock == nil {
		resp.Action = kvrpcpb.Action_LockNotExistRollback
	} else if mvcc.PhysicalTime(lock.Ts)+lock.Ttl <= mvcc.PhysicalTime(req.CurrentTs) {
		resp.Action = kvrpcpb.Action_TTLExpireRollback
		resp.LockTtl = lock.Ttl

		lks, err := mvcc.AllLocksForTxn(txn)
		if err != nil {
			log.Fatalf("get all locks for txn err.[%+v]", err)
		}
		for _, lk := range lks {
			txn.DeleteLock(lk.Key)
			txn.DeleteValue(lk.Key)
		}
	} else {
		return resp, nil
	}

	w := &mvcc.Write{StartTS: req.LockTs, Kind: mvcc.WriteKindRollback}
	txn.PutWrite(req.PrimaryKey, req.LockTs, w)
	if err := server.storage.Write(req.Context, txn.Writes()); err != nil {
		return nil, err
	}
	return resp, nil
}

func (server *Server) KvBatchRollback(_ context.Context, req *kvrpcpb.BatchRollbackRequest) (*kvrpcpb.BatchRollbackResponse, error) {
	// Your Code Here (4C).
	resp := &kvrpcpb.BatchRollbackResponse{}
	if wg := server.Latches.AcquireLatches(req.Keys); wg != nil {
		resp.Error = &kvrpcpb.KeyError{
			Retryable: "please retry",
		}
		return resp, nil
	}

	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return resp, nil
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)

	for _, key := range req.Keys {
		lock, err := txn.GetLock(key)
		if err != nil {
			log.Fatalf("get lock err.[%+v]", err)
		}
		if lock == nil {
			// 查看最近提交的事务信息
			write, _, err := txn.MostRecentWrite(key)
			if err != nil {
				log.Fatalf("get most recent write err.[%+v]", err)
			}
			if write != nil {
				if write.Kind != mvcc.WriteKindRollback {
					resp.Error = &kvrpcpb.KeyError{Abort: "abort"}
				}
				return resp, nil
			}
		}

		if lock != nil && lock.Ts == req.StartVersion {
			// 删除锁信息
			txn.DeleteLock(key)
			//删除数据信息
			txn.DeleteValue(key)
		}
		// 写入回滚信息
		w := &mvcc.Write{StartTS: req.StartVersion, Kind: mvcc.WriteKindRollback}
		txn.PutWrite(key, req.StartVersion, w)
	}

	if err = server.storage.Write(req.Context, txn.Writes()); err != nil {
		return resp, err
	}
	return resp, nil
}

func (server *Server) KvResolveLock(_ context.Context, req *kvrpcpb.ResolveLockRequest) (*kvrpcpb.ResolveLockResponse, error) {
	// Your Code Here (4C).
	resp := &kvrpcpb.ResolveLockResponse{}

	reader, err := server.storage.Reader(req.Context)
	if err != nil {
		return resp, nil
	}
	defer reader.Close()
	txn := mvcc.NewMvccTxn(reader, req.StartVersion)

	pairs, err := mvcc.AllLocksForTxn(txn)
	if err != nil || len(pairs) == 0 {
		return resp, err
	}

	var keys [][]byte
	for _, pair := range pairs {
		keys = append(keys, pair.Key)
	}
	if wg := server.Latches.AcquireLatches(keys); wg != nil {
		resp.Error = &kvrpcpb.KeyError{
			Retryable: "please retry",
		}
		return resp, nil
	}

	for _, pair := range pairs {
		if pair.Lock.Ts != req.StartVersion {
			continue
		}

		if req.CommitVersion == 0 {
			// 回滚
			txn.DeleteValue(pair.Key)
			txn.DeleteLock(pair.Key)
			w := &mvcc.Write{StartTS: req.StartVersion, Kind: mvcc.WriteKindRollback}
			txn.PutWrite(pair.Key, req.StartVersion, w)
		} else {
			// 使用commitVersion提交
			txn.DeleteLock(pair.Key)
			w := &mvcc.Write{StartTS: req.StartVersion, Kind: pair.Lock.Kind}
			txn.PutWrite(pair.Key, req.CommitVersion, w)
		}
	}
	if err = server.storage.Write(req.Context, txn.Writes()); err != nil {
		return resp, err
	}
	return resp, nil
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

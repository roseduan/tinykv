package server

import (
	"context"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
)

// The functions below are Server's Raw API. (implements TinyKvServer).
// Some helper methods can be found in sever.go in the current directory

// RawGet return the corresponding Get response based on RawGetRequest's CF and Key fields
func (server *Server) RawGet(_ context.Context, req *kvrpcpb.RawGetRequest) (resp *kvrpcpb.RawGetResponse, err error) {
	// Your Code Here (1).
	resp = &kvrpcpb.RawGetResponse{}

	// get storage reader.
	var reader storage.StorageReader
	reader, err = server.storage.Reader(req.Context)
	if err != nil {
		return
	}
	defer reader.Close()

	val, err := reader.GetCF(req.Cf, req.Key)
	if len(val) == 0 {
		resp.NotFound = true
	}
	resp.Value = val
	return
}

// RawPut puts the target data into storage and returns the corresponding response
func (server *Server) RawPut(_ context.Context, req *kvrpcpb.RawPutRequest) (resp *kvrpcpb.RawPutResponse, err error) {
	// Your Code Here (1).
	// Hint: Consider using Storage.Modify to store data to be modified
	resp = &kvrpcpb.RawPutResponse{}
	modify := []storage.Modify{
		{
			Data: storage.Put{
				Cf:    req.Cf,
				Key:   req.Key,
				Value: req.Value,
			},
		},
	}
	err = server.storage.Write(req.Context, modify)
	return
}

// RawDelete delete the target data from storage and returns the corresponding response
func (server *Server) RawDelete(_ context.Context, req *kvrpcpb.RawDeleteRequest) (resp *kvrpcpb.RawDeleteResponse, err error) {
	// Your Code Here (1).
	// Hint: Consider using Storage.Modify to store data to be deleted
	resp = &kvrpcpb.RawDeleteResponse{}
	modify := []storage.Modify{
		{
			Data: storage.Delete{
				Cf:  req.Cf,
				Key: req.Key,
			},
		},
	}
	err = server.storage.Write(req.Context, modify)
	return
}

// RawScan scan the data starting from the start key up to limit. and return the corresponding result
func (server *Server) RawScan(_ context.Context, req *kvrpcpb.RawScanRequest) (resp *kvrpcpb.RawScanResponse, err error) {
	// Your Code Here (1).
	// Hint: Consider using reader.IterCF
	resp = &kvrpcpb.RawScanResponse{}
	var reader storage.StorageReader
	reader, err = server.storage.Reader(req.Context)
	if err != nil {
		return
	}
	defer reader.Close()

	iter := reader.IterCF(req.Cf)
	defer iter.Close()

	var pairs []*kvrpcpb.KvPair
	limit := req.Limit
	for iter.Seek(req.StartKey); iter.Valid() && limit > 0; iter.Next() {
		item := iter.Item()
		value, err := item.Value()
		if err != nil {
			return resp, err
		}
		pairs = append(pairs, &kvrpcpb.KvPair{
			Key:   item.Key(),
			Value: value,
		})
		limit--
	}
	resp.Kvs = pairs
	return
}

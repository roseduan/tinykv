第一个 Project 是集成 Badger，实现一个简易的单机版 kv。

Badger 是一个很优秀的开源的单机版 kv 存储引擎，基于 LSM Tree 实现，读写性能都很好，需要简单熟悉下 Badger 的用法，可以参考下官方示例：https://github.com/dgraph-io/badger。

在 TinyKV 中，存储层是一个抽象接口，分别实现了 raft storage、mem storage、standalone storage，这里我们只需要实现 standalone storage 就行了。

具体的实现，在 kv/storage/standalone_storage/standalone_storage.go 中，需要封装一下 Badger，然后实现 storage 接口中定义的几个方法。

这里贴一下结构体的定义：

```go
type StandAloneStorage struct {
	// Your Data Here (1).
	badgerDB *badger.DB
	options  badger.Options
}
```

需要说明的是，在 Reader 方法中，需要返回一个 StorageReader 接口，这是一个抽象接口，具体逻辑需要我们自定义。

```go
func (s *StandAloneStorage) Reader(ctx *kvrpcpb.Context) (storage.StorageReader, error) {
	// Your Code Here (1).
	txn := s.badgerDB.NewTransaction(false)
	reader := NewStandaloneReader(txn)
	return reader, nil
}
```

例如我定义了一个 StandaloneReader：

```go
type StandaloneReader struct {
	txn *badger.Txn
}

func NewStandaloneReader(txn *badger.Txn) *StandaloneReader {
	return &StandaloneReader{
		txn: txn,
	}
}
```

这里完成之后，还需要在 kv/server/raw_api.go 中完善相应的 gRPC 接口，直接解析传过来的参数，然后调用 Storage 接口中的方法即可。这里展示一个示例：

```go
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
```

这里的几个接口完成之后，一个完整的 Standalone KV 就完成了。
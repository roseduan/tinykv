package standalone_storage

import (
	"fmt"
	"github.com/Connor1996/badger"
	"github.com/pingcap-incubator/tinykv/kv/config"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	"github.com/pingcap-incubator/tinykv/log"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
)

// StandAloneStorage is an implementation of `Storage` for a single-node TinyKV instance. It does not
// communicate with other nodes and all data is stored locally.
type StandAloneStorage struct {
	// Your Data Here (1).
	badgerDB *badger.DB
	options  badger.Options
}

func NewStandAloneStorage(conf *config.Config) *StandAloneStorage {
	// Your Code Here (1).
	options := badger.DefaultOptions
	options.Dir = conf.DBPath
	options.ValueDir = conf.DBPath
	return &StandAloneStorage{
		options: options,
	}
}

func (s *StandAloneStorage) Start() error {
	// Your Code Here (1).
	db, err := badger.Open(s.options)
	if err != nil {
		log.Debugf(fmt.Sprintf("open badger err.%+v", err))
		return err
	}
	s.badgerDB = db
	return nil
}

func (s *StandAloneStorage) Stop() error {
	// Your Code Here (1).
	if s.badgerDB != nil {
		if err := s.badgerDB.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (s *StandAloneStorage) Reader(ctx *kvrpcpb.Context) (storage.StorageReader, error) {
	// Your Code Here (1).
	txn := s.badgerDB.NewTransaction(false)
	reader := NewStandaloneReader(txn)
	return reader, nil
}

func (s *StandAloneStorage) Write(ctx *kvrpcpb.Context, batch []storage.Modify) error {
	// Your Code Here (1).
	err := s.badgerDB.Update(func(txn *badger.Txn) error {
		for _, modify := range batch {
			keyWithCF := engine_util.KeyWithCF(modify.Cf(), modify.Key())
			switch modify.Data.(type) {
			case storage.Put:
				if err := txn.Set(keyWithCF, modify.Value()); err != nil {
					return err
				}
			case storage.Delete:
				if err := txn.Delete(keyWithCF); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return err
}

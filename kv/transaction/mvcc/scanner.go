package mvcc

import (
	"bytes"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	"github.com/pingcap-incubator/tinykv/log"
)

// Scanner is used for reading multiple sequential key/value pairs from the storage layer. It is aware of the implementation
// of the storage layer and returns results suitable for users.
// Invariant: either the scanner is finished and cannot be used, or it is ready to return a value immediately.
type Scanner struct {
	// Your Data Here (4C).
	dbIter engine_util.DBIterator
	txn    *MvccTxn
}

// NewScanner creates a new scanner ready to read from the snapshot in txn.
func NewScanner(startKey []byte, txn *MvccTxn) *Scanner {
	// Your Code Here (4C).
	iter := txn.Reader.IterCF(engine_util.CfWrite)
	iter.Seek(EncodeKey(startKey, txn.StartTS))
	s := &Scanner{
		txn:    txn,
		dbIter: iter,
	}
	s.findIterPos(startKey)
	return s
}

func (scan *Scanner) Close() {
	// Your Code Here (4C).
	if scan.dbIter != nil {
		scan.dbIter.Close()
	}
}

// Next returns the next key/value pair from the scanner. If the scanner is exhausted, then it will return `nil, nil, nil`.
func (scan *Scanner) Next() ([]byte, []byte, error) {
	// Your Code Here (4C).
	if !scan.dbIter.Valid() {
		return nil, nil, nil
	}

	item := scan.dbIter.Item()
	resKey := DecodeUserKey(item.Key())
	value, err := item.Value()

	if err != nil {
		return nil, nil, err
	}
	write, err := ParseWrite(value)
	if err != nil {
		return nil, nil, err
	}
	resVal, err := scan.txn.Reader.GetCF(engine_util.CfDefault, EncodeKey(resKey, write.StartTS))
	if err != nil {
		return nil, nil, err
	}

	scan.findIterPos(resKey)

	return resKey, resVal, nil
}

func (scan *Scanner) findIterPos(curKey []byte) {
	for ; scan.dbIter.Valid(); scan.dbIter.Next() {
		item := scan.dbIter.Item()
		ts := decodeTimestamp(item.Key())

		if bytes.Equal(curKey, DecodeUserKey(item.Key())) {
			continue
		}
		if ts > scan.txn.StartTS {
			continue
		}

		value, err := item.Value()
		if err != nil {
			log.Fatal("get item value err.", err)
		}
		write, err := ParseWrite(value)
		if err != nil {
			log.Fatal("parse write err.", err)
		}
		if write.Kind != WriteKindPut {
			curKey = DecodeUserKey(item.Key())
			continue
		}
		break
	}
}

package db

import (
	"context"
	"time"

	"github.com/distributedio/titan/conf"
	"github.com/distributedio/titan/metrics"
	"github.com/pingcap/tidb/kv"
	"go.uber.org/zap"
)

var (
	sysZTLeader              = []byte("$sys:0:ZTL:ZTLeader")
	sysZTKeyPrefixLength     = len(toZTKey([]byte{}))
	sysZTLeaderFlushInterval = 10

	ztQueue chan []byte
)

// loadZList is read only, so ZList did not call Destroy()
func loadZList(txn *Transaction, metaKey []byte) (*ZList, error) {
	val, err := txn.t.Get(metaKey)
	if err != nil {
		return nil, err
	}

	obj, err := DecodeObject(val)
	if err != nil {
		return nil, err
	}
	if obj.Type != ObjectList {
		return nil, ErrTypeMismatch
	}
	if obj.Encoding != ObjectEncodingZiplist {
		zap.L().Error("[ZT] error in trans zlist, encoding type error", zap.Error(err))
		return nil, ErrEncodingMismatch
	}
	if obj.ExpireAt != 0 && obj.ExpireAt < Now() {
		return nil, ErrKeyNotFound
	}

	l := &ZList{
		rawMetaKey: metaKey,
		txn:        txn,
	}
	if err = l.Unmarshal(obj, val); err != nil {
		return nil, err
	}
	return l, nil
}

// toZTKey convert meta key to ZT key
// {sys.ns}:{sys.id}:{ZT}:{metakey}
// NOTE put this key to sys db.
func toZTKey(metakey []byte) []byte {
	b := []byte{}
	b = append(b, sysNamespace...)
	b = append(b, ':', byte(sysDatabaseID))
	b = append(b, ':', 'Z', 'T', ':')
	b = append(b, metakey...)
	return b
}

// PutZList should be called after ZList created
func PutZList(txn *Transaction, metakey []byte) error {
	if logEnv := zap.L().Check(zap.DebugLevel, "[ZT] Zlist recorded in txn"); logEnv != nil {
		logEnv.Write(zap.String("key", string(metakey)))
	}
	return txn.t.Set(toZTKey(metakey), []byte{0})
}

// RemoveZTKey remove an metakey from ZT
func RemoveZTKey(txn *Transaction, metakey []byte) error {
	return txn.t.Delete(toZTKey(metakey))
}

// doZListTransfer get zt key, create zlist and transfer to llist, after that, delete zt key
func doZListTransfer(txn *Transaction, metakey []byte) (int, error) {
	zlist, err := loadZList(txn, metakey)
	if err != nil {
		if err == ErrTypeMismatch || err == ErrEncodingMismatch || err == ErrKeyNotFound {
			if err = RemoveZTKey(txn, metakey); err != nil {
				zap.L().Error("[ZT] error in remove ZTKkey", zap.Error(err))
				return 0, err
			}
			return 0, nil
		}
		zap.L().Error("[ZT] error in create zlist", zap.Error(err))
		return 0, err
	}

	llist, err := zlist.TransferToLList(splitMetaKey(metakey))
	if err != nil {
		zap.L().Error("[ZT] error in convert zlist", zap.Error(err))
		return 0, err
	}
	// clean the zt key, after success
	if err = RemoveZTKey(txn, metakey); err != nil {
		zap.L().Error("[ZT] error in remove ZTKkey", zap.Error(err))
		return 0, err
	}

	return int(llist.Len), nil
}

func ztWorker(db *DB, batch int, interval time.Duration) {
	var txn *Transaction
	var err error
	var n int

	txnstart := false
	batchCount := 0
	sum := 0
	commit := func(t *Transaction) {
		if err = t.Commit(context.Background()); err != nil {
			zap.L().Error("[ZT] error in commit transfer", zap.Error(err))
			if err := txn.Rollback(); err != nil {
				zap.L().Error("[ZT] rollback failed", zap.Error(err))
			}
		} else {
			metrics.GetMetrics().ZTInfoCounterVec.WithLabelValues("zlist").Add(float64(batchCount))
			metrics.GetMetrics().ZTInfoCounterVec.WithLabelValues("key").Add(float64(sum))
			if logEnv := zap.L().Check(zap.DebugLevel, "[ZT] transfer zlist succeed"); logEnv != nil {
				logEnv.Write(zap.Int("count", batchCount),
					zap.Int("n", sum))
			}
		}
		txnstart = false
		batchCount = 0
		sum = 0
	}

	// create zlist and transfer to llist, after that, delete zt key
	for {
		select {
		case metakey := <-ztQueue:
			if !txnstart {
				if txn, err = db.Begin(); err != nil {
					zap.L().Error("[ZT] zt worker error in kv begin", zap.Error(err))
					continue
				}
				txnstart = true
			}

			if n, err = doZListTransfer(txn, metakey); err != nil {
				if err := txn.Rollback(); err != nil {
					zap.L().Error("[ZT] rollback failed", zap.Error(err))
				}
				txnstart = false
				continue
			}
			sum += n
			batchCount++
			if batchCount >= batch {
				commit(txn)
			}
		default:
			if batchCount > 0 {
				commit(txn)
			} else {
				time.Sleep(interval)
				txnstart = false
			}
		}
	}
}

func runZT(db *DB, prefix []byte, tick <-chan time.Time) ([]byte, error) {
	txn, err := db.Begin()
	if err != nil {
		zap.L().Error("[ZT] error in kv begin", zap.Error(err))
		return toZTKey(nil), nil
	}
	endPrefix := kv.Key(prefix).PrefixNext()
	iter, err := txn.t.Iter(prefix, endPrefix)
	if err != nil {
		zap.L().Error("[ZT] error in seek", zap.ByteString("prefix", prefix), zap.Error(err))
		return toZTKey(nil), err
	}

	for ; iter.Valid() && iter.Key().HasPrefix(prefix); err = iter.Next() {
		if err != nil {
			zap.L().Error("[ZT] error in iter next", zap.Error(err))
			return toZTKey(nil), err
		}
		select {
		case ztQueue <- iter.Key()[sysZTKeyPrefixLength:]:
		case <-tick:
			return iter.Key(), nil
		default:
			return iter.Key(), nil
		}
	}
	if logEnv := zap.L().Check(zap.DebugLevel, "[ZT] no more ZT item, retrive iterator"); logEnv != nil {
		logEnv.Write(zap.ByteString("prefix", prefix))
	}

	return toZTKey(nil), txn.Commit(context.Background())
}

// StartZT start ZT fill in the queue(channel), and start the worker to consume.
func StartZT(task *Task) {
	conf := task.conf.(conf.ZT)
	ztQueue = make(chan []byte, conf.QueueDepth)
	for i := 0; i < conf.Workers; i++ {
		go ztWorker(task.db, conf.BatchCount, conf.Interval)
	}

	// check leader and fill the channel
	var err error
	prefix := toZTKey(nil)
	ticker := time.NewTicker(conf.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-task.Done():
			if logEnv := zap.L().Check(zap.DebugLevel, "[ZT] current is not ztransfer leader"); logEnv != nil {
				logEnv.Write(zap.ByteString("key", task.key),
					zap.ByteString("uuid", task.id),
					zap.String("lable", task.lable))
			}
			return
		case <-ticker.C:
		}

		if prefix, err = runZT(task.db, prefix, ticker.C); err != nil {
			zap.L().Error("[ZT] error in run ZT",
				zap.Int64("dbid", int64(task.db.ID)),
				zap.ByteString("prefix", prefix),
				zap.Error(err))
			continue
		}
	}
}

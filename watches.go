package main

import "encoding/binary"
import "encoding/json"
import "time"

import "go.etcd.io/bbolt"

var watchesBucket = []byte("watches")

type watchType string

const watchTypeTransaction watchType = "transaction"
const watchTypeAddress watchType = "address"

type watchRecord struct {
    CreatedAt time.Time
    ChatID    int64
    Type      watchType
    WatchID   string
}

type watchStore struct {
    db *bbolt.DB
}

func openWatchStore(path string) (*watchStore, error) {
    var db, err = bbolt.Open(path, 0600, nil)
    if err != nil { return nil, err }
    err = db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(watchesBucket)
        return err
    })
    if err != nil {
        db.Close()
        return nil, err
    }
    return &watchStore{db: db}, nil
}

func (s *watchStore) close() error {
    return s.db.Close()
}

func (s *watchStore) add(chatID int64, typ watchType, watchID string) error {
    var data, err = json.Marshal(watchRecord{
        CreatedAt: time.Now(),
        ChatID:    chatID,
        Type:      typ,
        WatchID:   watchID,
    })
    if err != nil { return err }
    return s.db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(watchesBucket)
        var id, err = b.NextSequence()
        if err != nil { return err }
        return b.Put(itob(id), data)
    })
}

func itob(v uint64) []byte {
    var buf = make([]byte, 8)
    binary.BigEndian.PutUint64(buf, v)
    return buf
}

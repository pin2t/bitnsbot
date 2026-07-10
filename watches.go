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
    logDb("open %s", path)
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
    logDb("add chat=%d type=%s id=%s", chatID, typ, watchID)
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

func (s *watchStore) list() ([]watchRecord, error) {
    logDb("list")
    var records []watchRecord
    var err = s.db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket(watchesBucket).ForEach(func(k, v []byte) error {
            var r watchRecord
            if err := json.Unmarshal(v, &r); err != nil { return err }
            records = append(records, r)
            return nil
        })
    })
    if err != nil { return nil, err }
    return records, nil
}

// remove deletes every record matching both chatID and watchID (the chatID
// scoping is what stops one chat from removing another chat's watch) and
// returns how many were deleted. Keys are collected before deleting because
// bbolt forbids mutating a bucket during ForEach.
func (s *watchStore) remove(chatID int64, watchID string) (int, error) {
    logDb("remove chat=%d id=%s", chatID, watchID)
    var removed int
    var err = s.db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(watchesBucket)
        var keys [][]byte
        var err = b.ForEach(func(k, v []byte) error {
            var r watchRecord
            if err := json.Unmarshal(v, &r); err != nil { return err }
            if r.ChatID == chatID && r.WatchID == watchID {
                keys = append(keys, append([]byte(nil), k...))
            }
            return nil
        })
        if err != nil { return err }
        for _, k := range keys {
            if err := b.Delete(k); err != nil { return err }
            removed++
        }
        return nil
    })
    if err != nil { return 0, err }
    return removed, nil
}

func itob(v uint64) []byte {
    var buf = make([]byte, 8)
    binary.BigEndian.PutUint64(buf, v)
    return buf
}

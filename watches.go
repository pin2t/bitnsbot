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

func addWatch(chatID int64, typ watchType, watchID string) error {
    logDb("add chat=%d type=%s id=%s", chatID, typ, watchID)
    var data, err = json.Marshal(watchRecord{
        CreatedAt: time.Now(),
        ChatID:    chatID,
        Type:      typ,
        WatchID:   watchID,
    })
    if err != nil { return err }
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(watchesBucket)
        var id, err = b.NextSequence()
        if err != nil { return err }
        return b.Put(itob(id), data)
    })
}

func listWatches() ([]watchRecord, error) {
    logDb("list")
    var records []watchRecord
    var err = db.View(func(tx *bbolt.Tx) error {
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

// removeWatch deletes every record matching both chatID and watchID (the chatID
// scoping is what stops one chat from removing another chat's watch) and
// returns how many were deleted. Keys are collected before deleting because
// bbolt forbids mutating a bucket during ForEach.
func removeWatch(chatID int64, watchID string) (int, error) {
    logDb("remove chat=%d id=%s", chatID, watchID)
    var removed int
    var err = db.Update(func(tx *bbolt.Tx) error {
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

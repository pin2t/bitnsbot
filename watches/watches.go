package watches

import "encoding/binary"
import "time"

import "go.etcd.io/bbolt"
import "github.com/vmihailenco/msgpack/v5"
import "bitnsbot/logging"

var db *bbolt.DB
var bucket = []byte("watches")

// Watch is the public view of a stored address watch — the internal record type
// is not exposed.
type Watch struct {
    ChatID  int64
    Address string
    Alias   string
}

// watchRecord is the stored form. Only address watches are persisted (transaction
// watches live in memory), so there is no type field; the WatchID JSON key is
// kept for compatibility with records written before this refactor.
type watchRecord struct {
    CreatedAt time.Time
    ChatID    int64
    WatchID   string
    Alias     string
}

// Init stores the shared bbolt handle and ensures the watches bucket exists.
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(bucket)
        return err
    })
}

func itob(v uint64) []byte {
    var buf = make([]byte, 8)
    binary.BigEndian.PutUint64(buf, v)
    return buf
}

// Add stores an address watch for a chat under an auto-incrementing key.
func Add(chatID int64, address, alias string) error {
    logging.Db("add chat=%d address=%s alias=%s", chatID, address, alias)
    var data, err = msgpack.Marshal(watchRecord{
        CreatedAt: time.Now(),
        ChatID:    chatID,
        WatchID:   address,
        Alias:     alias,
    })
    if err != nil { return err }
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        var id, err = b.NextSequence()
        if err != nil { return err }
        return b.Put(itob(id), data)
    })
}

// List returns every stored watch, oldest first.
func List() ([]Watch, error) {
    logging.Db("list")
    var watches []Watch
    var err = db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket(bucket).ForEach(func(k, v []byte) error {
            var r watchRecord
            if err := msgpack.Unmarshal(v, &r); err != nil { return err }
            watches = append(watches, Watch{ChatID: r.ChatID, Address: r.WatchID, Alias: r.Alias})
            return nil
        })
    })
    if err != nil { return nil, err }
    return watches, nil
}

// Remove deletes every watch matching both chatID and address (the chatID
// scoping is what stops one chat from removing another chat's watch) and returns
// how many were deleted. Keys are collected before deleting because bbolt forbids
// mutating a bucket during ForEach.
func Remove(chatID int64, address string) (int, error) {
    logging.Db("remove chat=%d address=%s", chatID, address)
    var removed int
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        var keys [][]byte
        var err = b.ForEach(func(k, v []byte) error {
            var r watchRecord
            if err := msgpack.Unmarshal(v, &r); err != nil { return err }
            if r.ChatID == chatID && r.WatchID == address {
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

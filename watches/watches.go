package watches

import "bytes"
import "encoding/json"
import "sort"
import "strconv"
import "strings"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/logging"

var db *bbolt.DB
var bucket = []byte("watches")

// Watch is the public view of a stored address watch — the internal record type
// is not exposed.
type Watch struct {
    Chat    int64
    Address string
    Alias   string
}

// watchRecord is the stored value. The chat and the address are not in it: they
// are the key, so the value holds only what is left — when the watch was made
// and what the user called it.
type watchRecord struct {
    Created int64  `json:"created"`
    Alias   string `json:"alias"`
}

// key is "<chat>,<address>" as text. Making the pair the key is what stops one
// chat holding the same address twice: the old auto-incrementing key allowed it,
// and every read had to filter for it. Counting a chat's watches becomes a
// prefix scan, and removing or renaming one becomes a single lookup.
//
// The separator is safe because an address is base58 or bech32 and a chat id is
// digits with an optional minus, so neither can contain a comma — and the key is
// split on the *first* one regardless, so even one that did would round-trip.
func key(chatID int64, address string) []byte {
    return []byte(strconv.FormatInt(chatID, 10) + "," + address)
}

func parseKey(k []byte) (int64, string, bool) {
    var chat, address, found = strings.Cut(string(k), ",")
    if !found || address == "" { return 0, "", false }
    var id, err = strconv.ParseInt(chat, 10, 64)
    if err != nil { return 0, "", false }
    return id, address, true
}

// Init stores the shared bbolt handle, ensures the watches bucket exists, and
// carries any record still in the old format across.
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(bucket)
        if err != nil { return err }
        var moved, merr = migrate(tx)
        if merr != nil { return merr }
        if moved > 0 { logging.Status("watches: moved %d records to the chat,address key", moved) }
        return nil
    })
}

// migrate rewrites the records written under the old format — an
// auto-incrementing numeric key, with the chat and the address inside the value
// — under the key they belong to now. A record already in the new format is
// recognised by its key parsing, and left alone, so this is a no-op on every
// start after the first.
//
// Two old records can name the same chat and address, which the old format
// allowed and the new key cannot: they are written in the order the bucket holds
// them, which is the order they were added, so the newest wins and its alias is
// the one that survives.
//
// A value that does not decode at all is left where it is rather than dropped —
// it is not this function's to throw away, and a bucket that lost records
// silently would be worse than one with a stray key in it.
func migrate(tx *bbolt.Tx) (int, error) {
    var b = tx.Bucket(bucket)
    type stored struct {
        Created int64  `json:"created"`
        Chat    int64  `json:"chat"`
        Watch   string `json:"watch"`
        Alias   string `json:"alias"`
    }
    var oldKeys [][]byte
    var records []stored
    var err = b.ForEach(func(k, v []byte) error {
        if _, _, ok := parseKey(k); ok { return nil }
        var r stored
        if json.Unmarshal(v, &r) != nil || r.Watch == "" {
            logging.Warn("watches: leaving a record this cannot read under key %x", k)
            return nil
        }
        oldKeys = append(oldKeys, append([]byte(nil), k...))
        records = append(records, r)
        return nil
    })
    if err != nil { return 0, err }
    for _, r := range records {
        var data, merr = json.Marshal(watchRecord{Created: r.Created, Alias: r.Alias})
        if merr != nil { return 0, merr }
        if err := b.Put(key(r.Chat, r.Watch), data); err != nil { return 0, err }
    }
    // after the writes, since one of the old keys could otherwise be a new key
    // that was just written — they cannot collide in practice, but deleting last
    // is what makes that true rather than lucky
    for _, k := range oldKeys {
        if err := b.Delete(k); err != nil { return 0, err }
    }
    return len(records), nil
}

// Add stores an address watch for a chat. Watching an address the chat already
// watches replaces the record rather than adding a second one, and keeps the
// time the watch was first made: it is the same watch, and resetting its age
// would be a lie.
func Add(chatID int64, address, alias string) error {
    logging.Db("add chat=%d address=%s alias=%s", chatID, address, alias)
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        var k = key(chatID, address)
        var rec = watchRecord{Created: time.Now().Unix(), Alias: alias}
        if prev := b.Get(k); prev != nil {
            var old watchRecord
            if json.Unmarshal(prev, &old) == nil && old.Created > 0 { rec.Created = old.Created }
        }
        var data, err = json.Marshal(rec)
        if err != nil { return err }
        return b.Put(k, data)
    })
}

// Count returns how many address watches chatID currently has. The key carries
// the chat, so this is a scan of that chat's own keys rather than a walk of the
// whole bucket decoding every record.
func Count(chatID int64) (int, error) {
    logging.Db("count chat=%d", chatID)
    var count int
    var prefix = []byte(strconv.FormatInt(chatID, 10) + ",")
    var err = db.View(func(tx *bbolt.Tx) error {
        var c = tx.Bucket(bucket).Cursor()
        for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
            count++
        }
        return nil
    })
    if err != nil { return 0, err }
    return count, nil
}

// List returns every stored watch, oldest first. The keys sort by chat and then
// by address, so the order comes from the stored time instead — stably, since
// that is a whole second and several watches can share one.
func List() ([]Watch, error) {
    logging.Db("list")
    // The time is carried alongside the watch rather than in a slice of its own,
    // so that sorting moves the two together.
    type entry struct {
        watch   Watch
        created int64
    }
    var entries []entry
    var err = db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket(bucket).ForEach(func(k, v []byte) error {
            var chat, address, ok = parseKey(k)
            if !ok { return nil }
            var r watchRecord
            if json.Unmarshal(v, &r) != nil { return nil }
            entries = append(entries, entry{Watch{Chat: chat, Address: address, Alias: r.Alias}, r.Created})
            return nil
        })
    })
    if err != nil { return nil, err }
    sort.SliceStable(entries, func(i, j int) bool { return entries[i].created < entries[j].created })
    var watches []Watch
    for _, e := range entries { watches = append(watches, e.watch) }
    return watches, nil
}

// SetAlias renames the watch on chatID's address — the chat is half the key, so
// one chat cannot rename another's — and reports whether there was one to
// rename.
func SetAlias(chatID int64, address, alias string) (int, error) {
    logging.Db("set alias chat=%d address=%s alias=%s", chatID, address, alias)
    var renamed int
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        var k = key(chatID, address)
        var v = b.Get(k)
        if v == nil { return nil }
        var r watchRecord
        if err := json.Unmarshal(v, &r); err != nil { return err }
        r.Alias = alias
        var data, merr = json.Marshal(r)
        if merr != nil { return merr }
        if err := b.Put(k, data); err != nil { return err }
        renamed = 1
        return nil
    })
    if err != nil { return 0, err }
    return renamed, nil
}

// Remove deletes chatID's watch on address (the chat being half the key is what
// stops one chat from removing another chat's watch) and reports whether there
// was one to delete — bbolt's Delete does not say, and the caller tells "removed"
// from "you were not watching that" by the count.
func Remove(chatID int64, address string) (int, error) {
    logging.Db("remove chat=%d address=%s", chatID, address)
    var removed int
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        var k = key(chatID, address)
        if b.Get(k) == nil { return nil }
        if err := b.Delete(k); err != nil { return err }
        removed = 1
        return nil
    })
    if err != nil { return 0, err }
    return removed, nil
}

// Package addrindex is a compact address→transactions index, built because
// Bitcoin Core (unlike btcd's --addrindex) has no address index at all and the
// bot's /info <address> history stats need one.
//
// The scheme follows electrs/bindex-rs — hash each scriptPubKey, keep only a
// short prefix, and record one entry per "touch" (an address appearing in an
// output, or in a spent input) — but the *storage layout* is bbolt's, not
// RocksDB's, and that difference is the whole design.
//
// bindex stores one RocksDB row per touch, keyed `scriptHashPrefix(8) +
// globalTxNum(4)` with an empty value. That works because an LSM tree shares key
// prefixes across sorted runs and compresses them. bbolt is a B+tree: it neither
// compacts nor compresses, and every key pays a per-element page cost.
//
// A first attempt keyed the index by address, with the value an append-only list
// of that address's touches, on the theory that the per-key cost would amortize
// across repeated touches. Measured against real mainnet blocks it did not:
// 71% of addresses are touched exactly once and 96% no more than twice, so there
// was almost nothing to amortize, and the real cost came out at ~133 bytes per
// address — against 30 bytes of actual content — for a projected 150-250 GB.
//
// So addresses are not keyed individually at all. Touches are **sharded**: the
// key is the first two bytes of the script hash plus a block-range number, and
// the value is the packed run of every touch in that shard and range. That gives
// tens of millions of keys instead of over a billion, so per-key overhead stops
// mattering, and the cost falls to the ~10 bytes per touch the data actually
// needs (about 60 GB for mainnet's ~6.2 billion touches).
//
// Two further properties fall out of sharding, both of which the keyed-by-address
// layout lacked:
//   - Writes are **append-only**. Each (shard, range) value is written once, when
//     that range of blocks is indexed, and never rewritten. The earlier layout
//     rewrote an address's whole value on every touch, which is why it needed a
//     history cap to avoid quadratic rewrites on exchange-hot addresses. That cap
//     is gone; history is now complete.
//   - A lookup is one contiguous cursor scan. Keys sort by shard first, so all of
//     one shard's ranges sit together in the tree.
//
// Two deliberate consequences, both matching what the btcd path already did:
//   - The 8-byte script-hash prefix can collide, so a reader must confirm each
//     resolved transaction actually involves the address (post-filtering).
//   - A touch stores (height, txIndex), not bindex's global tx number, which
//     avoids a second txNum→txid index: it resolves straight to a txid via one
//     getblock at verbosity 1.
package addrindex

import "bytes"
import "crypto/sha256"
import "encoding/binary"
import "errors"
import "strconv"

import "go.etcd.io/bbolt"

var db *bbolt.DB
var bucket = []byte("addrindex")
var cursorBucket = []byte("addrindex-cursor")

// prefixLen is the script-hash prefix width, the same 8 bytes bindex uses: wide
// enough that collisions are astronomically rare, narrow enough to stay small.
// The first shardLen bytes select the shard; the rest is stored per touch to
// tell apart the addresses sharing that shard.
const prefixLen = 8
const shardLen = 2
const remainderLen = prefixLen - shardLen

// rangeBlocks is how many blocks share one key. It trades three things at once:
// the number of keys (fewer is less overhead), how much a lookup must read (one
// key per range per shard), and how much memory a backfill chunk holds before
// flushing. 1000 keeps a range's height offset inside a uint16, matches the
// backfill chunk size so each key is written exactly once, and puts a mainnet
// lookup at roughly a megabyte.
const rangeBlocks = 1000

// entryLen is one packed touch: the script hash's remaining bytes, the block's
// offset within the range, and the transaction's index in that block.
const entryLen = remainderLen + 2 + 2

// Touch is one appearance of an address in the chain: an output paying it or an
// input spending from it, located by block height and the transaction's index in
// that block.
type Touch struct {
    Height  uint32
    TxIndex uint16
}

// Init stores the shared bbolt handle and ensures the index buckets exist.
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        for _, name := range [][]byte{bucket, cursorBucket} {
            if _, err := tx.CreateBucketIfNotExists(name); err != nil { return err }
        }
        return nil
    })
}

// Prefix is the index prefix for a scriptPubKey: the first prefixLen bytes of
// its SHA-256, the same script hash electrs derives (before its display
// reversal).
func Prefix(script []byte) []byte {
    var sum = sha256.Sum256(script)
    return sum[:prefixLen]
}

// key is the storage key for a shard and block range: shard first so every range
// of one shard sorts together and a lookup is a single contiguous scan.
func key(prefix []byte, rangeIndex uint32) []byte {
    var k = make([]byte, shardLen+4)
    copy(k, prefix[:shardLen])
    binary.BigEndian.PutUint32(k[shardLen:], rangeIndex)
    return k
}

func rangeOf(height uint32) uint32 { return height / rangeBlocks }

func encodeEntry(prefix []byte, t Touch) []byte {
    var e = make([]byte, entryLen)
    copy(e, prefix[shardLen:prefixLen])
    binary.BigEndian.PutUint16(e[remainderLen:], uint16(t.Height%rangeBlocks))
    binary.BigEndian.PutUint16(e[remainderLen+2:], t.TxIndex)
    return e
}

// merge folds a chunk of touches into the index in one transaction. Touches are
// grouped by (shard, range) and appended to that key's packed run. When the
// chunk covers whole ranges — which the backfill's chunking arranges — each key
// is written exactly once and never rewritten; appending only happens where a
// resumed backfill picks up mid-range.
func merge(touches map[string][]Touch, height int) error {
    // Not a silent no-op: a write path that reports success while discarding
    // everything is how a missing Init went unnoticed through a whole chain
    // backfill. Reads may degrade quietly; writes must not.
    if db == nil { return errors.New("addrindex: not initialised (addrindex.Init was never called)") }
    var grouped = make(map[string][]byte)
    for prefix, list := range touches {
        for _, t := range list {
            var k = string(key([]byte(prefix), rangeOf(t.Height)))
            grouped[k] = append(grouped[k], encodeEntry([]byte(prefix), t)...)
        }
    }
    return db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(bucket)
        for k, entries := range grouped {
            var existing = b.Get([]byte(k))
            var buf = make([]byte, 0, len(existing)+len(entries))
            buf = append(buf, existing...)
            buf = append(buf, entries...)
            if err := b.Put([]byte(k), buf); err != nil { return err }
        }
        return updateCursor(tx, height)
    })
}

// Lookup returns an address's touches, oldest first, and whether the result hit
// limit (so the caller can flag partial history). It seeks to the address's
// shard and walks that shard's ranges in order, keeping only the entries whose
// stored remainder matches, since a shard holds every address whose hash starts
// with the same two bytes.
func Lookup(script []byte, limit int) (touches []Touch, capped bool) {
    if db == nil { return nil, false }
    var prefix = Prefix(script)
    var remainder = prefix[shardLen:prefixLen]
    db.View(func(tx *bbolt.Tx) error {
        var c = tx.Bucket(bucket).Cursor()
        for k, v := c.Seek(prefix[:shardLen]); k != nil && bytes.HasPrefix(k, prefix[:shardLen]); k, v = c.Next() {
            var base = binary.BigEndian.Uint32(k[shardLen:]) * rangeBlocks
            for i := 0; i+entryLen <= len(v); i += entryLen {
                if !bytes.Equal(v[i:i+remainderLen], remainder) { continue }
                if len(touches) >= limit {
                    capped = true
                    return nil
                }
                touches = append(touches, Touch{
                    Height:  base + uint32(binary.BigEndian.Uint16(v[i+remainderLen:])),
                    TxIndex: binary.BigEndian.Uint16(v[i+remainderLen+2:]),
                })
            }
        }
        return nil
    })
    return touches, capped
}

func updateCursor(tx *bbolt.Tx, height int) error { return setCursor(tx, "cursor", height) }

func setCursor(tx *bbolt.Tx, name string, height int) error {
    return tx.Bucket(cursorBucket).Put([]byte(name), []byte(strconv.FormatInt(int64(height), 10)))
}

func Cursor() (h int, ok bool) { return GetCursor("cursor") }

// GetCursor and SetCursor read and write a named cursor in the cursor bucket.
// The index's own is "cursor"; a second pass over the chain — tools/addrindex's
// actbuild — keeps its place beside it under its own name, so neither disturbs
// the other.
func GetCursor(name string) (h int, ok bool) {
    if db == nil { return 0, false }
    db.View(func(tx *bbolt.Tx) error {
        if v := tx.Bucket(cursorBucket).Get([]byte(name)); v != nil {
            var hh, err = strconv.ParseInt(string(v), 10, 64)
            if err == nil { h, ok = int(hh), true }
        }
        return nil
    })
    return
}

// SetCursorIn writes a named cursor inside the caller's transaction, so a pass
// can advance its place atomically with the batch that reached it.
func SetCursorIn(tx *bbolt.Tx, name string, height int) error { return setCursor(tx, name, height) }

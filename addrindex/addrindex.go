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
import "strconv"
import "errors"

import "database/sql"

import "go.etcd.io/bbolt"
import "bitnsbot/cursors"
import "bitnsbot/logging"

// Two stores, one index. db is bbolt, which tools/addrindex drives — the tool
// cannot be handed the bot's SQLite handle, and the bot has no reason to keep a
// bbolt file. sqldb is the bot's, the `addrindex` table tools/tosqlite writes. Each
// read and write below takes whichever is set; exactly one ever is.
var db *bbolt.DB
var sqldb *sql.DB
var bucket = []byte("addrindex")

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

// The index keeps its place in a bucket of its own file rather than in the bot's
// cursors table: tools/addrindex builds the same index into the same buckets, and
// a cursor belongs with the touches it describes. The names are what they have
// always been, so an index built by either side resumes where the other left it.
var cursorBucket = []byte("cursors")

// AddrIndexCursor is the build's own place. ActBuildCursor is a second pass over
// the chain — tools/addrindex's actbuild, which counts files rather than heights,
// deliberately under a name of its own so a place written by one is never read as
// the other's.
const AddrIndexCursor = "addrindex"
const ActBuildCursor = "actbuild-file"

// Touch is one appearance of an address in the chain: an output paying it or an
// input spending from it, located by block height and the transaction's index in
// that block.
type Touch struct {
    Height  uint32
    TxIndex uint16
}

// Init stores the bbolt handle and ensures the index bucket exists. This is
// tools/addrindex's only setup call, which is why that tool still resumes where it
// left off rather than rebuilding the chain.
func Init(handle *bbolt.DB) error {
    db = handle
    return db.Update(func(tx *bbolt.Tx) error {
        var _, err = tx.CreateBucketIfNotExists(bucket)
        return err
    })
}

// InitSQL stores the SQLite handle instead, which is what the bot does: its index
// is the `addrindex` table, beside everything else it keeps. The table is created
// by openDB, from the schema tools/tosqlite defines, and the build's place is a row
// of the shared cursors table rather than a bucket of its own.
func InitSQL(handle *sql.DB) error {
    sqldb = handle
    return nil
}

// sqlKey packs a shard and a block range into the one integer the table is keyed
// by: the 2-byte shard above the 4-byte range index, so a shard's ranges are
// contiguous and in order — which is what makes a lookup the same single scan the
// bbolt cursor does. tools/tosqlite packs a migrated key the same way, and
// tools/addrindex's own SQLite reader unpacks it, so the three must agree.
func sqlKey(prefix []byte, rangeIndex uint32) int64 {
    return int64(binary.BigEndian.Uint16(prefix[:shardLen]))<<32 | int64(rangeIndex)
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
    if db == nil && sqldb == nil {
        return errors.New("addrindex: not initialised (neither Init nor InitSQL was called)")
    }
    var grouped = make(map[string][]byte)
    for prefix, list := range touches {
        for _, t := range list {
            var k = string(key([]byte(prefix), rangeOf(t.Height)))
            grouped[k] = append(grouped[k], encodeEntry([]byte(prefix), t)...)
        }
    }
    if sqldb != nil { return mergeSQL(grouped, height) }
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

// mergeSQL is the same append, in one statement per key: the run is concatenated
// onto whatever the row holds. The `cast` is load-bearing — SQLite's `||` is
// byte-exact over blobs, NULs included, but its result is TEXT, so without it the
// column's type drifts from what the schema says it is.
//
// One transaction for the batch and the cursor together, the same crash-safety the
// bbolt path has: a run that is written and a place that is not would be counted
// twice on the next pass.
func mergeSQL(grouped map[string][]byte, height int) error {
    var tx, err = sqldb.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    var stmt, perr = tx.Prepare(`insert into addrindex (shard, data) values (?, ?)
        on conflict(shard) do update set data = cast(addrindex.data || excluded.data as blob)`)
    if perr != nil { return perr }
    for k, entries := range grouped {
        // the key is the bbolt one, packed again as the integer the table uses
        var raw = []byte(k)
        var shard = int64(binary.BigEndian.Uint16(raw[:shardLen]))<<32 | int64(binary.BigEndian.Uint32(raw[shardLen:]))
        if _, err := stmt.Exec(shard, entries); err != nil {
            stmt.Close()
            return err
        }
    }
    if err := stmt.Close(); err != nil { return err }
    if err := cursors.Set(tx, cursors.AddrIndex, int64(height)); err != nil { return err }
    return tx.Commit()
}

// Lookup returns an address's touches, oldest first, and whether the result hit
// limit (so the caller can flag partial history). It seeks to the address's
// shard and walks that shard's ranges in order, keeping only the entries whose
// stored remainder matches, since a shard holds every address whose hash starts
// with the same two bytes.
func Lookup(script []byte, limit int) (touches []Touch, capped bool) {
    var prefix = Prefix(script)
    var remainder = prefix[shardLen:prefixLen]
    if sqldb != nil { return lookupSQL(prefix, remainder, limit) }
    if db == nil { return nil, false }
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

// lookupSQL is the same walk over the same order: a shard's ranges are the
// integer keys from shard<<32 to the next shard's, so one indexed scan of the
// primary key reads them oldest first.
func lookupSQL(prefix, remainder []byte, limit int) (touches []Touch, capped bool) {
    var shard = int64(binary.BigEndian.Uint16(prefix[:shardLen]))
    var rows, err = sqldb.Query("select shard, data from addrindex where shard >= ? and shard < ? order by shard",
        shard<<32, (shard+1)<<32)
    if err != nil {
        logging.Warn("addrindex: lookup: %v", err)
        return nil, false
    }
    defer rows.Close()
    for rows.Next() {
        var k int64
        var v []byte
        if err := rows.Scan(&k, &v); err != nil { return touches, capped }
        var base = uint32(k&0xffffffff) * rangeBlocks
        for i := 0; i+entryLen <= len(v); i += entryLen {
            if !bytes.Equal(v[i:i+remainderLen], remainder) { continue }
            if len(touches) >= limit { return touches, true }
            touches = append(touches, Touch{
                Height:  base + uint32(binary.BigEndian.Uint16(v[i+remainderLen:])),
                TxIndex: binary.BigEndian.Uint16(v[i+remainderLen+2:]),
            })
        }
    }
    return touches, capped
}

func updateCursor(tx *bbolt.Tx, height int) error { return SetCursorIn(tx, AddrIndexCursor, height) }

// Cursor is where the build has got to, out of whichever store holds the index.
func Cursor() (h int, ok bool) {
    if sqldb != nil {
        var v, found = cursors.Get(cursors.AddrIndex)
        return int(v), found
    }
    return GetCursor(AddrIndexCursor)
}

// GetCursor and SetCursorIn read and write a named cursor in the shared cursors
// bucket. The index's own is cursors.AddrIndex; a second pass over the chain —
// tools/addrindex's actbuild — keeps its place beside it under its own name, so
// neither disturbs the other.
func GetCursor(name string) (h int, ok bool) {
    if db == nil { return 0, false }
    var v int64
    var found bool
    db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket(cursorBucket)
        if b == nil { return nil }
        if raw := b.Get([]byte(name)); raw != nil {
            var n, perr = strconv.ParseInt(string(raw), 10, 64)
            if perr == nil { v, found = n, true }
        }
        return nil
    })
    return int(v), found
}

// SetCursorIn writes a named cursor inside the caller's transaction, so a pass
// can advance its place atomically with the batch that reached it.
func SetCursorIn(tx *bbolt.Tx, name string, height int) error {
    var b, err = tx.CreateBucketIfNotExists(cursorBucket)
    if err != nil { return err }
    return b.Put([]byte(name), []byte(strconv.FormatInt(int64(height), 10)))
}

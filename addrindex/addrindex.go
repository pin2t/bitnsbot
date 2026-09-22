// Package addrindex is a compact address→transactions index, built because
// Bitcoin Core (unlike btcd's --addrindex) has no address index at all and the
// bot's /info <address> history stats need one.
//
// The scheme follows electrs/bindex-rs — hash each scriptPubKey, keep only a
// short prefix, and record one entry per "touch" (an address appearing in an
// output, or in a spent input) — but the *storage layout* is a B-tree's, not
// RocksDB's, and that difference is the whole design.
//
// bindex stores one RocksDB row per touch, keyed `scriptHashPrefix(8) +
// globalTxNum(4)` with an empty value. That works because an LSM tree shares key
// prefixes across sorted runs and compresses them. A B-tree — bbolt's, which this
// was first built on, or SQLite's, which holds it now — neither compacts nor
// compresses, and every key pays a per-row page cost.
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
//   - A lookup is one contiguous scan of the primary key. Keys sort by shard
//     first, so all of one shard's ranges sit together in the tree.
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
import "database/sql"
import "bitnsbot/cursors"
import "bitnsbot/logging"

// db is the database holding the `addrindex` table, and the cursors table the
// build keeps its place in: the bot's own, or the file tools/addrindex builds.
var db *sql.DB

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

// Init stores the handle. The `addrindex` and `cursors` tables are the caller's
// to create — openDB's in the bot, the tool's own in tools/addrindex — and the
// cursors package has to be given the same handle, since the build's place is a
// row of that table under cursors.AddrIndex.
func Init(handle *sql.DB) error {
    db = handle
    return nil
}

// Prefix is the index prefix for a scriptPubKey: the first prefixLen bytes of
// its SHA-256, the same script hash electrs derives (before its display
// reversal).
func Prefix(script []byte) []byte {
    var sum = sha256.Sum256(script)
    return sum[:prefixLen]
}

// key is the storage key for a shard and block range: the 2-byte shard above the
// 4-byte range index, read as one integer, so every range of one shard sorts
// together and in order and a lookup is a single contiguous scan. These are the
// six bytes the index's bbolt key held, big-endian, which is what keeps an index
// migrated from bbolt readable.
func key(prefix []byte, rangeIndex uint32) int64 {
    return int64(binary.BigEndian.Uint16(prefix[:shardLen]))<<32 | int64(rangeIndex)
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
// grouped by (shard, range) and appended to that key's packed run, one statement
// per key. When the chunk covers whole ranges — which the backfill's chunking
// arranges — each key is written exactly once and never rewritten; appending only
// happens where a resumed backfill picks up mid-range.
//
// The `cast` is load-bearing: SQLite's `||` is byte-exact over blobs, NULs
// included, but its result is TEXT, so without it the column's type drifts from
// what the schema says it is.
//
// The batch and the cursor commit together: a run that is written and a place
// that is not would be counted twice on the next pass.
//
// Not a silent no-op: a write path that reports success while discarding
// everything is how a missing Init went unnoticed through a whole chain
// backfill. Reads may degrade quietly; writes must not.
func merge(touches map[string][]Touch, height int) error {
    if db == nil { return errors.New("addrindex: not initialised (Init was not called)") }
    var grouped = make(map[int64][]byte)
    for prefix, list := range touches {
        for _, t := range list {
            var k = key([]byte(prefix), rangeOf(t.Height))
            grouped[k] = append(grouped[k], encodeEntry([]byte(prefix), t)...)
        }
    }
    var tx, err = db.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    var stmt, perr = tx.Prepare(`insert into addrindex (shard, data) values (?, ?)
        on conflict(shard) do update set data = cast(addrindex.data || excluded.data as blob)`)
    if perr != nil { return perr }
    for k, entries := range grouped {
        if _, err := stmt.Exec(k, entries); err != nil {
            stmt.Close()
            return err
        }
    }
    if err := stmt.Close(); err != nil { return err }
    if err := cursors.Set(tx, cursors.AddrIndex, int64(height)); err != nil { return err }
    return tx.Commit()
}

// Lookup returns an address's touches, oldest first, and whether the result hit
// limit (so the caller can flag partial history). A shard's ranges are the
// integer keys from shard<<32 to the next shard's, so one scan of the primary key
// reads them in order, keeping only the entries whose stored remainder matches,
// since a shard holds every address whose hash starts with the same two bytes.
func Lookup(script []byte, limit int) (touches []Touch, capped bool) {
    if db == nil { return nil, false }
    var prefix = Prefix(script)
    var remainder = prefix[shardLen:prefixLen]
    var rows, err = db.Query("select shard, data from addrindex where shard >= ? and shard < ? order by shard",
        key(prefix, 0), key(prefix, 0)+1<<32)
    if err != nil {
        logging.Warn("addrindex: lookup: %v", err)
        return nil, false
    }
    defer rows.Close()
    for rows.Next() {
        var k int64
        var v []byte
        if err := rows.Scan(&k, &v); err != nil { return touches, capped }
        var base = uint32(k) * rangeBlocks
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

// LookupFrom returns up to n of an address's most recent touches at or below
// height, newest first. The SQL reads the shard's ranges in reverse — order by
// shard desc, starting at the range holding height — so the ranges arrive
// newest first already; only the entries inside one range value, which are
// stored oldest first, are walked back to front. Nothing is gathered and then
// reversed whole. The scan stops once n+1 touches are collected, so a very
// active address does not force reading its whole history. capped is true when
// older touches remain behind the ones returned.
func LookupFrom(script []byte, height uint32, n int) (touches []Touch, capped bool) {
    if db == nil { return nil, false }
    var prefix = Prefix(script)
    var remainder = prefix[shardLen:prefixLen]
    var rows, err = db.Query("select shard, data from addrindex where shard >= ? and shard < ? order by shard desc",
        key(prefix, rangeOf(height)), key(prefix, 0)+1<<32)
    if err != nil {
        logging.Warn("addrindex: lookup from: %v", err)
        return nil, false
    }
    defer rows.Close()
    for rows.Next() {
        var k int64
        var v []byte
        if err := rows.Scan(&k, &v); err != nil { return touches, capped }
        var base = uint32(k) * rangeBlocks
        for i := len(v) - entryLen; i >= 0; i -= entryLen {
            if !bytes.Equal(v[i:i+remainderLen], remainder) { continue }
            var h = base + uint32(binary.BigEndian.Uint16(v[i+remainderLen:]))
            if height > 0 && h > height { continue }
            touches = append(touches, Touch{
                Height:  h,
                TxIndex: binary.BigEndian.Uint16(v[i+remainderLen+2:]),
            })
            if len(touches) > n {
                return touches[:n], true
            }
        }
    }
    return touches, false
}

// Cursor is where the build has got to.
func Cursor() (h int, ok bool) {
    var v, found = cursors.Get(cursors.AddrIndex)
    return int(v), found
}

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
import "context"
import "crypto/sha256"
import "database/sql"
import "encoding/binary"
import "errors"
import "fmt"
import "sync"
import "time"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/progress"

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

// Init stores the handle and creates the two tables when they are missing:
// `addrindex`, the index up to the build's cursor, and `recaddrindex`, the newest
// blocks above it, staged one row per (shard, block) so a block a reorg
// disconnects can still be taken out (see Rollback). The cursors package has to
// be given the same handle first, since the build's place is a row of its table
// under cursors.AddrIndex.
//
// recaddrindex's shard is the 2-byte shard alone, and its block the height, so
// an address's staged touches are a prefix of the primary key. Its data is
// packed exactly as addrindex's is, which is what lets a fold append it to the
// range key unchanged. Every block read has a row, if only the one under shard
// -1 that marks it read: a block touching no script would otherwise look unread.
func Init(handle *sql.DB) error {
    db = handle
    if db == nil { return nil }
    for _, s := range []string{
        `create table if not exists addrindex (shard INTEGER PRIMARY KEY, data BLOB NOT NULL)`,
        `create table if not exists recaddrindex (shard INTEGER NOT NULL, block INTEGER NOT NULL, data BLOB NOT NULL,
            PRIMARY KEY (shard, block)) without rowid`,
        `create index if not exists recaddrindex_block on recaddrindex (block)`,
    } {
        if _, err := db.Exec(s); err != nil { return err }
    }
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

// errNoInit is what a write to an index nobody opened returns.
var errNoInit = errors.New("addrindex: not initialised (Init was not called)")

// merge folds a chunk of touches into the index in one transaction, the build's
// path for every block deeper than `recent`. Touches are grouped by (shard,
// range) and appended to that key's packed run (write). It holds mu, so a
// Rollback cannot judge a block against a cursor this is about to move.
//
// Not a silent no-op: a write path that reports success while discarding
// everything is how a missing Init went unnoticed through a whole chain
// backfill. Reads may degrade quietly; writes must not.
func merge(touches map[string][]Touch, height int) error {
    if db == nil { return errNoInit }
    var grouped = make(map[int64][]byte)
    for prefix, list := range touches {
        for _, t := range list {
            var k = key([]byte(prefix), rangeOf(t.Height))
            grouped[k] = append(grouped[k], encodeEntry([]byte(prefix), t)...)
        }
    }
    mu.Lock()
    defer mu.Unlock()
    return write(grouped, height)
}

// write appends each key's entries to its packed run, one statement per key, and
// moves the cursor to height, in one transaction. When a chunk covers whole
// ranges each key is written once and never rewritten; appending only happens
// where a resumed build or a fold picks up mid-range. Whatever recaddrindex holds
// at or below height is deleted in the same transaction, those blocks being
// counted in addrindex now. The caller holds mu.
//
// The `cast` is load-bearing: SQLite's `||` is byte-exact over blobs, NULs
// included, but its result is TEXT, so without it the column's type drifts from
// what the schema says it is.
//
// The batch and the cursor commit together: a run that is written and a place
// that is not would be counted twice on the next pass.
func write(grouped map[int64][]byte, height int) error {
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
    if _, derr := tx.Exec("delete from recaddrindex where block <= ?", height); derr != nil { return derr }
    if err := cursors.Set(tx, cursors.AddrIndex, int64(height)); err != nil { return err }
    return tx.Commit()
}

// shardRows hands each packed run of one shard to each, with the height its
// offsets count from, until each returns false: addrindex's ranges up to the key
// top, then recaddrindex's staged blocks up to the height topBlock — or, when
// desc, the staged blocks newest first and then the ranges. Each of the two
// queries walks its table's primary key, and both run in one read transaction,
// so a fold moving blocks from one table to the other cannot be seen half done.
func shardRows(prefix []byte, desc bool, top, topBlock int64, each func(base uint32, v []byte) bool) error {
    var tx, err = db.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    var order = ""
    if desc { order = " desc" }
    var queries = []struct {
        sql   string
        args  []any
        base  func(k int64) uint32
    }{
        {"select shard, data from addrindex where shard >= ? and shard <= ? order by shard" + order,
            []any{key(prefix, 0), top}, func(k int64) uint32 { return uint32(k) * rangeBlocks }},
        {"select block, data from recaddrindex where shard = ? and block <= ? order by block" + order,
            []any{key(prefix, 0) >> 32, topBlock}, func(k int64) uint32 { return uint32(k) / rangeBlocks * rangeBlocks }},
    }
    if desc { queries[0], queries[1] = queries[1], queries[0] }
    for _, q := range queries {
        var rows, qerr = tx.Query(q.sql, q.args...)
        if qerr != nil { return qerr }
        for rows.Next() {
            var k int64
            var v []byte
            if serr := rows.Scan(&k, &v); serr != nil {
                rows.Close()
                return serr
            }
            if !each(q.base(k), v) {
                rows.Close()
                return nil
            }
        }
        rows.Close()
        if rerr := rows.Err(); rerr != nil { return rerr }
    }
    return nil
}

// Lookup returns an address's touches, oldest first, and whether the result hit
// limit (so the caller can flag partial history). A shard's ranges are the
// integer keys from shard<<32 to the next shard's, so one scan of the primary key
// reads them in order, and the shard's staged blocks follow, keeping only the
// entries whose stored remainder matches, since a shard holds every address
// whose hash starts with the same two bytes.
func Lookup(script []byte, limit int) (touches []Touch, capped bool) {
    if db == nil { return nil, false }
    var prefix = Prefix(script)
    var remainder = prefix[shardLen:prefixLen]
    var err = shardRows(prefix, false, key(prefix, 0)+1<<32-1, int64(1)<<62, func(b uint32, v []byte) bool {
        for i := 0; i+entryLen <= len(v); i += entryLen {
            if !bytes.Equal(v[i:i+remainderLen], remainder) { continue }
            if len(touches) >= limit {
                capped = true
                return false
            }
            touches = append(touches, Touch{
                Height:  b + uint32(binary.BigEndian.Uint16(v[i+remainderLen:])),
                TxIndex: binary.BigEndian.Uint16(v[i+remainderLen+2:]),
            })
        }
        return true
    })
    if err != nil { logging.Warn("addrindex: lookup: %v", err) }
    return touches, capped
}

// LookupFrom returns up to n of an address's most recent touches at or below
// height, newest first. The staged blocks are read newest first and then the
// shard's ranges in reverse, starting at the one holding height, so the rows
// arrive newest first already; only the entries inside one value, which are
// stored oldest first, are walked back to front. Nothing is gathered and then
// reversed whole. The scan stops once n+1 touches are collected, so a very
// active address does not force reading its whole history. capped is true when
// older touches remain behind the ones returned.
//
// The range holding height is the scan's upper bound, not its lower one: the
// ranges wanted are that one and every one below it. A height of 0 means no
// bound, so the scan starts at the shard's newest staged block.
func LookupFrom(script []byte, height uint32, n int) (touches []Touch, capped bool) {
    if db == nil { return nil, false }
    var prefix = Prefix(script)
    var remainder = prefix[shardLen:prefixLen]
    var top = key(prefix, 0) + 1<<32 - 1
    var topBlock = int64(1) << 62
    if height > 0 {
        top = key(prefix, rangeOf(height))
        topBlock = int64(height)
    }
    var err = shardRows(prefix, true, top, topBlock, func(b uint32, v []byte) bool {
        for i := len(v) - entryLen; i >= 0; i -= entryLen {
            if !bytes.Equal(v[i:i+remainderLen], remainder) { continue }
            var h = b + uint32(binary.BigEndian.Uint16(v[i+remainderLen:]))
            if height > 0 && h > height { continue }
            touches = append(touches, Touch{
                Height:  h,
                TxIndex: binary.BigEndian.Uint16(v[i+remainderLen+2:]),
            })
            if len(touches) > n {
                touches = touches[:n]
                capped = true
                return false
            }
        }
        return true
    })
    if err != nil { logging.Warn("addrindex: lookup from: %v", err) }
    return touches, capped
}

// Height is the newest block the index includes: the highest block staged in
// recaddrindex, or the cursor when nothing is staged. False when nothing has
// been indexed at all.
func Height() (h int, ok bool) {
    if db != nil {
        var staged sql.NullInt64
        if err := db.QueryRow("select max(block) from recaddrindex").Scan(&staged); err == nil && staged.Valid {
            return int(staged.Int64), true
        }
    }
    var v, found = cursors.Get(cursors.AddrIndex)
    return int(v), found
}

// chunkSize bounds how many blocks are merged into the index per
// transaction, the same reasoning as the miners collector: catching up hundreds
// of thousands of blocks must not build one giant transaction, and a crash
// mid-catch-up should resume from the last flushed chunk, not the beginning. A
// package var so tests shrink it.
var chunkSize int = 1000

// Build catches the index up to the current tip and returns. tools/addrindex
// calls it directly, so a command-line build and the bot's own catch-up advance
// the same cursor through the same chunking; the bot itself goes through Update,
// whose tip its caller has already fetched.
func Build() error {
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var count, err = core.GetBlockCount(ctx)
    if err != nil { return err }
    return Update(int64(count))
}

// Update advances the index from its cursor to tip and returns. The bot's one
// index catch-up goroutine calls it — blocks, miner statistics, addrindex,
// addrstat, addrbal, in that order — after fetching the tip once, so a
// genesis-to-tip build on a fresh index is a multi-hour, one-time cost paid the
// same way every other index pays its own catch-up.
//
// Three stages. First the staged blocks straight on from the cursor are folded
// into addrindex, as far as tip less `recent` (fold). Then every block from the
// cursor to tip less `recent` is read and merged straight into addrindex in
// chunks, staging nothing — the bulk of a first build, which a block a
// transaction would make many times slower. Last the newest `recent` blocks are
// staged in recaddrindex one block at a time (record), a height already staged
// skipped, so a block a reorg disconnects can still be taken out (Rollback).
//
// A chunk merge deletes whatever is staged at or below it in the same
// transaction, so a block staged by an earlier pass and read again here — after
// a gap the fold stopped at — is not counted twice.
func Update(tip int64) error {
    if db == nil { return errNoInit }
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var start = next()
    if start > tip { return nil }
    var prog = progress.New("addrindex", start, tip)
    for {
        var moved, ferr = fold(tip)
        if ferr != nil { return ferr }
        if !moved { break }
    }
    var from = next()
    var limit = tip - int64(recent)
    for from <= limit {
        var to = min(from+int64(chunkSize)-1, limit)
        var touches = map[string][]Touch{}
        for h := from; h <= to; h++ {
            var blk, berr = core.BlockAt(ctx, int(h))
            if berr != nil { return berr }
            indexBlock(touches, uint32(h), blk)
        }
        if err := merge(touches, int(to)); err != nil { return err }
        logging.Info("addrindex: built from " + blocksMsg(from, to))
        prog.Chunk(from, to)
        from = to + 1
    }
    var staged int64
    for h := from; h <= tip; h++ {
        var have, herr = holds(h)
        if herr != nil { return herr }
        if have { continue }
        if err := record(ctx, h); err != nil { return err }
        staged++
    }
    if staged > 0 { logging.Info("addrindex: staged %d recent blocks up to %d", staged, tip) }
    return nil
}

// next is the first height the cursor has not passed: 0 on a fresh index.
func next() int64 {
    if last, ok := cursors.Get(cursors.AddrIndex); ok { return last + 1 }
    return 0
}

// blocksMsg names a range of heights, in the singular when it is one block.
func blocksMsg(from, to int64) string {
    if from == to { return fmt.Sprintf("block %d", from) }
    return fmt.Sprintf("blocks %d..%d", from, to)
}

// indexBlock extracts every touch in one block into the running chunk map: every
// output's script is a funding touch, every spent prevout's script a spending
// touch. Both are gathered per transaction and deduplicated there, so a
// transaction touching the same address more than once — two outputs to one
// address, or an address appearing in both an input and an output of the same
// transaction — is recorded once.
func indexBlock(touches map[string][]Touch, height uint32, blk core.Block) {
    for txIndex, tx := range blk.Txs {
        var seen = map[string]bool{}
        for _, side := range [][]core.Payment{tx.Outputs, tx.Spent} {
            for _, o := range side {
                if p := string(Prefix(o.Script)); len(o.Script) > 0 && !seen[p] {
                    seen[p] = true
                    touches[p] = append(touches[p], Touch{Height: height, TxIndex: uint16(txIndex)})
                }
            }
        }
    }
}

// Balances returns every change one block makes to a script's balance: each
// output pays its own script, and each spent prevout takes back out of the
// script it was paid to. Nothing else moves value on the chain, so adding these
// up from genesis to a height leaves exactly what every script holds at that
// height — the UTXO set, aggregated by script.
//
// The list is flat, in block order, and deliberately not deduplicated: a script
// paid twice is paid twice, and a caller summing into its own table wants both.
// Nor is an amount ever dropped, so a block's changes sum to the subsidy its
// miner claimed. tools/addrindex's richbuild is the caller.
func Balances(blk core.Block) []core.Payment {
    var n int
    for _, tx := range blk.Txs { n += len(tx.Outputs) + len(tx.Spent) }
    var out = make([]core.Payment, 0, n)
    for _, tx := range blk.Txs { out = append(out, tx.Outputs...) }
    for _, tx := range blk.Txs {
        for _, o := range tx.Spent {
            out = append(out, core.Payment{Script: o.Script, Sat: -o.Sat})
        }
    }
    return out
}

// recent is how many of the newest blocks below the tip Update leaves staged in
// recaddrindex rather than merging into addrindex — the depth of reorg Rollback
// can undo. A package var so tests can lower it.
var recent = 10

// mergeBlocks is how many staged blocks one fold moves into addrindex in one
// transaction. A package var so tests shrink it.
var mergeBlocks int64 = 100

// mu serialises moving the cursor (merge, fold) with Rollback and with record,
// so a disconnected block cannot be deleted between a fold choosing it and
// folding it, and a block cannot be staged below a cursor that just passed it.
var mu sync.Mutex

// holds reports whether recaddrindex holds block h — every block staged has a
// row, if only the marker under shard -1.
func holds(h int64) (bool, error) {
    var have bool
    var err = db.QueryRow("select exists (select 1 from recaddrindex where block = ?)", h).Scan(&have)
    return have, err
}

// record reads block h and stages its touches in recaddrindex, one row per shard
// it touches plus the marker under shard -1, in one transaction. Whatever an
// earlier write left under h is replaced, so a block written again — a reorg
// racing a pass — is not counted twice. A block the cursor has passed is not
// written at all: it would be counted twice by every lookup until the next fold
// cleared it. The write holds mu, so a merge cannot move the cursor between the
// check and it.
func record(ctx context.Context, h int64) error {
    var blk, err = core.BlockAt(ctx, int(h))
    if err != nil { return fmt.Errorf("block %d: %w", h, err) }
    var touches = map[string][]Touch{}
    indexBlock(touches, uint32(h), blk)
    var shards = map[int64][]byte{}
    for prefix, list := range touches {
        var p = []byte(prefix)
        var s = key(p, 0) >> 32
        for _, t := range list { shards[s] = append(shards[s], encodeEntry(p, t)...) }
    }
    mu.Lock()
    defer mu.Unlock()
    if at, ok := cursors.Get(cursors.AddrIndex); ok && h <= at { return nil }
    var tx, terr = db.Begin()
    if terr != nil { return terr }
    defer tx.Rollback()
    if _, derr := tx.Exec("delete from recaddrindex where block = ?", h); derr != nil { return derr }
    var stmt, perr = tx.Prepare("insert into recaddrindex (shard, block, data) values (?, ?, ?)")
    if perr != nil { return perr }
    defer stmt.Close()
    if _, ierr := stmt.Exec(-1, h, []byte{}); ierr != nil { return ierr }
    for s, data := range shards {
        if _, ierr := stmt.Exec(s, h, data); ierr != nil { return ierr }
    }
    return tx.Commit()
}

// fold moves the next run of staged blocks into addrindex in one transaction,
// and reports whether it moved the cursor. The run starts at the height after
// the cursor and holds at most mergeBlocks heights, none past tip less `recent`;
// it ends at the first height recaddrindex does not hold, since the cursor goes
// to its last height and would otherwise step over one never read. Each staged
// row is appended to its range key in height order, so a range's entries stay
// oldest first, and write deletes the run from recaddrindex as it commits.
func fold(tip int64) (bool, error) {
    mu.Lock()
    defer mu.Unlock()
    var cursor = next() - 1
    var limit = min(cursor+mergeBlocks, tip-int64(recent))
    if limit <= cursor { return false, nil }
    var rows, err = db.Query("select shard, block, data from recaddrindex where block > ? and block <= ? order by block",
        cursor, limit)
    if err != nil { return false, err }
    var to = cursor
    var grouped = map[int64][]byte{}
    for rows.Next() {
        var s, h int64
        var data []byte
        if serr := rows.Scan(&s, &h, &data); serr != nil {
            rows.Close()
            return false, serr
        }
        if h > to+1 { break }
        to = h
        if s < 0 { continue }
        var k = s<<32 | int64(rangeOf(uint32(h)))
        grouped[k] = append(grouped[k], data...)
    }
    rows.Close()
    if rerr := rows.Err(); rerr != nil { return false, rerr }
    if to == cursor { return false, nil }
    if werr := write(grouped, int(to)); werr != nil { return false, werr }
    logging.Info("addrindex: folded %s", blocksMsg(cursor+1, to))
    return true, nil
}

// Rollback takes out the touches of a block a reorg disconnected, with every
// block above it, since they were built on it: their recaddrindex rows are
// deleted, so the next Update reads those heights again from the chain that
// replaced them. The height comes from the node, which keeps the header of a
// block it has disconnected. A block already merged into addrindex cannot be
// taken out — that is deeper than `recent` — and is logged as an error, as is
// any failure. Removing rows is logged as a status line; nothing is reported to
// the caller.
func Rollback(hash string) {
    if db == nil || !core.Enabled() { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var header, err = core.GetBlockHeader(ctx, hash)
    if err != nil {
        logging.Err("addrindex: roll back block %s: %v", hash, err)
        return
    }
    mu.Lock()
    defer mu.Unlock()
    if at, ok := cursors.Get(cursors.AddrIndex); ok && header.Height <= at {
        logging.Err("addrindex: roll back block %s: block %d is already merged into the index (cursor %d)", hash, header.Height, at)
        return
    }
    var res, derr = db.Exec("delete from recaddrindex where block >= ?", header.Height)
    if derr != nil {
        logging.Err("addrindex: roll back block %s: %v", hash, derr)
        return
    }
    if n, _ := res.RowsAffected(); n > 0 {
        logging.Status("addrindex: block %s disconnected, %d recent index rows removed", hash, n)
    }
}

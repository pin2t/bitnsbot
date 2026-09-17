// Package addrbal keeps what every address on the chain holds now, and so how
// many addresses hold anything at all — the figure the Mini App's Blockchain
// card shows.
//
// It is sharded the way the address index is, and for the same reason: a B-tree
// pays a per-row cost that one row per address — some sixty million of them on
// mainnet — would multiply. An address is the SHA-256 of its addrindex.Key, cut
// to 8 bytes: the first 2 pick the shard, which is the row, and the other 6 are
// stored in it beside the address's balance in satoshi. A row's value is the
// packed run of its entries, 14 bytes each, sorted by those 6 bytes.
//
// **Two bytes, so 65 536 rows**, of about 870 entries each on mainnet, was
// measured against three and four. Four puts nearly every address in a row of
// its own and pays SQLite's row overhead per address — 43% larger, and slower at
// every merge size. Three is 9% larger than two. Two makes a row a 12 KB value,
// so a merge touching a few addresses in it rewrites all of it; but a merge of a
// million addresses touches every row many times over, and so costs no more than
// rewriting the table once, whatever it carries — which is what makes large
// merges, and so a fast build, cheap.
//
// Keying by addrindex.Key rather than by script is what makes this a count of
// addresses: an early coinbase paying `<pubkey> OP_CHECKSIG` and a later payment
// to that key's P2PKH address land on one entry, the way the rest of the repo
// reads them. A script with no address — an OP_RETURN, a bare multisig — is not
// counted, and neither is what it holds.
//
// **An address whose balance reaches zero is removed**, and a row left with no
// entries is deleted, so the table holds exactly the funded addresses and the
// count is its entries.
package addrbal

import "context"
import "crypto/sha256"
import "database/sql"
import "encoding/binary"
import "errors"
import "fmt"
import "slices"
import "sync/atomic"
import "time"
import "bitnsbot/addrindex"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/signals"

var db *sql.DB

// remainderLen is the part of an address's 8-byte hash stored in its entry; the
// 2 bytes above it are the shard.
const remainderLen = 6
const remainderBits = 8 * remainderLen
const remainderMask = 1<<remainderBits - 1

// entryLen is one packed entry: the remainder, and the balance in satoshi.
const entryLen = remainderLen + 8

// confirmations is how far behind the tip the scan stays. A balance is a running
// total, so a block scanned and then reorged away would stay counted for good —
// its outputs paid and never spent, the replacement block's never seen. Six
// blocks is an hour behind for a figure nobody reads to the block.
const confirmations = 6

// chunkSize is the most blocks one pass reads before it merges, and flushKeys the
// most addresses it lets pile up first. The second bounds a merge's memory and
// its write transaction: SQLite has one writer, and every other writer in the
// bot — a /watch, the block cache — waits for this one's commit, busy_timeout at
// most. A million addresses is where a merge touches every shard, so its cost
// stops growing with its size; at mainnet's size it held the lock 3.5 s on the
// Mac it was measured on. Package vars so tests shrink them.
var chunkSize = 1000
var flushKeys = 1_000_000

// fetchers is how many blocks are read from the node at once. The merge needs
// them in height order, since a flush is a cursor, so each height gets a slot and
// the slots are read back in the order they were queued.
var fetchers = 4

// interval is the longest the scan waits once it has caught up; a block
// notification cuts it short.
var interval = 10 * time.Minute

// count is how many addresses hold a balance, as of the cursor. It is loaded from
// the table on the first pass and moved by each merge after that. caughtUp is
// whether a pass has reached the tip, so the count is the chain's and not a share
// of it.
var count atomic.Int64
var loaded atomic.Bool
var caughtUp atomic.Bool

// Init stores the handle. The `addrbal` table is openDB's to create, and the
// scan's place is a row of the cursors table under cursors.AddrBal.
func Init(handle *sql.DB) error {
    db = handle
    return nil
}

// Count is how many addresses hold a positive balance, and whether the scan has
// caught up with the chain. Until it has, the number is only the part of the
// chain the scan has read, which is not a figure to show as the whole.
func Count() (int64, bool) {
    return count.Load(), caughtUp.Load()
}

// Start runs the scan: a catch-up to the tip, then again on every block
// notification and every interval, whichever comes first.
func Start() {
    go func() {
        var wake = signals.Subscribe(signals.Block)
        for {
            if err := Build(); err != nil { logging.Warn("addrbal: %v", err) }
            select {
            case <-time.After(interval):
            case <-wake:
            }
        }
    }()
}

// Build walks the chain from the cursor to confirmations below the tip once and
// returns. A pass that fails leaves the cursor at its last merge, and the next
// resumes there. A pass that merged something, or caught up for the first time,
// fires signals.AddrBal, which is what the Blockchain card refreshes on.
//
// The count is loaded from the table the first time, which reads every row: the
// entries are all there is to count, and a stored total could drift from them.
func Build() error {
    if db == nil { return errors.New("addrbal: not initialised (Init was not called)") }
    if !loaded.Load() {
        var n int64
        if err := db.QueryRow("select coalesce(sum(length(data)), 0) from addrbal").Scan(&n); err != nil { return err }
        count.Store(n / entryLen)
        loaded.Store(true)
    }
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var tip, err = core.GetBlockCount(ctx)
    if err != nil { return err }
    var target = int(tip) - confirmations
    var last, ok = cursors.Get(cursors.AddrBal)
    var from int
    if ok { from = int(last) + 1 }
    var began = from
    for from <= target {
        var to = min(from+chunkSize-1, target)
        if err := scan(ctx, from, to); err != nil { return err }
        from = to + 1
    }
    if !caughtUp.Swap(true) || from > began { signals.Send(signals.AddrBal) }
    return nil
}

// fetched is one block's balance changes, or the error that stopped it.
type fetched struct {
    moves []move
    err   error
}

// move is one change to an address's balance: the address's 8-byte hash, and the
// satoshi paid to it or spent from it.
type move struct {
    hash uint64
    sat  int64
}

// scan reads blocks from..to and merges them, whenever flushKeys addresses have
// piled up and at the end. Several blocks are fetched at once and read back in
// height order, so a merge always stops at a height with nothing above it
// applied, and the cursor can be that height. Returning cancels the fetches still
// running.
func scan(ctx context.Context, from, to int) error {
    var sctx, cancel = context.WithCancel(ctx)
    defer cancel()
    var slots = make(chan chan fetched, fetchers)
    go func() {
        defer close(slots)
        var sem = make(chan struct{}, fetchers)
        for h := from; h <= to; h++ {
            var slot = make(chan fetched, 1)
            select {
            case slots <- slot:
            case <-sctx.Done():
                return
            }
            sem <- struct{}{}
            go func() {
                defer func() { <-sem }()
                var blk, err = addrindex.BlockAt(sctx, h)
                slot <- fetched{moves: moves(blk), err: err}
            }()
        }
    }()
    var deltas = map[uint64]int64{}
    var start, height = from, from
    var began = time.Now()
    for slot := range slots {
        var f = <-slot
        if f.err != nil { return fmt.Errorf("block %d: %w", height, f.err) }
        for _, m := range f.moves { deltas[m.hash] += m.sat }
        if len(deltas) >= flushKeys || height == to {
            var merging = time.Now()
            var change, err = flush(deltas, height)
            if err != nil { return err }
            count.Add(change)
            logging.Info("addrbal: blocks %d..%d read in %s, %d addresses merged in %s, %d with a balance",
                start, height, merging.Sub(began).Round(time.Millisecond), len(deltas),
                time.Since(merging).Round(time.Millisecond), count.Load())
            deltas = map[uint64]int64{}
            start = height + 1
            began = time.Now()
        }
        height++
    }
    return nil
}

// moves is every balance change a block makes, for the addresses among its
// scripts: what addrindex.Balances says, keyed by address hash. A zero amount
// moves nothing and is left out. It runs on the fetching goroutine, so the
// hashing is spread over the fetchers too.
func moves(blk addrindex.Block) []move {
    var out []move
    for _, p := range addrindex.Balances(blk) {
        var key, ok = addrindex.Key(p.Script)
        if !ok || p.Sat == 0 { continue }
        var sum = sha256.Sum256([]byte(key))
        out = append(out, move{hash: binary.BigEndian.Uint64(sum[:8]), sat: p.Sat})
    }
    return out
}

// flush merges a pass's balance changes into the table and moves the cursor to
// last, in one transaction: a merge that is written and a cursor that is not
// would count those blocks twice. It returns how the number of funded addresses
// changed.
//
// The addresses are merged in hash order, which is shard order, so each shard is
// read and written once, and the B-tree is walked forwards rather than at random.
// A change that nets to zero leaves the balance as it was and is skipped without
// reading anything.
func flush(deltas map[uint64]int64, last int) (int64, error) {
    var keys = make([]uint64, 0, len(deltas))
    for k, d := range deltas {
        if d != 0 { keys = append(keys, k) }
    }
    slices.Sort(keys)
    var tx, err = db.Begin()
    if err != nil { return 0, err }
    defer tx.Rollback()
    var read, rerr = tx.Prepare("select data from addrbal where shard = ?")
    if rerr != nil { return 0, rerr }
    var write, werr = tx.Prepare("insert into addrbal (shard, data) values (?, ?) on conflict(shard) do update set data = excluded.data")
    if werr != nil { return 0, werr }
    var drop, derr = tx.Prepare("delete from addrbal where shard = ?")
    if derr != nil { return 0, derr }
    var change int64
    for i := 0; i < len(keys); {
        var shard = int64(keys[i] >> remainderBits)
        var j = i
        for j < len(keys) && int64(keys[j]>>remainderBits) == shard { j++ }
        var data []byte
        if err := read.QueryRow(shard).Scan(&data); err != nil && !errors.Is(err, sql.ErrNoRows) { return 0, err }
        if len(data)%entryLen != 0 { return 0, fmt.Errorf("addrbal: shard %06x holds %d bytes, not whole entries", shard, len(data)) }
        var merged, diff = merge(data, keys[i:j], deltas)
        change += diff
        switch {
        case len(merged) > 0:
            if _, err := write.Exec(shard, merged); err != nil { return 0, err }
        case len(data) > 0:
            if _, err := drop.Exec(shard); err != nil { return 0, err }
        }
        i = j
    }
    if err := cursors.Set(tx, cursors.AddrBal, int64(last)); err != nil { return 0, err }
    return change, tx.Commit()
}

// merge applies the changes for one shard's addresses — keys, sorted, all in that
// shard — to its packed entries, and returns the new entries, still sorted, and
// how many more funded addresses the shard holds. An address whose balance comes
// to zero is left out, and one new to the shard is added.
func merge(data []byte, keys []uint64, deltas map[uint64]int64) ([]byte, int64) {
    var out = make([]byte, 0, len(data)+entryLen*len(keys))
    var change int64
    var i, k = 0, 0
    for i < len(data) || k < len(keys) {
        var rem uint64
        var bal int64
        switch {
        case k == len(keys) || i < len(data) && remainder(data[i:]) < keys[k]&remainderMask:
            out = append(out, data[i:i+entryLen]...)
            i += entryLen
            continue
        case i < len(data) && remainder(data[i:]) == keys[k]&remainderMask:
            rem = keys[k] & remainderMask
            bal = int64(binary.BigEndian.Uint64(data[i+remainderLen:])) + deltas[keys[k]]
            i += entryLen
            if bal == 0 { change-- }
        default:
            rem = keys[k] & remainderMask
            bal = deltas[keys[k]]
            change++
        }
        k++
        if bal == 0 { continue }
        out = binary.BigEndian.AppendUint16(out, uint16(rem>>32))
        out = binary.BigEndian.AppendUint32(out, uint32(rem))
        out = binary.BigEndian.AppendUint64(out, uint64(bal))
    }
    return out, change
}

// remainder reads the 6 bytes an entry starts with.
func remainder(entry []byte) uint64 {
    return uint64(binary.BigEndian.Uint16(entry))<<32 | uint64(binary.BigEndian.Uint32(entry[2:remainderLen]))
}

// Package addrstat keeps on-chain statistics for every funded address, so /info
// and the Mini App answer for one without resolving its history per request —
// the busiest address on the chain holds millions of transactions, where the
// live path caps at ten thousand and marks the answer with a "+". The three
// ranked lists the Addresses tab shows are three readings of the same records.
//
// Two tables. addrstat, keyed by address, holds one row each — the statistics as
// of the scan's cursor. recaddrstat, keyed by address and block, holds what each
// block above the cursor moved, one row per address the block touched: the scan
// is staged, block readers writing there and an aggregator folding the oldest of
// them into addrstat while keeping the newest `recent` blocks apart, so a block a
// reorg disconnects can still be taken out (see Rollback). Every read combines the
// two tables. The cursor is addrstat's alone: recaddrstat's blocks say for
// themselves which heights they hold.
//
// **This is the keep-up path**: the bot reads one block at a time, in height
// order. A first build from genesis is tools/addrstat's job, which reads blocks
// on several goroutines and merges each straight into addrstat through Merge,
// staging nothing, and leaves the cursor a few blocks below the tip for Update to
// carry on from.
//
// **The scan writes the addresses it finds**, rather than gathering for a set
// somebody put there: a fold inserts a row for every address it saw and had none
// for, and keeps every row, an address whose balance has reached zero included —
// so its history is whole however often it is emptied and paid again.
package addrstat

import "context"
import "errors"
import "fmt"
import "sync"
import "time"
import "database/sql"
import "bitnsbot/addrindex"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/progress"
import "bitnsbot/signals"

var db *sql.DB

// recent is how many of the newest blocks below the tip Update leaves in
// recaddrstat rather than folding into addrstat — the depth of reorg Rollback can
// undo. A package var so tests can lower it.
var recent = 10

// roundBlocks is how many heights one round of Update reads, counted from the
// one after the cursor: the staged blocks a round leaves behind are
// bounded by it, since the cursor moves only between rounds. A package var so
// tests shrink it.
var roundBlocks int64 = 1000

// mergeBlocks is how many staged blocks one fold moves into addrstat in one
// transaction, which holds SQLite's one write lock for its length. A package var
// so tests shrink it.
var mergeBlocks int64 = 100

// onConflict is what an insert into addrstat does with an address that already
// has a row: add the movements to it. Balance and Flow are derived from the sums
// they follow from, so they cannot drift out of step with them. `first` takes the
// earlier of the two only when there is one — a fresh row has 0 there, which is
// not earlier than anything — and `type` is only ever written by the insert, so a
// form named by hand is left as it is. Shared by fold and Merge.
const onConflict = ` on conflict(addr) do update set
    recv = addrstat.recv + excluded.recv,
    sent = addrstat.sent + excluded.sent,
    fees = addrstat.fees + excluded.fees,
    txs = addrstat.txs + excluded.txs,
    first = case when excluded.first > 0 and (addrstat.first = 0 or excluded.first < addrstat.first)
        then excluded.first else addrstat.first end,
    last = max(addrstat.last, excluded.last),
    balance = addrstat.recv + excluded.recv - (addrstat.sent + excluded.sent),
    flow = addrstat.recv + excluded.recv + addrstat.sent + excluded.sent`

// mu serialises folding blocks into addrstat with Rollback and with record, so a
// disconnected block cannot be deleted between a fold choosing it and folding it,
// which would move the cursor over a height whose movements were never counted.
var mu sync.Mutex

// Stat is one address's record. Balance and Flow are derived from Recv and Sent
// rather than accumulated on their own, so they cannot drift out of step with
// them; they are stored because a reader of the table should not have to know
// which columns are sums and which are arithmetic.
type Stat struct {
    Type    string
    Balance int64
    Recv    int64
    Sent    int64
    Flow    int64
    Fees    int64
    Txs     int64
    First   int64
    Last    int64
}

// Init stores the shared handle and creates the two tables and their indexes when
// they are missing: the three addrstat indexes the rankings are read through, and
// recaddrstat's by block, which the aggregator and Rollback find a run of blocks
// through. The scan's place is a row of the shared cursors table under
// cursors.AddrStat.
//
// recaddrstat's rows are keyed (addr, block), so an address's recent movements
// are a prefix of the primary key. Every block the scan has read has a row, if
// only the one under the empty address that marks it read: a block paying no
// address at all would otherwise be indistinguishable from one never read, and
// the aggregator would wait on it for ever.
//
// The table is the scan's to fill, so there is nothing for an operator to seed
// and nothing here that reads it into memory. An address added by hand gathers
// nothing until the scan passes it again, which means clearing the cursor by hand
// — an operator's job, the alternative being hours of automatic rescanning
// triggered by a signal nothing can tell apart from an address the chain has
// never seen.
func Init(handle *sql.DB) error {
    db = handle
    if db == nil { return nil }
    for _, s := range []string{
        `create table if not exists addrstat (addr TEXT PRIMARY KEY, type TEXT NOT NULL, balance INTEGER NOT NULL,
            recv INTEGER NOT NULL, sent INTEGER NOT NULL, flow INTEGER NOT NULL, fees INTEGER NOT NULL,
            txs INTEGER NOT NULL, first INTEGER NOT NULL, last INTEGER NOT NULL)`,
        `create index if not exists addrstat_balance on addrstat (balance)`,
        `create index if not exists addrstat_txs on addrstat (txs)`,
        `create index if not exists addrstat_last on addrstat (last)`,
        `create table if not exists recaddrstat (addr TEXT NOT NULL, type TEXT NOT NULL, balance INTEGER NOT NULL,
            recv INTEGER NOT NULL, sent INTEGER NOT NULL, flow INTEGER NOT NULL, fees INTEGER NOT NULL,
            txs INTEGER NOT NULL, first INTEGER NOT NULL, last INTEGER NOT NULL, block INTEGER NOT NULL,
            PRIMARY KEY (addr, block)) without rowid`,
        `create index if not exists recaddrstat_block on recaddrstat (block)`,
    } {
        if _, err := db.Exec(s); err != nil { return err }
    }
    return nil
}

// Get returns an address's statistics — its addrstat row with the movements of
// the blocks still in recaddrstat added — and false when there are none to give,
// the scan not having reached the address. A record with no
// transactions in it is answered with false as well — a row an earlier version
// seeded and this scan has not been past, so answering with it would present "0
// transactions" as a fact. The live path answers those instead, as it does for
// every address with no row at all.
func Get(addr string) (Stat, bool) {
    if db == nil || addr == "" { return Stat{}, false }
    var rows, err = db.Query(`select type, recv, sent, fees, txs, first, last from addrstat where addr = ?
        union all select type, recv, sent, fees, txs, first, last from recaddrstat where addr = ?`, addr, addr)
    if err != nil { return Stat{}, false }
    defer rows.Close()
    var s Stat
    for rows.Next() {
        var d Stat
        if rows.Scan(&d.Type, &d.Recv, &d.Sent, &d.Fees, &d.Txs, &d.First, &d.Last) != nil { return Stat{}, false }
        if s.Type == "" { s.Type = d.Type }
        s.Recv += d.Recv
        s.Sent += d.Sent
        s.Fees += d.Fees
        s.Txs += d.Txs
        if d.First > 0 && (s.First == 0 || d.First < s.First) { s.First = d.First }
        s.Last = max(s.Last, d.Last)
    }
    if rows.Err() != nil { return Stat{}, false }
    s.Balance = s.Recv - s.Sent
    s.Flow = s.Recv + s.Sent
    return s, s.Txs > 0
}

// Count is how many addresses the statistics hold as of the newest block
// scanned: the addrstat rows, and the addresses recaddrstat's blocks touched that
// have no row yet. It counts rather than keeping a total, so it cannot drift from
// what it counts, and the scan does not call it per block.
func Count() int {
    if db == nil { return 0 }
    var n int
    var err = db.QueryRow(`select (select count(*) from addrstat) + (select count(distinct r.addr)
        from recaddrstat r where r.addr != '' and not exists (select 1 from addrstat s where s.addr = r.addr))`).Scan(&n)
    if err != nil { return 0 }
    return n
}

// Height is the newest block the statistics include: the highest block in
// recaddrstat, or the cursor when it is empty. False when nothing has been
// scanned at all.
func Height() (int64, bool) {
    if db == nil { return 0, false }
    var h sql.NullInt64
    if err := db.QueryRow("select max(block) from recaddrstat").Scan(&h); err == nil && h.Valid { return h.Int64, true }
    return cursors.Get(cursors.AddrStat)
}

// Rollback takes out what a block a reorg disconnected moved, with every block
// above it, since they were built on it: their recaddrstat rows are deleted, so
// the next Update reads those heights again from the chain that replaced them.
// The height comes from the node, which keeps the header of a block it has
// disconnected. A block already folded into addrstat cannot be taken out — that
// is deeper than `recent` — and is logged as an error, as is any failure.
// Removing rows is logged as a status line; nothing is reported to the caller.
func Rollback(hash string) {
    if db == nil || !core.Enabled() { return }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var header, err = core.GetBlockHeader(ctx, hash)
    if err != nil {
        logging.Err("addrstat: roll back block %s: %v", hash, err)
        return
    }
    mu.Lock()
    defer mu.Unlock()
    if at, ok := cursors.Get(cursors.AddrStat); ok && header.Height <= at {
        logging.Err("addrstat: roll back block %s: block %d is already folded into the statistics (cursor %d)", hash, header.Height, at)
        return
    }
    var res, derr = db.Exec("delete from recaddrstat where block >= ?", header.Height)
    if derr != nil {
        logging.Err("addrstat: roll back block %s: %v", hash, derr)
        return
    }
    if n, _ := res.RowsAffected(); n > 0 {
        logging.Status("addrstat: block %s disconnected, %d recent address statistics removed", hash, n)
    }
}

// Collect walks the chain from the cursor to the current tip once and returns.
// It is exported so a caller that wants one pass can have one — the bot itself
// goes through Update, whose tip its caller has already fetched.
func Collect() error {
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var tip, err = core.GetBlockCount(ctx)
    if err != nil { return err }
    return Update(tip)
}

// Update advances the statistics to tip once and returns, in rounds of two
// stages that run one after the other. First the read: the roundBlocks heights
// from the one after the cursor, or up to tip when that is nearer, are read one
// at a time in height order (core.BlockAt, one getblock at verbosity 3, whose
// prevouts are where a spend's script and amount, and so a transaction's fee, are
// written down), each block's per-address movements written to recaddrstat and a
// height recaddrstat already holds — read by an earlier round or pass — skipped.
// Then the merge: the staged blocks straight on from the cursor are folded into
// addrstat a mergeBlocks run at a time, each run moving the cursor in its own
// transaction, stopping at the first missing height or at tip less `recent`, so
// the newest `recent` blocks stay staged. The rounds repeat until one has read
// every height up to tip.
//
// A block that cannot be read stops the round, the blocks already staged are
// merged as far as they run, and the pass fails; the next pass reads only what
// is missing.
func Update(tip int64) error {
    if db == nil { return nil }
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var start int64
    if last, ok := cursors.Get(cursors.AddrStat); ok { start = last + 1 }
    if start > tip { return nil }
    var prog = progress.New("addrstat", start, tip)
    var read int64
    for {
        var from int64
        if last, ok := cursors.Get(cursors.AddrStat); ok { from = last + 1 }
        var to = min(from+roundBlocks-1, tip)
        var n, rerr = readBlocks(ctx, from, to)
        read += n
        if rerr == nil { rerr = ctx.Err() }
        var f = folded{from: -1}
        for {
            var moved, ferr = fold(tip, &f)
            if ferr != nil { return ferr }
            if !moved { break }
        }
        if f.from >= 0 {
            logging.Info("addrstat: folded %s, %d addresses merged", blocksMsg(f.from, f.to), f.merged)
        }
        if rerr != nil { return rerr }
        if from <= to { prog.Chunk(from, to) }
        if to >= tip { break }
    }
    if read > 0 {
        var h, _ = Height()
        logging.Info("addrstat: caught up with %s, %d addresses", blocksMsg(start, h), Count())
        signals.Send(signals.AddrStat)
    }
    return nil
}

// readBlocks is one round's read: the heights from..to in order, skipping a
// height recaddrstat already holds and recording the rest. It stops at the first
// failure; the count is how many blocks were written.
func readBlocks(ctx context.Context, from, to int64) (int64, error) {
    var written int64
    for h := from; h <= to; h++ {
        if err := ctx.Err(); err != nil { return written, err }
        var have, err = holds(h)
        if err != nil { return written, err }
        if have { continue }
        if rerr := record(ctx, h); rerr != nil { return written, rerr }
        written++
    }
    return written, nil
}

// holds reports whether recaddrstat holds block h — every block read has a row,
// if only the empty address's marking it read.
func holds(h int64) (bool, error) {
    var have bool
    var err = db.QueryRow("select exists (select 1 from recaddrstat where block = ?)", h).Scan(&have)
    return have, err
}

// record reads block h and writes what it moved to recaddrstat, one row per
// address plus the empty address's row marking the block read, in one
// transaction. Whatever an earlier write left under h is replaced, so a block
// written again — a reorg racing a pass — is not counted twice. A block already
// folded is not written at all: its rows would sit below the cursor, counted
// twice by every read until the next fold cleared them. The write holds mu, so a
// fold cannot move the cursor between the check and it.
func record(ctx context.Context, h int64) error {
    var blk, err = core.BlockAt(ctx, int(h))
    if err != nil { return fmt.Errorf("block %d: %w", h, err) }
    var deltas = Deltas(blk)
    mu.Lock()
    defer mu.Unlock()
    if at, ok := cursors.Get(cursors.AddrStat); ok && h <= at { return nil }
    var tx, terr = db.Begin()
    if terr != nil { return terr }
    defer tx.Rollback()
    if _, derr := tx.Exec("delete from recaddrstat where block = ?", h); derr != nil { return derr }
    var stmt, perr = tx.Prepare(`insert into recaddrstat (addr, type, balance, recv, sent, flow, fees, txs, first, last, block)
        values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
    if perr != nil { return perr }
    defer stmt.Close()
    if _, ierr := stmt.Exec("", "", 0, 0, 0, 0, 0, 0, 0, 0, h); ierr != nil { return ierr }
    for addr, d := range deltas {
        if _, ierr := stmt.Exec(addr, d.Type, d.Recv-d.Sent, d.Recv, d.Sent, d.Recv+d.Sent, d.Fees, d.Txs,
            d.First, d.Last, h); ierr != nil {
            return ierr
        }
    }
    return tx.Commit()
}

// folded is what one round's merge moved into addrstat, for its log line; from
// is -1 until a fold has moved something.
type folded struct {
    from    int64
    to      int64
    merged  int64
}

// fold moves the next run of staged blocks into addrstat in one transaction, and
// reports whether it moved the cursor. The run starts at the height after the
// cursor and holds at most mergeBlocks heights, none of them past tip less
// `recent`; it ends at the first height recaddrstat does not hold, since the
// cursor goes to its last height and would otherwise step over one never read.
//
// The movements are summed per address in SQL and added to the row in the same
// statement (onConflict) — one statement for the lot rather than a query and an
// update per address.
//
// An address with no row gets one, and a row is never deleted, an address
// holding nothing included.
func fold(tip int64, f *folded) (bool, error) {
    mu.Lock()
    defer mu.Unlock()
    var cursor = int64(-1)
    if last, ok := cursors.Get(cursors.AddrStat); ok { cursor = last }
    var limit = min(cursor+mergeBlocks, tip-int64(recent))
    if limit <= cursor { return false, nil }
    var rows, err = db.Query("select distinct block from recaddrstat where block > ? and block <= ? order by block", cursor, limit)
    if err != nil { return false, err }
    var to = cursor
    for rows.Next() {
        var h int64
        if serr := rows.Scan(&h); serr != nil {
            rows.Close()
            return false, serr
        }
        if h != to+1 { break }
        to = h
    }
    rows.Close()
    if rerr := rows.Err(); rerr != nil { return false, rerr }
    if to == cursor { return false, nil }
    var tx, terr = db.Begin()
    if terr != nil { return false, terr }
    defer tx.Rollback()
    var res, uerr = tx.Exec(`insert into addrstat (addr, type, balance, recv, sent, flow, fees, txs, first, last)
        select addr, max(type), sum(recv) - sum(sent), sum(recv), sum(sent), sum(recv) + sum(sent), sum(fees),
            sum(txs), coalesce(min(nullif(first, 0)), 0), max(last)
        from recaddrstat where block > ? and block <= ? and addr != '' group by addr`+onConflict, cursor, to)
    if uerr != nil { return false, uerr }
    var merged, _ = res.RowsAffected()
    if _, rerr := tx.Exec("delete from recaddrstat where block <= ?", to); rerr != nil { return false, rerr }
    if serr := cursors.Set(tx, cursors.AddrStat, to); serr != nil { return false, serr }
    if cerr := tx.Commit(); cerr != nil { return false, cerr }
    if f.from < 0 { f.from = cursor + 1 }
    f.to = to
    f.merged += merged
    return true, nil
}

// Merge adds the movements of every block up to h straight to addrstat and moves
// the cursor to h, in one transaction — tools/addrstat's pre-build, which reads
// blocks in parallel, sums a run of them in memory in height order and hands the
// sum here, staging nothing. Whatever recaddrstat holds at or below h is deleted
// in the same transaction, those blocks being counted now, so a build over a
// database the bot has staged blocks in does not count them twice. A nil deltas
// only moves the cursor.
func Merge(h int64, deltas map[string]*Stat) error {
    if db == nil { return errors.New("addrstat: no database") }
    mu.Lock()
    defer mu.Unlock()
    var tx, err = db.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    var stmt, perr = tx.Prepare(`insert into addrstat (addr, type, balance, recv, sent, flow, fees, txs, first, last)
        values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`+onConflict)
    if perr != nil { return perr }
    defer stmt.Close()
    for addr, d := range deltas {
        if _, ierr := stmt.Exec(addr, d.Type, d.Recv-d.Sent, d.Recv, d.Sent, d.Recv+d.Sent, d.Fees, d.Txs,
            d.First, d.Last); ierr != nil {
            return ierr
        }
    }
    if _, derr := tx.Exec("delete from recaddrstat where block <= ?", h); derr != nil { return derr }
    if serr := cursors.Set(tx, cursors.AddrStat, h); serr != nil { return serr }
    return tx.Commit()
}

// blocksMsg names a range of heights the way every collector's line does, in the
// singular when it is one block.
func blocksMsg(from, to int64) string {
    if from == to { return fmt.Sprintf("block %d", from) }
    return fmt.Sprintf("blocks %d..%d", from, to)
}

// Deltas is what one block moved, per address it touched. A transaction is
// counted once per address however many of its outputs or inputs name it, which
// is the same dedup the index itself does — an address paid twice by one
// transaction was in one transaction.
//
// The fee is the whole transaction's, charged to every address that spends in
// it: the chain records who funded a transaction, not which of them paid for it,
// and this is what the live path already does.
//
// a coinbase spends nothing and pays no fee
func Deltas(blk core.Block) map[string]*Stat {
    var deltas = map[string]*Stat{}
    for _, tx := range blk.Txs {
        var fee int64
        if len(tx.Spent) > 0 {
            for _, p := range tx.Spent { fee += p.Sat }
            for _, p := range tx.Outputs { fee -= p.Sat }
        }
        var seen = map[string]bool{}
        var paid = map[string]bool{}
        for _, p := range tx.Outputs {
            var s, addr = at(deltas, p.Script)
            if s == nil { continue }
            s.Recv += p.Sat
            touch(s, seen, addr, blk.Time)
        }
        for _, p := range tx.Spent {
            var s, addr = at(deltas, p.Script)
            if s == nil { continue }
            s.Sent += p.Sat
            if fee > 0 && !paid[addr] {
                paid[addr] = true
                s.Fees += fee
            }
            touch(s, seen, addr, blk.Time)
        }
    }
    return deltas
}

// at is the running record for the address a script pays, or nil when it pays no
// address at all — an OP_RETURN, a bare multisig, anything nonstandard, which is
// what addrbal leaves out of its count for the same reason. The address is
// encoded from the script, and its form named from the address, once per block
// rather than once per touch: a busy address appears many times in a block, and
// base58 and bech32 are the expensive part of this scan — which is why the
// tool's pre-build reads and applies blocks on several goroutines at once.
func at(deltas map[string]*Stat, script []byte) (*Stat, string) {
    var addr = addrindex.Address(script)
    if addr == "" { return nil, "" }
    var s = deltas[addr]
    if s == nil {
        var _, kind, _ = addrindex.Decode(addr)
        s = &Stat{Type: kind}
        deltas[addr] = s
    }
    return s, addr
}

// touch counts the transaction once for this address and moves the first and
// last dates. Both are a min and a max rather than the first and last seen: a
// block's timestamp may legitimately run up to two hours behind the block before
// it, so the latest block an address appears in is not always the latest time.
func touch(s *Stat, seen map[string]bool, addr string, when int64) {
    if !seen[addr] {
        seen[addr] = true
        s.Txs++
    }
    if when <= 0 { return }
    if s.First == 0 || when < s.First { s.First = when }
    if when > s.Last { s.Last = when }
}

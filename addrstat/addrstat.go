// Package addrstat keeps on-chain statistics for every funded address, so /info
// and the Mini App answer for one without resolving its history per request —
// the busiest address on the chain holds millions of transactions, where the
// live path caps at ten thousand and marks the answer with a "+". The three
// ranked lists the Addresses tab shows are three readings of the same records.
//
// One table, addrstat, keyed by address, holding one row each. The bot's one
// index catch-up goroutine walks the chain from genesis filling them in,
// resuming from its place in the shared cursors table exactly as the block cache
// and the miner statistics do.
//
// **The scan writes the addresses it finds**, rather than gathering for a set
// somebody put there: a chunk inserts a row for every address it saw and had
// none for, and deletes the row of every address it saw whose balance has
// reached zero — so the table holds the addresses that hold coins, the way
// addrbal's entries do, rather than growing to the billion-odd an address a
// chain-wide history would keep. The consequence to know is that an address
// emptied and later paid again starts a fresh row: its row was deleted while it
// held nothing, so the transactions and the first date from before that are gone.
package addrstat

import "context"
import "fmt"
import "runtime"
import "sync"
import "sync/atomic"
import "time"
import "database/sql"
import "bitnsbot/addrindex"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/progress"
import "bitnsbot/signals"

var db *sql.DB

// chunkSize bounds how many blocks are read before one database flush, the same
// reasoning as the miners collector: catching up the whole chain must not build
// one giant transaction, and a crash mid-catch-up resumes from the last flushed
// chunk.
var chunkSize int64 = 1000

// flushKeys is the most addresses a chunk lets pile up before it merges what it
// has and advances the cursor to the block it had reached. A chunk of a thousand
// modern blocks touches millions of addresses, each a row's worth of sums held in
// memory, so the chunk alone is not a bound the Pi can keep to. A package var so
// tests shrink it.
var flushKeys = 500_000

// workers is how many goroutines read a chunk's blocks at once: half the
// processors, so the node — on the Pi, the same machine — keeps the other half,
// and at least one. A package var so tests can raise it on a machine with few.
var workers = max(1, runtime.GOMAXPROCS(0)/2)

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

// Init stores the shared handle and creates the table and the three indexes the
// rankings are read through when they are missing. The scan's place is a row of
// the shared cursors table under cursors.AddrStat.
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
    } {
        if _, err := db.Exec(s); err != nil { return err }
    }
    return nil
}

// Get returns an address's statistics, and false when there are none to give:
// the scan has not reached the address, or it holds nothing and so has no row.
// A row with no transactions in it is answered with false as well — it is a row
// an earlier version seeded and this scan has not been past, so answering with it
// would present "0 transactions" as a fact. The live path answers those instead,
// as it does for every address with no row at all.
func Get(addr string) (Stat, bool) {
    if db == nil { return Stat{}, false }
    var s Stat
    var err = db.QueryRow(`select type, balance, recv, sent, flow, fees, txs, first, last
        from addrstat where addr = ?`, addr).Scan(&s.Type, &s.Balance, &s.Recv, &s.Sent, &s.Flow,
        &s.Fees, &s.Txs, &s.First, &s.Last)
    if err != nil { return Stat{}, false }
    return s, s.Txs > 0
}

// Count is how many addresses the table holds, which is how many are known to
// hold coins as of the cursor. It counts the rows rather than keeping a total, so
// it cannot drift from what it counts, and the scan does not call it per chunk.
func Count() int {
    if db == nil { return 0 }
    var n int
    if err := db.QueryRow("select count(*) from addrstat").Scan(&n); err != nil { return 0 }
    return n
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

// Update advances the statistics from the cursor to tip once and returns. The
// blocks are the ones the address index is built from — core.BlockAt, one
// getblock at verbosity 3 a block, whose prevouts are where a spend's script and
// amount, and so a transaction's fee, are written down.
//
// A chunk's blocks are read in parallel by collect, which returns once the chunk
// is done or flushKeys addresses have piled up; what it returns is merged and the
// cursor goes to the last height it covers, and the chunk carries on from there.
func Update(tip int64) error {
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var last, ok = cursors.Get(cursors.AddrStat)
    var curr int64
    if ok { curr = last + 1 }
    var began = curr
    var prog = progress.New("addrstat", curr, tip)
    for curr <= tip {
        var to = curr + chunkSize - 1
        if to > tip { to = tip }
        for curr <= to {
            var deltas, reached, cerr = collect(ctx, curr, to)
            if cerr != nil { return cerr }
            var kept, ferr = flush(deltas, reached)
            if ferr != nil { return ferr }
            logging.Info("addrstat: collected from %s, %d addresses merged, %d of them emptied",
                blocksMsg(curr, reached), len(deltas), len(deltas) - kept)
            prog.Chunk(curr, reached)
            curr = reached + 1
        }
    }
    if curr > began {
        logging.Info("addrstat: caught up with %s, %d addresses hold coins", blocksMsg(began, curr - 1), Count())
        signals.Send(signals.AddrStat)
    }
    return nil
}

// collect reads blocks from..to on workers goroutines and returns their movements
// merged, with the last height they cover. Each goroutine takes the next height
// off a shared counter, applies its block to a map of its own with no lock held —
// the address encoding, which is the scan's expensive step, included — and only
// then takes the lock to merge that map into the chunk's.
//
// Heights are handed out in order, so the blocks read are always a run from
// `from` with nothing missing: a goroutine stops taking new ones once the merged
// chunk reaches flushKeys addresses, the ones already taken are finished and
// merged, and the run ends at the highest height handed out. That is what lets
// the cursor go to it, exactly as when the blocks were read one at a time.
//
// The first failed block cancels the rest and is returned, and nothing is merged
// into the table, so the next pass retries the same heights.
func collect(parent context.Context, from, to int64) (map[string]*Stat, int64, error) {
    var ctx, cancel = context.WithCancel(parent)
    defer cancel()
    var next atomic.Int64
    next.Store(from)
    var stop atomic.Bool
    var mu sync.Mutex
    var deltas = map[string]*Stat{}
    var failure error
    var wg sync.WaitGroup
    for range workers {
        wg.Go(func() {
            for !stop.Load() {
                var h = next.Add(1) - 1
                if h > to { return }
                var blk, err = core.BlockAt(ctx, int(h))
                if err != nil {
                    mu.Lock()
                    if failure == nil { failure = fmt.Errorf("block %d: %w", h, err) }
                    mu.Unlock()
                    stop.Store(true)
                    cancel()
                    return
                }
                mu.Lock()
                apply(deltas, blk)
                var full = len(deltas) >= flushKeys
                mu.Unlock()
                if full { stop.Store(true) }
            }
        })
    }
    wg.Wait()
    if failure != nil { return nil, 0, failure }
    return deltas, min(next.Load() - 1, to), nil
}

// blocksMsg names a range of heights the way every collector's line does, in the
// singular when it is one block.
func blocksMsg(from, to int64) string {
    if from == to { return fmt.Sprintf("block %d", from) }
    return fmt.Sprintf("blocks %d..%d", from, to)
}

// apply adds one block's movements to the running chunk. A transaction is
// counted once per address however many of its outputs or inputs name it, which
// is the same dedup the index itself does — an address paid twice by one
// transaction was in one transaction.
//
// The fee is the whole transaction's, charged to every address that spends in
// it: the chain records who funded a transaction, not which of them paid for it,
// and this is what the live path already does.
//
// a coinbase spends nothing and pays no fee
func apply(deltas map[string]*Stat, blk core.Block) {
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
}

// at is the running record for the address a script pays, or nil when it pays no
// address at all — an OP_RETURN, a bare multisig, anything nonstandard, which is
// what addrbal leaves out of its count for the same reason. The address is
// encoded from the script, and its form named from the address, once per block
// rather than once per touch: a busy address appears many times in a block, and
// base58 and bech32 are the expensive part of this scan — which is why each
// block is applied on a goroutine of its own, outside the chunk's lock.
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

// flush merges a chunk into the table and advances the cursor in one
// transaction, so an interrupted catch-up resumes from the last flushed chunk
// rather than counting a block twice. It returns how many of the chunk's
// addresses still hold coins, which is what the line above it reports.
//
// The delta is added to the row in SQL rather than read, added to in Go and
// written back: one statement per address instead of a query and an update, and
// the arithmetic is where the row is. Balance and Flow are derived in the same
// statement from the sums they follow from, so they cannot drift out of step with
// them. `first` takes the earlier of the two only when there is one — a fresh row
// has 0 there, which is not earlier than anything — and `type` is only ever
// written by the insert, so a form named by hand is left as it is.
//
// An address with no row gets one, and one whose balance has reached zero loses
// the row it had: the table is the addresses that hold coins, so the delete is
// what keeps it from growing to every address the chain has ever had. It is one
// statement per address of the chunk, by primary key, rather than a sweep of the
// balance index, which would also take rows this chunk never touched.
func flush(deltas map[string]*Stat, last int64) (int, error) {
    if db == nil { return 0, nil }
    var tx, err = db.Begin()
    if err != nil { return 0, err }
    defer tx.Rollback()
    var stmt, perr = tx.Prepare(`insert into addrstat
        (addr, type, balance, recv, sent, flow, fees, txs, first, last)
        values (?, ?, ? - ?, ?, ?, ? + ?, ?, ?, ?, ?)
        on conflict(addr) do update set
            recv = addrstat.recv + excluded.recv,
            sent = addrstat.sent + excluded.sent,
            fees = addrstat.fees + excluded.fees,
            txs = addrstat.txs + excluded.txs,
            first = case when excluded.first > 0 and (addrstat.first = 0 or excluded.first < addrstat.first)
                then excluded.first else addrstat.first end,
            last = max(addrstat.last, excluded.last),
            balance = addrstat.recv + excluded.recv - (addrstat.sent + excluded.sent),
            flow = addrstat.recv + excluded.recv + addrstat.sent + excluded.sent`)
    if perr != nil { return 0, perr }
    defer stmt.Close()
    var empty, eerr = tx.Prepare(`delete from addrstat where addr = ? and balance = 0`)
    if eerr != nil { return 0, eerr }
    defer empty.Close()
    var kept int
    for addr, d := range deltas {
        if _, err := stmt.Exec(addr, d.Type, d.Recv, d.Sent, d.Recv, d.Sent, d.Recv, d.Sent,
            d.Fees, d.Txs, d.First, d.Last); err != nil {
            return 0, err
        }
        var res, derr = empty.Exec(addr)
        if derr != nil { return 0, derr }
        var gone, rerr = res.RowsAffected()
        if rerr != nil { return 0, rerr }
        if gone == 0 { kept++ }
    }
    if err := cursors.Set(tx, cursors.AddrStat, last); err != nil { return 0, err }
    return kept, tx.Commit()
}

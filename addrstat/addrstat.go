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

// recent is how many of the newest scanned blocks the aggregator leaves in
// recaddrstat rather than folding into addrstat — the depth of reorg Rollback can
// undo. It is also the distance to the tip past which Update reads blocks on
// several goroutines. A package var so tests can lower it.
var recent = 10

// workers is how many goroutines read blocks at once while the scan is more than
// `recent` blocks behind the tip: all the processors but two, which the node — on
// the Pi, the same machine — and the aggregator keep, and at least one. A
// package var so tests can raise it on a machine with few.
var workers = max(1, runtime.GOMAXPROCS(0)-2)

// aggregateRows bounds how many recaddrstat rows one aggregation folds into
// addrstat in one transaction: the transaction holds SQLite's one write lock, and
// a block reader waiting on it gives up after the busy timeout. Further rows are
// folded by the next transaction straight after. A package var so tests shrink it.
var aggregateRows int64 = 200_000

// gapBlocks is how many distinct blocks recaddrstat may hold while the height
// straight after the cursor is still missing before the aggregator reads that
// height itself on a goroutine of its own. Nothing can be folded past the gap, so
// one slow block — a node busy connecting one, a reader stuck on a timeout —
// would otherwise let the staged blocks pile up behind it. A package var so tests
// shrink it.
var gapBlocks = 100

// aggregateEvery is the backstop the aggregator wakes on when no block reader has
// told it a block was written.
var aggregateEvery = time.Minute

// mu serialises folding blocks into addrstat with Rollback, so a disconnected
// block cannot be deleted between the aggregator choosing it and folding it, which
// would move the cursor over a height whose movements were never counted.
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
// the blocks still in recaddrstat added — and false when there are none to give:
// the scan has not reached the address, or it holds nothing. A record with no
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
    return s, s.Txs > 0 && s.Balance != 0
}

// Count is how many addresses hold coins as of the newest block scanned: the
// addrstat rows, which all do, adjusted by what recaddrstat's blocks moved — an
// address they paid that has no row is one more, a row they emptied one fewer.
// It counts rather than keeping a total, so it cannot drift from what it counts,
// and the scan does not call it per block.
func Count() int {
    if db == nil { return 0 }
    var n int
    var err = db.QueryRow(`select (select count(*) from addrstat) + coalesce((select sum(case
            when s.addr is null then r.balance != 0 else (s.balance + r.balance != 0) - 1 end)
        from (select addr, sum(recv) - sum(sent) as balance from recaddrstat where addr != '' group by addr) r
        left join addrstat s on s.addr = r.addr), 0)`).Scan(&n)
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
// is deeper than `recent` — and is reported as an error. It returns how many rows
// it removed.
func Rollback(hash string) (int64, error) {
    if db == nil || !core.Enabled() { return 0, nil }
    var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    var header, err = core.GetBlockHeader(ctx, hash)
    if err != nil { return 0, err }
    mu.Lock()
    defer mu.Unlock()
    if at, ok := cursors.Get(cursors.AddrStat); ok && header.Height <= at {
        return 0, fmt.Errorf("block %d is already folded into the statistics (cursor %d)", header.Height, at)
    }
    var res, derr = db.Exec("delete from recaddrstat where block >= ?", header.Height)
    if derr != nil { return 0, derr }
    var n, _ = res.RowsAffected()
    return n, nil
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

// Update advances the statistics to tip once and returns, in two stages that run
// at once. Block readers — `workers` goroutines while more than `recent` blocks
// are left to read, one otherwise — each take the next height off a shared
// counter, read the block (core.BlockAt, one getblock at verbosity 3, whose
// prevouts are where a spend's script and amount, and so a transaction's fee, are
// written down), and write its per-address movements to recaddrstat in a
// transaction of their own. A height recaddrstat already holds was read by an
// earlier pass and is skipped.
//
// Alongside them the aggregator, woken by each block written and every
// aggregateEvery, folds the oldest blocks into addrstat until `recent` are left
// (see aggregate). It runs once more when the readers are done. When more than
// gapBlocks blocks are staged and the height straight after the cursor is still
// missing, it reads that height on a goroutine of its own, once per height, so
// one slow block does not hold every fold up behind it.
//
// A block that cannot be read stops the readers and fails the pass; the blocks
// already written stay, and the next pass reads only what is missing.
func Update(tip int64) error {
    if db == nil { return nil }
    var ctx, cancel = context.WithTimeout(context.Background(), 6*time.Hour)
    defer cancel()
    var from int64
    if last, ok := cursors.Get(cursors.AddrStat); ok { from = last + 1 }
    if from > tip { return nil }
    var done, err = scanned(from)
    if err != nil { return err }
    var left = tip - from + 1 - int64(len(done))
    var readers = 1
    if left > int64(recent) { readers = workers }
    var tally = &tally{prog: progress.New("addrstat", from, tip), said: time.Now(), read: from - 1, shown: from - 1, gap: -1}
    var changed = make(chan struct{}, 1)
    var stop = make(chan struct{})
    var aggregated = make(chan error, 1)
    var next atomic.Int64
    next.Store(from)
    var failure error
    var fmu sync.Mutex
    var fail = func(rerr error) {
        fmu.Lock()
        if failure == nil { failure = rerr }
        fmu.Unlock()
        cancel()
    }
    var wrote = func() {
        select {
        case changed <- struct{}{}:
        default:
        }
    }
    var rescues sync.WaitGroup
    var rescued = map[int64]bool{}
    go func() {
        var t = time.NewTicker(aggregateEvery)
        defer t.Stop()
        for {
            select {
            case <-stop:
                aggregated <- nil
                return
            case <-changed:
            case <-t.C:
            }
            if aerr := aggregate(tally); aerr != nil {
                aggregated <- aerr
                cancel()
                return
            }
            tally.report(false)
            if tally.staged > gapBlocks && tally.gap >= 0 && !rescued[tally.gap] {
                var gap = tally.gap
                rescued[gap] = true
                logging.Info("addrstat: block %d still missing with %d blocks staged, reading it again", gap, tally.staged)
                rescues.Go(func() {
                    if rerr := record(ctx, gap); rerr != nil {
                        fail(rerr)
                        return
                    }
                    wrote()
                })
            }
        }
    }()
    var read atomic.Int64
    var wg sync.WaitGroup
    for range readers {
        wg.Go(func() {
            for ctx.Err() == nil {
                var h = next.Add(1) - 1
                if h > tip { return }
                if done[h] { continue }
                if rerr := record(ctx, h); rerr != nil {
                    fail(rerr)
                    return
                }
                read.Add(1)
                wrote()
            }
        })
    }
    wg.Wait()
    close(stop)
    var aerr = <-aggregated
    rescues.Wait()
    if failure != nil { return failure }
    if aerr != nil { return aerr }
    var ferr = aggregate(tally)
    tally.report(true)
    if ferr != nil { return ferr }
    if read.Load() > 0 {
        var h, _ = Height()
        logging.Info("addrstat: caught up with %s, %d addresses hold coins", blocksMsg(from, h), Count())
        signals.Send(signals.AddrStat)
    }
    return nil
}

// scanned is the set of heights from `from` up that recaddrstat holds, which an
// earlier pass read and wrote and this one must not count again.
func scanned(from int64) (map[int64]bool, error) {
    var rows, err = db.Query("select distinct block from recaddrstat where block >= ?", from)
    if err != nil { return nil, err }
    defer rows.Close()
    var done = map[int64]bool{}
    for rows.Next() {
        var h int64
        if serr := rows.Scan(&h); serr != nil { return nil, serr }
        done[h] = true
    }
    return done, rows.Err()
}

// record reads block h and writes what it moved to recaddrstat, one row per
// address plus the empty address's row marking the block read, in one
// transaction. Whatever an earlier write left under h is replaced, so a block
// written again — a reorg racing a pass, or a slow reader finishing a block the
// aggregator had read again to close the gap behind the cursor — is not counted
// twice. A block already folded is not written at all: its rows would sit below
// the cursor, counted twice by every read until the next fold cleared them. The
// write holds mu, so a fold cannot move the cursor between the check and it.
func record(ctx context.Context, h int64) error {
    var blk, err = core.BlockAt(ctx, int(h))
    if err != nil { return fmt.Errorf("block %d: %w", h, err) }
    var deltas = map[string]*Stat{}
    apply(deltas, blk)
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

// aggregate folds recaddrstat's oldest blocks into addrstat until `recent` are
// left, a transaction of at most aggregateRows rows at a time. Only a run of
// heights straight on from the cursor can be folded — the cursor goes to the
// last of them, and a height still being read would otherwise be stepped over —
// so it stops at the first gap and waits for the reader that will fill it.
//
// What it folded goes into t, which says so in the log at most once a minute.
func aggregate(t *tally) error {
    for {
        var moved, err = fold(t)
        if err != nil || !moved { return err }
    }
}

// tally is what the aggregator has folded since it last said so. The aggregator
// folds whenever a block is written, so a line per fold would be a line per
// block of a build that reads hundreds a second.
type tally struct {
    prog    *progress.Progress
    said    time.Time
    folded  bool
    from    int64
    to      int64
    read    int64
    shown   int64
    staged  int
    gap     int64
    merged  int64
    emptied int64
}

// report logs what has been folded and how far the scan has read, if a minute has
// passed since the last line or force says the pass is over. Progress is the run
// of heights read, not folded: that is how far the scan has got, the newest
// `recent` blocks being as read as the rest.
func (t *tally) report(force bool) {
    if !force && time.Since(t.said) < aggregateEvery { return }
    t.said = time.Now()
    if t.folded {
        logging.Info("addrstat: folded %s, %d addresses merged, %d of them emptied",
            blocksMsg(t.from, t.to), t.merged, t.emptied)
    }
    if t.read > t.shown {
        t.prog.Chunk(t.shown+1, t.read)
        t.shown = t.read
    }
    t.folded, t.merged, t.emptied = false, 0, 0
}

// fold is one of aggregate's transactions, and reports whether it moved the
// cursor. It notes in t how many blocks are staged and, when the height straight
// after the cursor is not among them, that height — which Update reads again on a
// goroutine of its own once more than gapBlocks are staged behind it.
//
// The movements are summed per address in SQL and added to the row in the same
// statement — one statement for the lot rather than a query and an update per
// address. Balance and Flow are derived from the sums they follow from, so they
// cannot drift out of step with them. `first` takes the earlier of the two only
// when there is one — a fresh row has 0 there, which is not earlier than anything
// — and `type` is only ever written by the insert, so a form named by hand is left
// as it is.
//
// An address with no row gets one, and one whose balance has reached zero loses
// the row it had: the table is the addresses that hold coins, so the delete is
// what keeps it from growing to every address the chain has ever had. It reaches
// only the addresses the folded blocks touched, rather than sweeping the balance
// index, which would also take rows these blocks never touched.
func fold(t *tally) (bool, error) {
    mu.Lock()
    defer mu.Unlock()
    var cursor = int64(-1)
    if last, ok := cursors.Get(cursors.AddrStat); ok { cursor = last }
    var rows, err = db.Query("select block, count(*) from recaddrstat where block > ? group by block order by block", cursor)
    if err != nil { return false, err }
    var heights, counts []int64
    for rows.Next() {
        var h, n int64
        if serr := rows.Scan(&h, &n); serr != nil {
            rows.Close()
            return false, serr
        }
        heights = append(heights, h)
        counts = append(counts, n)
    }
    rows.Close()
    if rerr := rows.Err(); rerr != nil { return false, rerr }
    var run = 0
    for run < len(heights) && heights[run] == cursor+1+int64(run) { run++ }
    t.staged, t.gap = len(heights), -1
    if len(heights) > 0 && run == 0 { t.gap = cursor + 1 }
    if run > 0 { t.read = heights[run-1] }
    var foldable = min(run, len(heights)-recent)
    var to, total = cursor, int64(0)
    for i := range max(foldable, 0) {
        if to > cursor && total+counts[i] > aggregateRows { break }
        to = heights[i]
        total += counts[i]
    }
    if to == cursor { return false, nil }
    var tx, terr = db.Begin()
    if terr != nil { return false, terr }
    defer tx.Rollback()
    var res, uerr = tx.Exec(`insert into addrstat (addr, type, balance, recv, sent, flow, fees, txs, first, last)
        select addr, max(type), sum(recv) - sum(sent), sum(recv), sum(sent), sum(recv) + sum(sent), sum(fees),
            sum(txs), coalesce(min(nullif(first, 0)), 0), max(last)
        from recaddrstat where block > ? and block <= ? and addr != '' group by addr
        on conflict(addr) do update set
            recv = addrstat.recv + excluded.recv,
            sent = addrstat.sent + excluded.sent,
            fees = addrstat.fees + excluded.fees,
            txs = addrstat.txs + excluded.txs,
            first = case when excluded.first > 0 and (addrstat.first = 0 or excluded.first < addrstat.first)
                then excluded.first else addrstat.first end,
            last = max(addrstat.last, excluded.last),
            balance = addrstat.recv + excluded.recv - (addrstat.sent + excluded.sent),
            flow = addrstat.recv + excluded.recv + addrstat.sent + excluded.sent`, cursor, to)
    if uerr != nil { return false, uerr }
    var merged, _ = res.RowsAffected()
    var gone, derr = tx.Exec(`delete from addrstat where balance = 0 and addr in
        (select addr from recaddrstat where block > ? and block <= ? and addr != '')`, cursor, to)
    if derr != nil { return false, derr }
    var emptied, _ = gone.RowsAffected()
    if _, rerr := tx.Exec("delete from recaddrstat where block <= ?", to); rerr != nil { return false, rerr }
    if serr := cursors.Set(tx, cursors.AddrStat, to); serr != nil { return false, serr }
    if cerr := tx.Commit(); cerr != nil { return false, cerr }
    if !t.folded { t.from = cursor + 1 }
    t.folded = true
    t.to = to
    t.merged += merged
    t.emptied += emptied
    return true, nil
}

// blocksMsg names a range of heights the way every collector's line does, in the
// singular when it is one block.
func blocksMsg(from, to int64) string {
    if from == to { return fmt.Sprintf("block %d", from) }
    return fmt.Sprintf("blocks %d..%d", from, to)
}

// apply adds one block's movements to deltas. A transaction is
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
// base58 and bech32 are the expensive part of this scan — which is why blocks
// are read and applied on several goroutines at once.
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

// Command addrstat builds and queries the bot's per-address statistics from the
// command line. It drives the same addrstat package the bot does, against the
// same SQLite table and the same cursor, so statistics built here are ones the
// bot can serve and vice versa — point -db at the bot's own database to extend
// it, or at a file of its own to work on a copy.
//
// Usage:
//
//	addrstat build -db bitnsbot.sqlite -coreURL http://127.0.0.1:8332 -coreCookie ./cookie
//	addrstat query -db bitnsbot.sqlite -coreURL http://127.0.0.1:8332 -coreCookie ./cookie <address>
//
// build is the fast first build, in three stages. It reads blocks in parallel
// from the cursor to five blocks below the tip and sums their movements per
// address in an in-memory database, writing it to a range file on disk —
// range000000..183422.db, named for the blocks it holds — each time it reaches
// -rows addresses, and carrying on in a fresh one. Then it merges the range
// files two at a time until one is left, and adds that one to the addrstat table
// with the cursor moved to its last block, and exits, leaving the bot's index
// catch-up goroutine to keep up from there one block at a time. query prints
// what the table holds for one address. query reads only the database — the node
// is asked for its tip, best effort, so the answer can say how far behind it
// stands.
package main

import "cmp"
import "context"
import "database/sql"
import "errors"
import "flag"
import "fmt"
import "io"
import "os"
import "os/signal"
import "path/filepath"
import "regexp"
import "runtime"
import "slices"
import "strconv"
import "strings"
import "sync"
import "syscall"
import "time"
import _ "modernc.org/sqlite"
import "bitnsbot/addrstat"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/progress"

// options are the flags both commands share, named as the bot's own are, so a
// line of the bot's config file reads the same here.
type options struct {
    db      string
    dir     string
    rows    int64
    url     string
    user    string
    pass    string
    cookie  string
    verbose int
}

func flags(fs *flag.FlagSet) *options {
    var o = &options{}
    fs.StringVar(&o.db, "db", "", "the SQLite database holding the addrstat table")
    fs.StringVar(&o.dir, "dir", "", "build: the directory range files are written to and merged in (default: the database's)")
    fs.Int64Var(&o.rows, "rows", 100_000_000, "build: how many addresses a range is summed in memory before it is written to disk")
    fs.StringVar(&o.url, "coreURL", "http://127.0.0.1:8332", "Bitcoin Core JSON-RPC URL")
    fs.StringVar(&o.user, "coreUser", "", "Bitcoin Core RPC username (or use -coreCookie)")
    fs.StringVar(&o.pass, "corePass", "", "Bitcoin Core RPC password (or use -coreCookie)")
    fs.StringVar(&o.cookie, "coreCookie", "", "path to Bitcoin Core's .cookie file")
    fs.IntVar(&o.verbose, "verbose", 1, "log level: 0 quiet, 1 progress, 2 every request")
    return o
}

func usage() {
    fmt.Fprintln(os.Stderr, "usage: addrstat <command> -db <database> [flags] [address]")
    fmt.Fprintln(os.Stderr, "")
    fmt.Fprintln(os.Stderr, "commands:")
    fmt.Fprintln(os.Stderr, "  build  catch the statistics up from their cursor to the chain tip")
    fmt.Fprintln(os.Stderr, "  query  print the statistics held for an address")
}

func main() {
    if len(os.Args) < 2 {
        usage()
        os.Exit(2)
    }
    var cmd = os.Args[1]
    if cmd != "build" && cmd != "query" {
        fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
        usage()
        os.Exit(2)
    }
    var fs = flag.NewFlagSet(cmd, flag.ExitOnError)
    var opt = flags(fs)
    fs.Parse(os.Args[2:])
    logging.SetVerbose(opt.verbose)
    if opt.db == "" { logging.Fatal("%s needs -db naming the SQLite database", cmd) }
    if cmd == "query" && fs.NArg() != 1 {
        fmt.Fprintln(os.Stderr, "query needs exactly one address")
        os.Exit(2)
    }
    var handle, err = openDB(opt.db, cmd == "query")
    if err != nil { logging.Fatal("open %s: %v", opt.db, err) }
    defer handle.Close()
    switch cmd {
    case "build":
        if berr := build(opt, handle, os.Stdout); berr != nil { logging.Fatal("build: %v", berr) }
    case "query":
        var found, qerr = query(opt, fs.Arg(0), os.Stdout)
        if qerr != nil { logging.Fatal("query: %v", qerr) }
        if !found {
            handle.Close()
            os.Exit(1)
        }
    }
}

// openDB opens the database with the pragmas the bot opens its own with — WAL,
// so the table can be read while a build writes it, and a busy timeout, so a bot
// sharing the file makes this wait rather than fail — and hands the handle to
// cursors and addrstat, which create their tables when they are missing.
//
// existing is query's: SQLite would otherwise create the file and answer that
// nothing is known, which reads like a scan that never ran rather than a
// mistyped path.
//
// SQLite's scratch goes beside the database: a first build's install creates
// the three ranking indexes over every address on the chain, each a sort of
// gigabytes, which would otherwise land in /tmp — memory-backed on this repo's
// own machine. It has to be set before SQLite is first used: modernc's libc
// copies the environment once, on the first call into it.
func openDB(path string, existing bool) (*sql.DB, error) {
    if existing {
        if _, err := os.Stat(path); err != nil { return nil, err }
    }
    os.Setenv("SQLITE_TMPDIR", filepath.Dir(path))
    var handle, err = sql.Open("sqlite", "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)")
    if err != nil { return nil, err }
    if cerr := cursors.Init(handle); cerr != nil {
        handle.Close()
        return nil, cerr
    }
    if aerr := addrstat.Init(handle); aerr != nil {
        handle.Close()
        return nil, aerr
    }
    return handle, nil
}

// behind is how far below the tip a build stops, so the bot's own Update — which
// stages the newest blocks where a reorg can take them out — carries on from
// there. An install goes straight into addrstat and cannot be undone.
const behind = 5

// attempts is how many times a block is asked for before the build gives up on
// it, waiting retryDelay, then twice that, and so on between them: 1, 2, 4 and
// 8 seconds.
const attempts = 5

// retryDelay is the first wait between two tries at a block. A package var so
// tests do not sit through it.
var retryDelay = time.Second

// workers is how many goroutines read blocks at once: all the processors but
// two, which the node and the database keep, and at least one.
var workers = max(1, runtime.GOMAXPROCS(0)-2)

// progressEvery is how many applied blocks a progress line covers.
const progressEvery = 1000

// onConflict is what an insert into addrstat does with an address that already
// has a row: add the movements to it. It is addrstat's own clause, which the
// package keeps unexported, and so the same sums its fold makes — Balance and
// Flow derived from the sums they follow from, `first` taking the earlier of the
// two only when there is one, and `type` written only by the insert.
// TestBuildMatchesUpdate is what holds the two copies together. Shared by the
// in-memory stage and the install, each of which names its table addrstat.
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

// rangeSchema is the table an in-memory stage sums into and a range file holds:
// addrstat's columns, keyed by address alone. Without rowid, so the address is
// stored once rather than in the row and again in its key's index, and with none
// of the bot's ranking indexes, which nothing reads a range file through.
const rangeSchema = `create table addrstat (addr TEXT PRIMARY KEY, type TEXT NOT NULL, balance INTEGER NOT NULL,
    recv INTEGER NOT NULL, sent INTEGER NOT NULL, flow INTEGER NOT NULL, fees INTEGER NOT NULL,
    txs INTEGER NOT NULL, first INTEGER NOT NULL, last INTEGER NOT NULL) without rowid`

// upsert applies one address's movements in one block to a stage.
const upsert = `insert into addrstat (addr, type, balance, recv, sent, flow, fees, txs, first, last)
    values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)` + onConflict

// mergeRows sums two attached range files, a and b, into the empty table of a
// third. Each arm is a scan of its file's primary key, so it comes out in
// address order and SQLite merges the two (MERGE (UNION ALL)) rather than
// sorting them, and the group by consumes that merged stream as it comes —
// nothing is sorted and no temporary B-tree is built, so a merge is one pass over
// each file, and its rows are inserted in key order, each an append. An address
// in both files is summed as addrstat's fold sums blocks; one in either alone is
// copied as it is.
const mergeRows = `insert into addrstat (addr, type, balance, recv, sent, flow, fees, txs, first, last)
    select addr, max(type), sum(recv) - sum(sent), sum(recv), sum(sent), sum(recv) + sum(sent), sum(fees), sum(txs),
        coalesce(min(nullif(first, 0)), 0), max(last)
    from (select addr, type, recv, sent, fees, txs, first, last from a.addrstat
        union all select addr, type, recv, sent, fees, txs, first, last from b.addrstat order by addr)
    group by addr`

// installRows adds an attached range file, r, to the database's addrstat table,
// upserting every row of it in address order. The `where true` is SQLite's own
// advice for an upsert fed by a select, whose ON would otherwise parse as a join
// constraint.
const installRows = `insert into addrstat (addr, type, balance, recv, sent, flow, fees, txs, first, last)
    select addr, type, balance, recv, sent, flow, fees, txs, first, last from r.addrstat where true` + onConflict

// rangeFile matches a range file's name, and the temporary one it is written
// under before it is whole.
var rangeFile = regexp.MustCompile(`^range(\d+)\.\.(\d+)\.db(\.tmp)?$`)

// fetched is one block's movements per address, or why it could not be read.
type fetched struct {
    deltas map[string]*addrstat.Stat
    err    error
}

// job is a height for a worker to read and the slot its result goes in.
type job struct {
    height int64
    slot   chan fetched
}

// span is a range file: the blocks from..to whose movements it holds, summed per
// address, and where it is.
type span struct {
    from int64
    to   int64
    path string
}

// build pre-builds the statistics from their cursor to the tip less `behind`
// and exits, in three stages. The bot's addrstat.Update is the keep-up path —
// one block at a time, staged — and this is the fast first build, which keeps
// the summing in memory and away from the bot's indexed table until the end.
//
// First collect: `workers` goroutines read blocks and work out their movements,
// which is where the time goes (a getblock at verbosity 3 and an address encoded
// per script), and one goroutine applies them in height order to an in-memory
// database, writing it to a range file each time it holds opt.rows addresses.
// Then mergeAll merges the range files two at a time until one is left. Then
// install adds that one to the addrstat table and moves the cursor to its last
// block, in one transaction.
//
// Range files outlive the run that wrote them, in opt.dir (the database's own
// directory unless it names another), so a build that stops partway — by Ctrl+C,
// which writes out what the in-memory database holds first, or by a failure —
// carries on from the last block they hold rather than from the cursor, and one
// that stops while merging carries on merging. They must run on from the cursor
// without a gap; one that does not is reported rather than counted. After the
// first Ctrl+C a second one kills the build outright, for an operator unwilling
// to wait for the in-memory database to be written.
//
// A block that still cannot be read after `attempts` tries is reported and
// skipped: its height is printed and its range steps over it, so what it moved
// is missing from the statistics. The heights skipped are listed at the end.
func build(opt *options, handle *sql.DB, out io.Writer) error {
    if opt.rows < 1 { return errors.New("-rows must be at least 1") }
    if err := core.Init(opt.url, opt.user, opt.pass, opt.cookie); err != nil { return err }
    var tctx, tcancel = context.WithTimeout(context.Background(), 30*time.Second)
    var tip, err = core.GetBlockCount(tctx)
    tcancel()
    if err != nil { return fmt.Errorf("Core RPC is unreachable at %s: %w", opt.url, err) }
    var from int64
    if h, ok := cursors.Get(cursors.AddrStat); ok { from = h + 1 }
    var dir = opt.dir
    if dir == "" { dir = filepath.Dir(opt.db) }
    var done, rerr = ranges(dir)
    if rerr != nil { return rerr }
    var next = from
    for _, s := range done {
        if s.from != next { return fmt.Errorf("%s does not start at block %d, the next one the statistics need", s.path, next) }
        next = s.to + 1
    }
    var to = tip - behind
    if next > to && len(done) == 0 {
        fmt.Fprintf(out, "Statistics are already within %d blocks of the tip (block %d), %d addresses\n", behind, tip, addrstat.Count())
        return nil
    }
    var ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    context.AfterFunc(ctx, stop)
    var started = time.Now()
    if next <= to {
        fmt.Fprintf(out, "Collecting blocks %d..%d on %d workers into range files of %d addresses in %s\n", next, to, workers, opt.rows, dir)
        var skipped, cerr = collect(ctx, next, to, dir, opt.rows, out)
        if len(skipped) > 0 { fmt.Fprintf(out, "Skipped %d blocks: %v\n", len(skipped), skipped) }
        if cerr != nil { return cerr }
    }
    var last, merr = mergeAll(ctx, dir, out)
    if merr != nil { return merr }
    if ierr := install(ctx, handle, last, out); ierr != nil { return ierr }
    fmt.Fprintf(out, "Collected %d blocks up to %d in %s, %d addresses\n",
        last.to-from+1, last.to, took(time.Since(started)), addrstat.Count())
    return nil
}

// collect reads from..to on the workers and applies each block's movements in
// height order to an in-memory stage — a slot per height, queued as the heights
// are handed out and read back in that order — writing the stage to its range
// file and opening a fresh one each time it holds rows addresses, and writing
// the last one at the end. It returns the heights it skipped.
//
// Height order is what makes a range file's name true: when a stage is written,
// every block up to the one just applied is in it and none after. A cancelled
// context stops the reading, and what the stage holds is still written, up to
// the block before the first one not read, so a stopped build loses nothing it
// had read. A failed apply loses its stage, which holds part of a block, and a
// failed write loses the one it was writing; a later build reads those blocks
// again.
func collect(parent context.Context, from, to int64, dir string, rows int64, out io.Writer) ([]int64, error) {
    var st, err = newStage(from)
    if err != nil { return nil, err }
    defer func() { st.close() }()
    var ctx, cancel = context.WithCancel(parent)
    defer cancel()
    var jobs = make(chan job)
    var order = make(chan chan fetched, workers*4)
    var wg sync.WaitGroup
    for range workers {
        wg.Go(func() {
            for j := range jobs { j.slot <- read(ctx, j.height) }
        })
    }
    go func() {
        defer close(order)
        defer close(jobs)
        for h := from; h <= to && ctx.Err() == nil; h++ {
            var slot = make(chan fetched, 1)
            order <- slot
            jobs <- job{height: h, slot: slot}
        }
    }()
    var prog = progress.New("addrstat", from, to)
    var skipped []int64
    var failure error
    var chunk = from
    var h = from
    for slot := range order {
        if failure != nil { continue }
        var f = <-slot
        if f.err != nil && ctx.Err() != nil {
            failure = fmt.Errorf("stopped at block %d: %w", h, ctx.Err())
            cancel()
            continue
        }
        if f.err != nil {
            fmt.Fprintf(out, "Skipping block %d: %v\n", h, f.err)
            skipped = append(skipped, h)
        }
        var full, aerr = st.add(f.deltas, rows)
        if aerr != nil {
            failure = fmt.Errorf("apply block %d: %w", h, aerr)
            st.close()
            cancel()
            continue
        }
        if full && h < to {
            if derr := st.dump(dir, h, out); derr != nil {
                failure = derr
                cancel()
                continue
            }
            var fresh, nerr = newStage(h + 1)
            if nerr != nil {
                failure = nerr
                cancel()
                continue
            }
            st = fresh
        }
        if h-chunk+1 >= progressEvery || h == to {
            prog.Chunk(chunk, h)
            chunk = h + 1
        }
        h++
    }
    if failure == nil && h <= to { failure = fmt.Errorf("stopped at block %d: %w", h, context.Cause(ctx)) }
    wg.Wait()
    if st.handle != nil && h > st.from { failure = errors.Join(failure, st.dump(dir, h-1, out)) }
    return skipped, failure
}

// stage is the in-memory database one range of blocks is summed in: from is the
// block it starts at, and bound an upper bound on how many addresses it holds —
// its count when last counted, plus every address applied since.
type stage struct {
    handle *sql.DB
    conn   *sql.Conn
    tx     *sql.Tx
    stmt   *sql.Stmt
    from   int64
    bound  int64
}

// newStage opens a fresh in-memory database for the blocks from `from` on, and
// begins the one transaction everything up to its dump is applied in. An
// in-memory database belongs to the connection that opened it, so the pool is
// held to one and that one is pinned with a sql.Conn: a second connection would
// be a second, empty database.
//
// Its pages are 64 KB, which halves what it costs in memory: each page SQLite
// caches is allocated as the page plus a header, and modernc's allocator rounds
// that up to a power of two, so a 4 KB page takes 8 KB, where a 64 KB one is
// mapped whole. Measured at 4.4 M rows, 189 bytes a row of resident memory at
// 4 KB pages against 101 at 64 KB — 19 GB at a hundred million rows against
// 10 — for an upsert about a tenth slower. The size has to be set before the
// first table is created, so it is in the DSN; VACUUM INTO keeps it, so range
// files have 64 KB pages too.
func newStage(from int64) (*stage, error) {
    var handle, err = sql.Open("sqlite", "file::memory:?_pragma=page_size(65536)")
    if err != nil { return nil, err }
    handle.SetMaxOpenConns(1)
    var s = &stage{handle: handle, from: from}
    s.conn, err = handle.Conn(context.Background())
    if err == nil { s.tx, err = s.conn.BeginTx(context.Background(), nil) }
    if err == nil { _, err = s.tx.Exec(rangeSchema) }
    if err == nil { s.stmt, err = s.tx.Prepare(upsert) }
    if err != nil {
        s.close()
        return nil, err
    }
    return s, nil
}

// add applies one block's movements, an upsert per address adding them to its
// row the way addrstat's fold adds a block's, and reports whether the stage
// holds limit addresses. Counting is a scan of the table — a third of a second
// at a hundred million rows — so it is done only once bound says the limit could
// have been reached: a block cannot add more rows than it names addresses.
func (s *stage) add(deltas map[string]*addrstat.Stat, limit int64) (bool, error) {
    for addr, d := range deltas {
        if _, err := s.stmt.Exec(addr, d.Type, d.Recv-d.Sent, d.Recv, d.Sent, d.Recv+d.Sent, d.Fees, d.Txs,
            d.First, d.Last); err != nil {
            return false, err
        }
    }
    s.bound += int64(len(deltas))
    if s.bound < limit { return false, nil }
    if err := s.tx.QueryRow("select count(*) from addrstat").Scan(&s.bound); err != nil { return false, err }
    return s.bound >= limit, nil
}

// dump writes the stage — blocks s.from..to — to its range file in dir, and
// closes it, freeing its memory. VACUUM INTO writes the table out in address
// order, which is what lets a merge read two range files in one pass each. It
// writes a temporary name that place moves into place once the file is whole and
// on disk, so a range file is always a complete one: the next run trusts its
// name to say which blocks it holds.
func (s *stage) dump(dir string, to int64, out io.Writer) error {
    defer s.close()
    var started = time.Now()
    var n int64
    if cerr := s.tx.QueryRow("select count(*) from addrstat").Scan(&n); cerr != nil { return cerr }
    if terr := s.tx.Commit(); terr != nil { return terr }
    var path = rangeName(dir, s.from, to)
    if _, verr := s.conn.ExecContext(context.Background(), "vacuum into ?", path+".tmp"); verr != nil {
        return fmt.Errorf("write %s: %w", path, verr)
    }
    if perr := place(path); perr != nil { return perr }
    fmt.Fprintf(out, "Wrote blocks %d..%d to %s, %d addresses in %s\n", s.from, to, filepath.Base(path), n, took(time.Since(started)))
    return nil
}

// close frees the stage's memory — an in-memory database goes with its one
// connection — and does nothing the second time.
func (s *stage) close() {
    if s.handle == nil { return }
    if s.tx != nil { s.tx.Rollback() }
    if s.conn != nil { s.conn.Close() }
    s.handle.Close()
    s.handle = nil
}

// mergeAll merges the range files in dir two at a time until one is left, and
// returns it. Each round merges the first with the second, the third with the
// fourth and so on, an odd one out waiting for the next round, so every address
// is rewritten once a round, about log2 of the files' count times in all —
// where folding each file into one growing result would rewrite that result once
// per file. Each round lists the directory afresh, so a build stopped mid-merge
// carries on from what is on disk.
func mergeAll(ctx context.Context, dir string, out io.Writer) (span, error) {
    for {
        var all, err = ranges(dir)
        if err != nil { return span{}, err }
        if len(all) == 0 { return span{}, fmt.Errorf("no range files in %s", dir) }
        if len(all) == 1 { return all[0], nil }
        for i := 0; i+1 < len(all); i += 2 {
            if merr := merge(ctx, all[i], all[i+1], out); merr != nil { return span{}, merr }
        }
    }
}

// merge sums two adjacent range files into the one covering both (mergeRows),
// and deletes them once it is in place. The result is written under a temporary
// name without a journal — it is nothing until it is whole, and a failed merge
// leaves only a temporary file the next listing deletes — and moved into place by
// place. The inputs go after it, so a stop between the two leaves files the
// result covers, which the next listing deletes too.
func merge(ctx context.Context, a, b span, out io.Writer) error {
    var started = time.Now()
    var path = rangeName(filepath.Dir(a.path), a.from, b.to)
    var handle, err = sql.Open("sqlite", "file:"+path+".tmp?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
    if err != nil { return err }
    defer handle.Close()
    handle.SetMaxOpenConns(1)
    var conn, cerr = handle.Conn(ctx)
    if cerr != nil { return cerr }
    defer conn.Close()
    if _, serr := conn.ExecContext(ctx, rangeSchema); serr != nil { return serr }
    if _, aerr := conn.ExecContext(ctx, "attach ? as a", a.path); aerr != nil { return aerr }
    if _, berr := conn.ExecContext(ctx, "attach ? as b", b.path); berr != nil { return berr }
    var res, merr = conn.ExecContext(ctx, mergeRows)
    if merr != nil { return fmt.Errorf("merge %s and %s: %w", filepath.Base(a.path), filepath.Base(b.path), merr) }
    var n, _ = res.RowsAffected()
    conn.Close()
    handle.Close()
    if perr := place(path); perr != nil { return perr }
    if rerr := os.Remove(a.path); rerr != nil { return rerr }
    if rerr := os.Remove(b.path); rerr != nil { return rerr }
    fmt.Fprintf(out, "Merged %s and %s into %s, %d addresses in %s\n",
        filepath.Base(a.path), filepath.Base(b.path), filepath.Base(path), n, took(time.Since(started)))
    return nil
}

// install adds the one range file left to the database's addrstat table and
// moves the cursor to its last block, in one transaction, then deletes it.
// Whatever recaddrstat holds at or below that block is deleted in the same
// transaction, those blocks being counted now, so a build over a database the
// bot has staged blocks in does not count them twice.
//
// Into an empty table — a first build — the three ranking indexes are dropped in
// that transaction and built again once it commits, by addrstat.Init, which
// creates whatever is missing: a row inserted under an index on balance or date
// lands at a random place in it, a billion times over, where building the index
// afterwards is one sort. A stop between the commit and the indexes leaves them
// to the next Init, the bot's or this tool's. A table that already holds rows
// keeps its indexes, the addresses a later build adds being a small share of
// them, and every address is upserted.
//
// The range file is attached to one connection of the pool, which is detached
// again before the connection goes back to it.
func install(ctx context.Context, handle *sql.DB, r span, out io.Writer) error {
    var started = time.Now()
    var conn, err = handle.Conn(ctx)
    if err != nil { return err }
    defer conn.Close()
    if _, aerr := conn.ExecContext(ctx, "attach ? as r", r.path); aerr != nil { return aerr }
    defer conn.ExecContext(context.Background(), "detach r")
    var empty bool
    if qerr := conn.QueryRowContext(ctx, "select not exists (select 1 from addrstat)").Scan(&empty); qerr != nil { return qerr }
    var tx, terr = conn.BeginTx(ctx, nil)
    if terr != nil { return terr }
    defer tx.Rollback()
    if empty {
        for _, index := range []string{"addrstat_balance", "addrstat_txs", "addrstat_last"} {
            if _, derr := tx.ExecContext(ctx, "drop index if exists "+index); derr != nil { return derr }
        }
    }
    var res, ierr = tx.ExecContext(ctx, installRows)
    if ierr != nil { return fmt.Errorf("install %s: %w", filepath.Base(r.path), ierr) }
    var n, _ = res.RowsAffected()
    if _, derr := tx.ExecContext(ctx, "delete from recaddrstat where block <= ?", r.to); derr != nil { return derr }
    if serr := cursors.Set(tx, cursors.AddrStat, r.to); serr != nil { return serr }
    if cerr := tx.Commit(); cerr != nil { return cerr }
    fmt.Fprintf(out, "Installed %s, %d addresses, in %s\n", filepath.Base(r.path), n, took(time.Since(started)))
    if empty {
        var indexed = time.Now()
        if nerr := addrstat.Init(handle); nerr != nil { return fmt.Errorf("index addrstat: %w", nerr) }
        fmt.Fprintf(out, "Indexed the addresses in %s\n", took(time.Since(indexed)))
    }
    return os.Remove(r.path)
}

// ranges lists the range files in dir in height order. Two kinds of leftover are
// deleted on the way: a temporary file, which a stopped run was still writing and
// so holds nothing whole, and a range file whose blocks another one covers, which
// a merge stopped between placing its result and deleting its inputs left behind,
// already counted in that result. Two files that overlap otherwise are an error,
// neither being trustworthy about which blocks it holds.
func ranges(dir string) ([]span, error) {
    var entries, err = os.ReadDir(dir)
    if err != nil { return nil, err }
    var all []span
    for _, e := range entries {
        var m = rangeFile.FindStringSubmatch(e.Name())
        if m == nil || !e.Type().IsRegular() { continue }
        var path = filepath.Join(dir, e.Name())
        if m[3] != "" {
            if rerr := os.Remove(path); rerr != nil { return nil, rerr }
            continue
        }
        var from, _ = strconv.ParseInt(m[1], 10, 64)
        var to, _ = strconv.ParseInt(m[2], 10, 64)
        if to < from { return nil, fmt.Errorf("range file %s ends before it starts", path) }
        all = append(all, span{from: from, to: to, path: path})
    }
    slices.SortFunc(all, func(a, b span) int { return cmp.Or(cmp.Compare(a.from, b.from), cmp.Compare(b.to, a.to)) })
    var kept []span
    for _, s := range all {
        var n = len(kept)
        if n == 0 || s.from > kept[n-1].to {
            kept = append(kept, s)
            continue
        }
        if s.to > kept[n-1].to { return nil, fmt.Errorf("range files %s and %s overlap", kept[n-1].path, s.path) }
        if rerr := os.Remove(s.path); rerr != nil { return nil, rerr }
    }
    return kept, nil
}

// rangeName is the range file in dir holding blocks from..to, its heights padded
// so a listing of the directory reads in height order.
func rangeName(dir string, from, to int64) string {
    return filepath.Join(dir, fmt.Sprintf("range%06d..%06d.db", from, to))
}

// place moves a finished range file from its temporary name to path, syncing it
// first, so a rename that survives a crash never names a file whose contents did
// not.
func place(path string) error {
    var f, err = os.Open(path + ".tmp")
    if err != nil { return err }
    var serr = f.Sync()
    f.Close()
    if serr != nil { return serr }
    return os.Rename(path+".tmp", path)
}

// read asks the node for block h up to `attempts` times, doubling the wait
// between tries from retryDelay, and works out what it moved. A cancelled context
// ends the waiting at once.
func read(ctx context.Context, h int64) fetched {
    var delay = retryDelay
    var last error
    for try := 1; try <= attempts; try++ {
        var blk, err = core.BlockAt(ctx, int(h))
        if err == nil { return fetched{deltas: addrstat.Deltas(blk)} }
        last = err
        if try == attempts { break }
        select {
        case <-time.After(delay):
        case <-ctx.Done():
            return fetched{err: ctx.Err()}
        }
        delay *= 2
    }
    return fetched{err: fmt.Errorf("%d attempts: %w", attempts, last)}
}

// query prints the statistics held for an address, and reports false when there
// are none: the scan has not been past it. The node is asked for its tip only to
// say how current the answer is, so a node that cannot be reached costs that line
// and nothing else.
func query(opt *options, address string, out io.Writer) (bool, error) {
    var at, built = addrstat.Height()
    if !built { return false, errors.New("the statistics have never been built — run addrstat build first") }
    var stat, ok = addrstat.Get(address)
    if !ok {
        fmt.Fprintf(out, "No statistics for %s as of block %d\n", address, at)
        fmt.Fprint(out, current(opt, at))
        return false, nil
    }
    var rows = [][2]string{
        {"Address:", address},
        {"Type:", stat.Type},
        {"Balance:", sats(stat.Balance)},
        {"Received:", sats(stat.Recv)},
        {"Sent:", sats(stat.Sent)},
        {"Flow:", sats(stat.Flow)},
        {"Fees:", sats(stat.Fees)},
        {"Transactions:", group(stat.Txs)},
        {"First tx:", stamp(stat.First)},
        {"Last tx:", stamp(stat.Last)},
        {"As of block:", group(at)},
    }
    for _, r := range rows { fmt.Fprintf(out, "%-14s %s\n", r[0], r[1]) }
    fmt.Fprint(out, current(opt, at))
    return true, nil
}

// current is the line saying how far the statistics stand behind the node's
// tip, or nothing when they are at it or the node did not answer.
func current(opt *options, at int64) string {
    if opt.url == "" { return "" }
    if err := core.Init(opt.url, opt.user, opt.pass, opt.cookie); err != nil { return "" }
    var ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    var tip, err = core.GetBlockCount(ctx)
    if err != nil || tip <= at { return "" }
    return fmt.Sprintf("The node's tip is block %s, %s blocks ahead — run addrstat build to catch up\n", group(tip), group(tip-at))
}

func sats(n int64) string { return group(n) + " sat" }

// group writes a number with its thousands space-separated, the bot's own way,
// splitting the sign off first so a negative is not grouped through it.
func group(n int64) string {
    var sign = ""
    if n < 0 {
        sign = "-"
        n = -n
    }
    var digits = strconv.FormatInt(n, 10)
    var b strings.Builder
    for i, c := range digits {
        if i > 0 && (len(digits)-i)%3 == 0 { b.WriteByte(' ') }
        b.WriteRune(c)
    }
    return sign + b.String()
}

func stamp(unix int64) string {
    if unix <= 0 { return "unknown" }
    return strings.ToLower(time.Unix(unix, 0).UTC().Format("2 Jan 2006 15:04"))
}

// took renders a duration the way a person reads one off a long-running build.
func took(d time.Duration) string {
    switch {
    case d < time.Minute:
        return fmt.Sprintf("%.0f sec", d.Seconds())
    case d < time.Hour:
        return fmt.Sprintf("%d min %d sec", int(d.Minutes()), int(d.Seconds())%60)
    default:
        return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
    }
}

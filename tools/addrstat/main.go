// Command addrstat builds and queries the bot's per-address statistics from the
// command line. It drives the same addrstat package the bot does, against the
// same SQLite table and the same cursor, so statistics built here are ones the
// bot can serve and vice versa — point -db at the bot's own database to extend
// it, or at a file of its own to work on a copy.
//
// Usage:
//
//	addrstat build -db bitnsbot.sqlite -coreURL http://127.0.0.1:8332 -coreCookie ./cookie
//	addrstat build -memory -db bitnsbot.sqlite -coreURL http://127.0.0.1:8332 -coreCookie ./cookie
//	addrstat query -db bitnsbot.sqlite -coreURL http://127.0.0.1:8332 -coreCookie ./cookie <address>
//
// build catches the statistics up from their cursor to the chain tip and exits,
// where the bot's index catch-up goroutine keeps polling; query prints what the
// table holds for one address. query reads only the database — the node is asked
// for its tip, best effort, so the answer can say how far behind it stands.
//
// build -memory does the same work on several goroutines in an in-memory SQLite
// database and writes the table to -db once, at the end. See buildMemory for what
// that trades away.
package main

import "context"
import "database/sql"
import "errors"
import "flag"
import "fmt"
import "io"
import "os"
import "path/filepath"
import "runtime"
import "strconv"
import "strings"
import "sync"
import "sync/atomic"
import "time"
import _ "modernc.org/sqlite"
import "bitnsbot/addrindex"
import "bitnsbot/addrstat"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"
import "bitnsbot/progress"

// scanWorkers is how many goroutines a -memory build reads blocks on: every core
// but the two left for Bitcoin Core, and at least one. A var so tests can set it.
var scanWorkers = max(1, runtime.NumCPU()-2)

// blockTries is how many times a -memory build asks for a block before giving up,
// retryWait the rest between tries. A failed block loses the whole run, so a node
// busy or restarting for a moment should not cost one; the wait outlasts the two
// failed pings after which the core package redials. A var so tests shrink it.
const blockTries = 3
var retryWait = 5 * time.Second

// progressBlocks is how many blocks a -memory build finishes between progress lines.
const progressBlocks = 1000

// options are the flags both commands share, named as the bot's own are, so a
// line of the bot's config file reads the same here.
type options struct {
    db      string
    url     string
    user    string
    pass    string
    cookie  string
    verbose int
    memory  bool
}

func flags(fs *flag.FlagSet) *options {
    var o = &options{}
    fs.StringVar(&o.db, "db", "", "the SQLite database holding the addrstat table")
    fs.StringVar(&o.url, "coreURL", "http://127.0.0.1:8332", "Bitcoin Core JSON-RPC URL")
    fs.StringVar(&o.user, "coreUser", "", "Bitcoin Core RPC username (or use -coreCookie)")
    fs.StringVar(&o.pass, "corePass", "", "Bitcoin Core RPC password (or use -coreCookie)")
    fs.StringVar(&o.cookie, "coreCookie", "", "path to Bitcoin Core's .cookie file")
    fs.IntVar(&o.verbose, "verbose", 1, "log level: 0 quiet, 1 progress, 2 every request")
    fs.BoolVar(&o.memory, "memory", false, "build: work in an in-memory database, writing the table to -db when done")
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
    if opt.memory && cmd != "build" { logging.Fatal("-memory is for build, %s reads -db", cmd) }
    if opt.memory { os.Setenv("SQLITE_TMPDIR", filepath.Dir(opt.db)) }
    var handle, err = openDB(opt.db, cmd == "query")
    if err != nil { logging.Fatal("open %s: %v", opt.db, err) }
    defer handle.Close()
    switch cmd {
    case "build":
        var run = build
        if opt.memory { run = func(o *options, out io.Writer) error { return buildMemory(o, handle, out) } }
        if berr := run(opt, os.Stdout); berr != nil { logging.Fatal("build: %v", berr) }
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
func openDB(path string, existing bool) (*sql.DB, error) {
    if existing {
        if _, err := os.Stat(path); err != nil { return nil, err }
    }
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

// build catches the statistics up to the tip and exits, through the package's own
// chunked Update.
func build(opt *options, out io.Writer) error {
    return catchUp(opt, out, func(from, tip int64) error {
        var nop, hh = 0, from
        for hh < tip && nop < 3 {
            if uerr := addrstat.Update(tip); uerr != nil {
                fmt.Fprint(out, "Error updating index: ", uerr)
            }
            if h, ok := cursors.Get(cursors.AddrStat); ok {
                if hh == h + 1 { nop++ } else { nop = 0 }
                hh = h + 1
            }
        }
        return nil
    })
}

// catchUp hands collect the blocks between the cursor and the tip, and says what
// came of it. It asks the node for the tip itself, so an unreachable node is
// reported as that rather than as the first block failing.
func catchUp(opt *options, out io.Writer, collect func(from, tip int64) error) error {
    if err := core.Init(opt.url, opt.user, opt.pass, opt.cookie); err != nil { return err }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    var tip, err = core.GetBlockCount(ctx)
    cancel()
    if err != nil { return fmt.Errorf("Core RPC is unreachable at %s: %w", opt.url, err) }
    var from int64
    if h, ok := cursors.Get(cursors.AddrStat); ok { from = h + 1 }
    if from > tip {
        fmt.Fprintf(out, "Statistics are already at the tip (block %d), %d addresses hold coins\n", tip, addrstat.Count())
        return nil
    }
    fmt.Fprintf(out, "Collecting blocks %d..%d\n", from, tip)
    var started = time.Now()
    if cerr := collect(from, tip); cerr != nil { return cerr }
    fmt.Fprintf(out, "Collected %d blocks up to %d in %s, %d addresses hold coins\n",
        tip-from+1, tip, took(time.Since(started)), addrstat.Count())
    return nil
}

// buildMemory scans the blocks into an in-memory database, and writes the table
// and its cursor to the database at dest when it is done. Nothing touches dest
// until then, so a run that dies loses all of its work and leaves dest as it was.
// It starts from what dest already holds, as build does: starting empty would
// answer a run that stopped early by replacing a further-along table with a
// shorter one.
//
// Nothing is written when the cursor did not move, a dump of a chain-sized table
// being too much to do for nothing.
//
// dest is handed back to the packages afterwards, and addrstat.Init is what puts
// back the indexes dump left off.
func buildMemory(opt *options, dest *sql.DB, out io.Writer) error {
    var mem, err = openMemory(opt.db, out)
    if err != nil { return err }
    defer mem.Close()
    var before, had = cursors.Get(cursors.AddrStat)
    if berr := catchUp(opt, out, func(from, tip int64) error { return scan(mem, from, tip) }); berr != nil { return berr }
    var after, has = cursors.Get(cursors.AddrStat)
    if has && (!had || after != before) {
        if derr := dump(mem, opt.db, out); derr != nil { return derr }
    }
    if cerr := mem.Close(); cerr != nil { return cerr }
    if cerr := cursors.Init(dest); cerr != nil { return cerr }
    return addrstat.Init(dest)
}

// scan reads blocks from..tip on scanWorkers goroutines. Each takes the next
// height off a shared counter, reads the block, adds it up on its own and flushes
// that into the table, with nothing shared but the database's one connection,
// which is where the flushes queue.
//
// Blocks therefore reach the table out of order, so the cursor cannot follow them:
// it moves once, to the tip, after every block is in. A block that still fails
// after its retries stops the run with no cursor at all, and since the table may
// then hold blocks above the one that failed, buildMemory writes nothing.
//
// A balance is a sum, so neither the order nor the flushing can change it, nor
// which addresses end up with a row. What they can change is the history in a row
// whose balance touches zero, since the delete starts that address afresh: a block
// at a time sees zeros a thousand-block chunk sums past, and which of the blocks
// being read at once landed first decides some more. So recv, sent, flow, fees, txs
// and first may read lower than a plain build's, and differ from one run to the
// next. Over 120 mainnet blocks and 407 963 addresses, 1 701 rows differed from a
// plain build on one worker, and eight workers differed from one on another 151.
//
// The progress line counts blocks finished, so the range it names is approximate.
func scan(mem *sql.DB, from, tip int64) error {
    var ctx, cancel = context.WithCancel(context.Background())
    defer cancel()
    var next, done atomic.Int64
    next.Store(from)
    var prog = progress.New("addrstat", from, tip)
    var mu sync.Mutex
    var failure error
    var wg sync.WaitGroup
    for range scanWorkers {
        wg.Go(func() {
            for ctx.Err() == nil {
                var h = next.Add(1) - 1
                if h > tip { return }
                var blk, err = core.BlockAt(ctx, int(h))
                for try := 1; err != nil && try < blockTries && ctx.Err() == nil; try++ {
                    logging.Warn("addrstat: block %d, try %d of %d: %v", h, try, blockTries, err)
                    time.Sleep(retryWait)
                    blk, err = core.BlockAt(ctx, int(h))
                }
                if err == nil { err = flush(mem, deltasOf(blk)) }
                if err != nil {
                    mu.Lock()
                    if failure == nil { failure = fmt.Errorf("block %d: %w", h, err) }
                    mu.Unlock()
                    cancel()
                    return
                }
                var n = done.Add(1)
                if n % progressBlocks == 0 { prog.Chunk(from + n - progressBlocks, from + n - 1) }
            }
        })
    }
    wg.Wait()
    if failure != nil { return failure }
    var tx, err = mem.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    if cerr := cursors.Set(tx, cursors.AddrStat, tip); cerr != nil { return cerr }
    return tx.Commit()
}

// deltasOf adds up what one block moved for each address it touches. These are the
// rules addrstat's own apply follows, which the package keeps unexported: a
// transaction counts once per address however many of its outputs or inputs name
// it, its whole fee is charged to every address spending in it, and first and last
// are a min and a max since block times are not strictly ordered. A script paying
// no address is skipped. TestMemoryBuildWritesWhatADiskBuildWrites holds the two
// to the same answer.
func deltasOf(blk core.Block) map[string]*addrstat.Stat {
    var deltas = map[string]*addrstat.Stat{}
    var at = func(script []byte) (*addrstat.Stat, string) {
        var addr = addrindex.Address(script)
        if addr == "" { return nil, "" }
        var s = deltas[addr]
        if s == nil {
            var _, kind, _ = addrindex.Decode(addr)
            s = &addrstat.Stat{Type: kind}
            deltas[addr] = s
        }
        return s, addr
    }
    for _, tx := range blk.Txs {
        var fee int64
        if len(tx.Spent) > 0 {
            for _, p := range tx.Spent { fee += p.Sat }
            for _, p := range tx.Outputs { fee -= p.Sat }
        }
        var seen = map[string]bool{}
        var touch = func(s *addrstat.Stat, addr string) {
            if !seen[addr] {
                seen[addr] = true
                s.Txs++
            }
            if blk.Time <= 0 { return }
            if s.First == 0 || blk.Time < s.First { s.First = blk.Time }
            if blk.Time > s.Last { s.Last = blk.Time }
        }
        var paid = map[string]bool{}
        for _, p := range tx.Outputs {
            var s, addr = at(p.Script)
            if s == nil { continue }
            s.Recv += p.Sat
            touch(s, addr)
        }
        for _, p := range tx.Spent {
            var s, addr = at(p.Script)
            if s == nil { continue }
            s.Sent += p.Sat
            if fee > 0 && !paid[addr] {
                paid[addr] = true
                s.Fees += fee
            }
            touch(s, addr)
        }
    }
    return deltas
}

// flush adds one block's deltas to the table in one transaction: the upsert and
// the delete addrstat's own flush runs, without its cursor, which scan keeps. A row
// may go negative in passing, a spend landing before the block that paid it, and
// comes right when that block lands.
//
// The journal is off, so a transaction that failed partway cannot be rolled back.
// Nothing here can fail in memory short of running out of it, and a failure ends
// the run with nothing written.
func flush(mem *sql.DB, deltas map[string]*addrstat.Stat) error {
    var tx, err = mem.Begin()
    if err != nil { return err }
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
    if perr != nil { return perr }
    defer stmt.Close()
    var empty, eerr = tx.Prepare(`delete from addrstat where addr = ? and balance = 0`)
    if eerr != nil { return eerr }
    defer empty.Close()
    for addr, d := range deltas {
        if _, serr := stmt.Exec(addr, d.Type, d.Recv, d.Sent, d.Recv, d.Sent, d.Recv, d.Sent,
            d.Fees, d.Txs, d.First, d.Last); serr != nil {
            return serr
        }
        if _, derr := empty.Exec(addr); derr != nil { return derr }
    }
    return tx.Commit()
}

// openMemory opens the in-memory database with path attached as disk, and loads
// path's table and cursor into it. The packages are pointed at it, so everything
// unqualified from here on lands in memory.
//
// One connection, because every connection to :memory: is a database of its own;
// nothing here holds a transaction while asking the handle for something else, or
// that one connection would wait for itself. The rankings' indexes are dropped:
// nothing reads a ranking during a build, and each would cost a row's worth of
// memory and an update per address merged.
func openMemory(path string, out io.Writer) (*sql.DB, error) {
    var mem, err = sql.Open("sqlite", "file::memory:?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)&_pragma=busy_timeout(10000)")
    if err != nil { return nil, err }
    mem.SetMaxOpenConns(1)
    var load = func() error {
        if cerr := cursors.Init(mem); cerr != nil { return cerr }
        if aerr := addrstat.Init(mem); aerr != nil { return aerr }
        if _, aerr := mem.Exec("attach database ? as disk", path); aerr != nil { return aerr }
        var started = time.Now()
        var tx, terr = mem.Begin()
        if terr != nil { return terr }
        defer tx.Rollback()
        if derr := dropIndexes(tx, "main"); derr != nil { return derr }
        var res, ierr = tx.Exec("insert into main.addrstat select * from disk.addrstat")
        if ierr != nil { return ierr }
        var _, cerr = tx.Exec("insert into main.cursors select * from disk.cursors where name = ?", cursors.AddrStat)
        if cerr != nil { return cerr }
        var rows, rerr = res.RowsAffected()
        if rerr != nil { return rerr }
        fmt.Fprintf(out, "Loaded %d addresses from %s into memory in %s\n", rows, path, took(time.Since(started)))
        return tx.Commit()
    }
    if lerr := load(); lerr != nil {
        mem.Close()
        return nil, lerr
    }
    return mem, nil
}

// dump replaces the attached database's addrstat table with the one in memory and
// advances its cursor, in one transaction, so a reader of that file sees either
// the old table at the old height or the new one at the new. A row that left the
// table in memory has to leave it on disk too, which is why this is a replacement
// and not a merge. Other tables and other scans' cursors are left alone.
//
// The file's three indexes are dropped for the insert rather than kept up row by
// row, and rows go in in address order, so the primary key's index grows at its
// end. The caller recreates them through addrstat.Init, outside the transaction;
// a process dying between the two costs nothing, every Init creating what is
// missing. The transaction needs room for the whole table in the file's WAL, and
// holds SQLite's write lock throughout, so a bot on the same file cannot write.
func dump(mem *sql.DB, path string, out io.Writer) error {
    var started = time.Now()
    if _, err := mem.Exec("pragma disk.synchronous = normal"); err != nil { return err }
    var tx, err = mem.Begin()
    if err != nil { return err }
    defer tx.Rollback()
    if derr := dropIndexes(tx, "disk"); derr != nil { return derr }
    if _, derr := tx.Exec("delete from disk.addrstat"); derr != nil { return derr }
    var res, ierr = tx.Exec("insert into disk.addrstat select * from main.addrstat order by addr")
    if ierr != nil { return ierr }
    var _, cerr = tx.Exec(`insert into disk.cursors (name, place) select name, place from main.cursors where name = ?
        on conflict(name) do update set place = excluded.place`, cursors.AddrStat)
    if cerr != nil { return cerr }
    var rows, rerr = res.RowsAffected()
    if rerr != nil { return rerr }
    if terr := tx.Commit(); terr != nil { return terr }
    fmt.Fprintf(out, "Wrote %d addresses to %s in %s\n", rows, path, took(time.Since(started)))
    return nil
}

// dropIndexes drops every index an addrstat table in schema carries, read from
// the schema rather than named here so an index added to the package comes along.
// The primary key's has no sql and cannot be dropped.
func dropIndexes(tx *sql.Tx, schema string) error {
    var rows, err = tx.Query("select name from " + schema + ".sqlite_master where type = 'index' and tbl_name = 'addrstat' and sql is not null")
    if err != nil { return err }
    defer rows.Close()
    var names []string
    for rows.Next() {
        var name string
        if serr := rows.Scan(&name); serr != nil { return serr }
        names = append(names, name)
    }
    if rerr := rows.Err(); rerr != nil { return rerr }
    rows.Close()
    for _, name := range names {
        if _, derr := tx.Exec("drop index " + schema + "." + name); derr != nil { return derr }
    }
    return nil
}

// query prints the statistics held for an address, and reports false when there
// are none: the address holds nothing as of the cursor, or the scan has not been
// past it. The node is asked for its tip only to say how current the answer is,
// so a node that cannot be reached costs that line and nothing else.
func query(opt *options, address string, out io.Writer) (bool, error) {
    var at, built = cursors.Get(cursors.AddrStat)
    if !built { return false, errors.New("the statistics have never been built — run addrstat build first") }
    var stat, ok = addrstat.Get(address)
    if !ok {
        fmt.Fprintf(out, "No statistics for %s: it holds nothing as of block %d\n", address, at)
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

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
// build is the fast first build: it reads blocks in parallel from the cursor to
// five blocks below the tip and merges them straight into addrstat, then exits,
// leaving the bot's index catch-up goroutine to keep up from there one block at
// a time; query prints what the table holds for one address. query reads only the database — the node is asked
// for its tip, best effort, so the answer can say how far behind it stands.
package main

import "context"
import "database/sql"
import "errors"
import "flag"
import "fmt"
import "io"
import "os"
import "os/signal"
import "runtime"
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
    url     string
    user    string
    pass    string
    cookie  string
    verbose int
}

func flags(fs *flag.FlagSet) *options {
    var o = &options{}
    fs.StringVar(&o.db, "db", "", "the SQLite database holding the addrstat table")
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
        if berr := build(opt, os.Stdout); berr != nil { logging.Fatal("build: %v", berr) }
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

// behind is how far below the tip a build stops, so the bot's own Update — which
// stages the newest blocks where a reorg can take them out — carries on from
// there. A merge goes straight into addrstat and cannot be undone.
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

// progressEvery is how many merged blocks a progress line covers.
const progressEvery = 1000

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

// build pre-builds the statistics from their cursor to the tip less `behind`,
// straight into the addrstat table, and exits. The bot's addrstat.Update is the
// keep-up path — one block at a time, staged — and this is the fast first build:
// `workers` goroutines read blocks and work out their movements, which is where
// the time goes (a getblock at verbosity 3 and an address encoded per script),
// and one goroutine merges them into addrstat with addrstat.Merge, each block
// with the cursor in one transaction.
//
// The merge takes the blocks in height order — a slot per height, queued as the
// heights are handed out, read back in that order — so the cursor always says
// exactly which blocks are counted, and a build stopped partway, by Ctrl+C or a
// failure, resumes from it without counting a block twice.
//
// A block that still cannot be read after `attempts` tries is reported and
// skipped: its height is printed and the cursor steps over it, so what it moved
// is missing from the statistics. The heights skipped are listed at the end.
func build(opt *options, out io.Writer) error {
    if err := core.Init(opt.url, opt.user, opt.pass, opt.cookie); err != nil { return err }
    var tctx, tcancel = context.WithTimeout(context.Background(), 30*time.Second)
    var tip, err = core.GetBlockCount(tctx)
    tcancel()
    if err != nil { return fmt.Errorf("Core RPC is unreachable at %s: %w", opt.url, err) }
    var from int64
    if h, ok := cursors.Get(cursors.AddrStat); ok { from = h + 1 }
    var to = tip - behind
    if from > to {
        fmt.Fprintf(out, "Statistics are already within %d blocks of the tip (block %d), %d addresses\n", behind, tip, addrstat.Count())
        return nil
    }
    fmt.Fprintf(out, "Collecting blocks %d..%d on %d workers\n", from, to, workers)
    var ctx, stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer stop()
    var started = time.Now()
    var skipped, cerr = collect(ctx, from, to, out)
    if len(skipped) > 0 { fmt.Fprintf(out, "Skipped %d blocks: %v\n", len(skipped), skipped) }
    if cerr != nil { return cerr }
    fmt.Fprintf(out, "Collected %d blocks up to %d in %s, %d addresses\n",
        to-from+1, to, took(time.Since(started)), addrstat.Count())
    return nil
}

// collect reads from..to on the workers and merges each block in height order,
// returning the heights it skipped. A failed merge or a cancelled context stops
// it, the reads still queued are drained, and the cursor stands at the last block
// merged.
func collect(parent context.Context, from, to int64, out io.Writer) ([]int64, error) {
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
        if merr := addrstat.Merge(h, f.deltas); merr != nil {
            failure = fmt.Errorf("merge block %d: %w", h, merr)
            cancel()
            continue
        }
        if h-chunk+1 >= progressEvery || h == to {
            prog.Chunk(chunk, h)
            chunk = h + 1
        }
        h++
    }
    wg.Wait()
    return skipped, failure
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

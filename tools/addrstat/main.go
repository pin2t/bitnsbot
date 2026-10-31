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
// build catches the statistics up from their cursor to the chain tip and exits,
// where the bot's index catch-up goroutine keeps polling; query prints what the
// table holds for one address. query reads only the database — the node is asked
// for its tip, best effort, so the answer can say how far behind it stands.
package main

import "context"
import "database/sql"
import "errors"
import "flag"
import "fmt"
import "io"
import "os"
import "strconv"
import "strings"
import "time"
import _ "modernc.org/sqlite"
import "bitnsbot/addrstat"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"

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

// build catches the statistics up to the tip and exits. It asks the node for the
// tip itself, so an unreachable node is reported as that rather than as the
// first block failing.
func build(opt *options, out io.Writer) error {
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
    var started, nop, hh = time.Now(), 0, from
    for hh < tip && nop < 3 {
        if uerr := addrstat.Update(tip); uerr != nil {
            fmt.Fprint(out, "Error updating index: ", uerr)
        }
        if h, ok := cursors.Get(cursors.AddrStat); ok {
            if hh == h + 1 { nop++ } else { nop = 0 }
            hh = h + 1
        }
    }
    fmt.Fprintf(out, "Collected %d blocks up to %d in %s, %d addresses hold coins\n",
        tip-from+1, tip, took(time.Since(started)), addrstat.Count())
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

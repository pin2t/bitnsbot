// Command addrindex builds and queries the bot's address index from the command
// line. It drives the same addrindex package the bot does, against the same
// bbolt buckets and the same cursor, so an index built here is one the bot can
// serve and vice versa — point -db at the bot's own database to extend it, or at
// a file of its own to work on a copy.
//
// Usage:
//
//	addrindex build    -db ai.db -url http://127.0.0.1:8332 -cookie ./cookie
//	addrindex list     -db ai.db -url http://127.0.0.1:8332 -cookie ./cookie <address>
//	addrindex actbuild -db ai.db -url http://127.0.0.1:8332 -cookie ./cookie
//
// build catches the index up from its cursor to the chain tip and exits; list
// prints every transaction the index holds for an address, then a summary;
// actbuild walks the chain again and records the addresses whose history is
// longer than -active transactions.
package main

import "context"
import "flag"
import "fmt"
import "os"
import "time"

import "go.etcd.io/bbolt"
import "bitnsbot/addrindex"
import "bitnsbot/logging"

// options are the flags every command shares. Core's REST interface (which the
// build reads) is served on the same host:port as JSON-RPC (which the lookups
// use), so one -url covers both; only the RPC half needs credentials.
type options struct {
    db      string
    url     string
    cookie  string
    user    string
    pass    string
    limit   int
    active  int
    verbose int
}

func flags(fs *flag.FlagSet) *options {
    var o = &options{}
    fs.StringVar(&o.db, "db", "addrindex.db", "path to the bbolt database holding the index")
    fs.StringVar(&o.url, "url", "http://127.0.0.1:8332", "Bitcoin Core base URL, serving both JSON-RPC and REST")
    fs.StringVar(&o.cookie, "cookie", "", "path to Core's .cookie file, for RPC auth")
    fs.StringVar(&o.user, "user", "", "Core RPC username, instead of a cookie")
    fs.StringVar(&o.pass, "pass", "", "Core RPC password, instead of a cookie")
    fs.IntVar(&o.limit, "limit", 1000000, "most touches to read for one address")
    fs.IntVar(&o.active, "active", 1000, "actbuild: transactions an address needs to count as active")
    fs.IntVar(&o.verbose, "verbose", 1, "log level: 0 quiet, 1 progress, 2 every request")
    return o
}

func usage() {
    fmt.Fprintln(os.Stderr, "usage: addrindex <command> [flags] [address]")
    fmt.Fprintln(os.Stderr, "")
    fmt.Fprintln(os.Stderr, "commands:")
    fmt.Fprintln(os.Stderr, "  build     catch the index up from its cursor to the chain tip")
    fmt.Fprintln(os.Stderr, "  list      print every transaction the index holds for an address")
    fmt.Fprintln(os.Stderr, "  actbuild  record the addresses with more than -active transactions")
}

func main() {
    if len(os.Args) < 2 {
        usage()
        os.Exit(2)
    }
    var cmd = os.Args[1]
    if cmd != "build" && cmd != "list" && cmd != "actbuild" {
        fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
        usage()
        os.Exit(2)
    }
    var fs = flag.NewFlagSet(cmd, flag.ExitOnError)
    var opt = flags(fs)
    fs.Parse(os.Args[2:])
    logging.SetVerbose(opt.verbose)

    var err error
    db, err = bbolt.Open(opt.db, 0600, &bbolt.Options{Timeout: 5 * time.Second})
    if err != nil { logging.Fatal("open %s: %v", opt.db, err) }
    defer db.Close()
    // the same buckets the bot's openDB creates, so either can carry on from the
    // other's cursor
    if err := addrindex.Init(db); err != nil { logging.Fatal("init index: %v", err) }

    switch cmd {
    case "build":
        build(opt)
    case "list":
        if fs.NArg() != 1 {
            fmt.Fprintln(os.Stderr, "list needs exactly one address")
            os.Exit(2)
        }
        list(opt, fs.Arg(0))
    case "actbuild":
        activeMin = opt.active
        actbuild(opt)
    }
}

// db is the open index, shared by the commands.
var db *bbolt.DB

// build catches the index up to the tip and exits, where the bot's StartBackfill
// keeps polling. Both call addrindex.Build, so both chunk and advance the cursor
// identically and either can resume what the other started.
func build(opt *options) {
    var src = addrindex.NewREST(opt.url)
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    var tip, err = src.Tip(ctx)
    cancel()
    if err != nil {
        logging.Fatal("Core REST is unreachable at %s (%v) — enable -rest=1", opt.url, err)
    }
    var from = 0
    if h, ok := addrindex.Cursor(); ok { from = h + 1 }
    if from > tip {
        fmt.Printf("Index is already at the tip (block %d)\n", tip)
        return
    }
    fmt.Printf("Building blocks %d..%d\n", from, tip)
    var started = time.Now()
    if err := addrindex.Build(src); err != nil {
        logging.Fatal("build: %v", err)
    }
    var at, _ = addrindex.Cursor()
    fmt.Printf("Built %d blocks up to %d in %s\n", at-from+1, at, took(time.Since(started)))
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

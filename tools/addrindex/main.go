// Command addrindex builds and queries the bot's address index from the command
// line. It drives the same addrindex package the bot does, against the same
// SQLite tables and the same cursor, so an index built here is one the bot can
// serve and vice versa — point -db at the bot's own database to extend it, or at
// a file of its own to work on a copy.
//
// Usage:
//
//	addrindex build     -db ai.db -url http://127.0.0.1:8332 -cookie ./cookie
//	addrindex list      -db ai.db -url http://127.0.0.1:8332 -cookie ./cookie <address>
//	addrindex actbuild  -db ai.db -blocks ~/.bitcoin/blocks
//	addrindex richbuild -db rich.db -url http://127.0.0.1:8332
//	addrindex ababuild  -db abandoned.db -url http://127.0.0.1:8332
//
// build catches the index up from its cursor to the chain tip and exits; list
// prints every transaction the index holds for an address, then a summary;
// actbuild reads Core's raw block files and records the addresses whose history
// is longer than -active transactions in a table named active. It talks to no
// node at all — it reads the files and encodes the addresses itself — so it
// needs neither -url nor -cookie; richbuild reads the whole chain over RPC and
// writes what every address holds now to a table named rich, keeping no index
// at all; ababuild reads it the same way and writes the addresses that still
// hold coins but whose coins have gone longest without moving, to a table named
// abandoned.
package main

import "context"
import "database/sql"
import "flag"
import "fmt"
import "os"
import "time"
import _ "modernc.org/sqlite"
import "bitnsbot/addrindex"
import "bitnsbot/core"
import "bitnsbot/cursors"
import "bitnsbot/logging"

// options are the flags every command shares. Every command that talks to a
// node does it over JSON-RPC, so one -url and one set of credentials cover the
// builds and the lookups alike.
type options struct {
    db      string
    url     string
    cookie  string
    user    string
    pass    string
    limit   int
    active  int
    blocks  string
    addrs   int
    batch   int
    shards  int
    fetch   int
    min     int64
    to      int
    tmp     string
    top     int
    sum     int
    verbose int
}

func flags(fs *flag.FlagSet) *options {
    var o = &options{}
    fs.StringVar(&o.db, "db", "", "the SQLite database: the index for build, list and actbuild, the tables richbuild and ababuild write")
    fs.StringVar(&o.url, "url", "http://127.0.0.1:8332", "Bitcoin Core JSON-RPC URL")
    fs.StringVar(&o.cookie, "cookie", "", "path to Core's .cookie file, for RPC auth")
    fs.StringVar(&o.user, "user", "", "Core RPC username, instead of a cookie")
    fs.StringVar(&o.pass, "pass", "", "Core RPC password, instead of a cookie")
    fs.IntVar(&o.limit, "limit", 5000000, "list: most touches to read for one address")
    fs.IntVar(&o.active, "active", 1000, "actbuild: transactions an address needs to count as active")
    fs.StringVar(&o.blocks, "blocks", "", "actbuild: Core's blocks directory, read instead of its RPC")
    fs.IntVar(&o.addrs, "addrs", 0, "actbuild: distinct addresses to reserve room for, so the set never reallocates")
    fs.IntVar(&o.batch, "batch", 4000000, "richbuild, ababuild: movements held in memory before they are written to the shards")
    fs.IntVar(&o.shards, "shards", 128, "richbuild, ababuild: files the movements are split into")
    fs.IntVar(&o.fetch, "fetch", 4, "richbuild, ababuild: blocks fetched at once")
    fs.Int64Var(&o.min, "min", 0, "richbuild, ababuild: satoshi an address needs before it is written to the answer table")
    fs.IntVar(&o.to, "to", 0, "richbuild, ababuild: stop at this height instead of the chain tip")
    fs.StringVar(&o.tmp, "tmp", "", "richbuild, ababuild: directory for the movement shards (default the database's name with .shards)")
    fs.IntVar(&o.top, "top", 10000, "ababuild: addresses to keep in the abandoned table")
    fs.IntVar(&o.sum, "sum", 4, "ababuild: shards summed at once, which is also what multiplies the run's peak memory")
    fs.IntVar(&o.verbose, "verbose", 1, "log level: 0 quiet, 1 progress, 2 every request")
    return o
}

func usage() {
    fmt.Fprintln(os.Stderr, "usage: addrindex <command> -db <database> [flags] [address]")
    fmt.Fprintln(os.Stderr, "")
    fmt.Fprintln(os.Stderr, "commands:")
    fmt.Fprintln(os.Stderr, "  build     catch the index up from its cursor to the chain tip")
    fmt.Fprintln(os.Stderr, "  list      print every transaction the index holds for an address")
    fmt.Fprintln(os.Stderr, "  actbuild  record the addresses with more than -active transactions, from -blocks")
    fmt.Fprintln(os.Stderr, "  richbuild sum every address's balance over the whole chain into -db")
    fmt.Fprintln(os.Stderr, "  ababuild  rank the addresses that hold coins by how long since their coins last moved")
}

// build, list and actbuild open -db as the index; richbuild and ababuild open it
// as their own store instead, which holds no index, so opening it as one would
// create an empty index beside the tables actually being worked on. list only
// reads, so a -db that does not exist is a mistyped path rather than an index to
// create.
func main() {
    if len(os.Args) < 2 {
        usage()
        os.Exit(2)
    }
    var cmd = os.Args[1]
    if cmd != "build" && cmd != "list" && cmd != "actbuild" && cmd != "richbuild" && cmd != "ababuild" {
        fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
        usage()
        os.Exit(2)
    }
    var fs = flag.NewFlagSet(cmd, flag.ExitOnError)
    var opt = flags(fs)
    fs.Parse(os.Args[2:])
    logging.SetVerbose(opt.verbose)
    if cmd != "richbuild" && cmd != "ababuild" {
        if opt.db == "" { logging.Fatal("%s needs -db naming the SQLite database that holds the index", cmd) }
        if err := openIndex(opt.db, cmd == "list"); err != nil { logging.Fatal("open %s: %v", opt.db, err) }
        defer db.Close()
    }
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
    case "richbuild":
        if err := richbuild(opt); err != nil { logging.Fatal("richbuild: %v", err) }
    case "ababuild":
        if err := ababuild(opt); err != nil { logging.Fatal("ababuild: %v", err) }
    }
}

// db is the open index, shared by the commands.
var db *sql.DB

// indexDDL is the index's two tables exactly as the bot's openDB creates them, so
// -db can be the bot's own database and either side carries on from the other's
// cursor.
var indexDDL = []string{
    `create table if not exists addrindex (shard INTEGER PRIMARY KEY, data BLOB NOT NULL)`,
    `create table if not exists cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)`,
}

// openIndex opens the database with the pragmas the bot opens its own with —
// WAL, so the index can be read while a build writes it, and a busy timeout, so
// a bot sharing the file makes this wait rather than fail — creates the index's
// tables when they are missing, and hands the handle to the packages that read
// and write them.
//
// existing is list's: SQLite would otherwise create the file and answer with an
// empty index, which reads like a build that never ran rather than a mistyped
// path.
func openIndex(path string, existing bool) error {
    if existing {
        if _, err := os.Stat(path); err != nil { return err }
    }
    var opened, err = sql.Open("sqlite", "file:"+path+
        "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)")
    if err != nil { return err }
    for _, ddl := range indexDDL {
        if _, err := opened.Exec(ddl); err != nil {
            opened.Close()
            return err
        }
    }
    db = opened
    if err := cursors.Init(db); err != nil { return err }
    return addrindex.Init(db)
}

// build catches the index up to the tip and exits, where the bot's one index
// catch-up goroutine keeps polling. Both drive the same addrindex package
// against the same cursor, so both chunk and advance it identically and either
// can resume what the other started.
func build(opt *options) {
    if err := core.Init(opt.url, opt.user, opt.pass, opt.cookie); err != nil { logging.Fatal("RPC client: %v", err) }
    var ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
    var count, err = core.GetBlockCount(ctx)
    cancel()
    if err != nil {
        logging.Fatal("Core RPC is unreachable at %s (%v)", opt.url, err)
    }
    var tip = int(count)
    var from = 0
    if h, ok := addrindex.Cursor(); ok { from = h + 1 }
    if from > tip {
        fmt.Printf("Index is already at the tip (block %d)\n", tip)
        return
    }
    fmt.Printf("Building blocks %d..%d\n", from, tip)
    var started = time.Now()
    if err := addrindex.Build(); err != nil {
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

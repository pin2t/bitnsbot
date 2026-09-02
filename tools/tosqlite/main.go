// Command tosqlite migrates the bot's bbolt database to SQLite:
//
//	tosqlite -src bitnsbot.db -dst bitnsbot.sqlite.db
//
// Seven buckets become six tables: addrindex, blocks-stat, market, rates and
// watches map one to one, and the three miner buckets (miners, miners-tag,
// miners-stat) combine into one. The source is opened read-only and never
// written; the destination must not already exist.
//
// The SQLite driver is modernc.org/sqlite (gitlab.com/cznic/sqlite), a pure-Go
// translation of SQLite with no cgo, so this still cross-compiles like the rest
// of the repo.
package main

import "database/sql"
import "errors"
import "flag"
import "fmt"
import "os"
import "time"

import "go.etcd.io/bbolt"
import _ "modernc.org/sqlite"
import "bitnsbot/logging"

var src = flag.String("src", "", "path to the bbolt database to read (required)")
var dst = flag.String("dst", "", "path to the SQLite database to create (required; must not already exist)")
var verbose = flag.Int("verbose", 0, "log level: 0 status, 1 detail, 2 per-record")
var batch = flag.Int("batch", 10000, "rows per SQLite transaction")

// pragmas prepare a fresh destination for a bulk load, and their order matters:
// page_size is fixed when the file header is written, so it has to be set before
// anything creates a page — a later one is silently a no-op. 16K measured 9%
// faster than the 4K default and 11% smaller (1265 MB against 1425 MB on the same
// million-row source), because a packed shard-range value averages about a
// kilobyte and four times as many of them fit per page. Raising cache_size on top
// was measured too and is not here: it was slower at every size tried, 64 MB by
// 0.5% and 256 MB by 1.3%, sequential rowid inserts having nothing to re-read.
// journal_mode=OFF rather than WAL, and synchronous=OFF, because this run builds
// the whole file — a crash means running it again, not recovering.
var pragmas = []string{
    "PRAGMA page_size=16384",
    "PRAGMA journal_mode=OFF",
    "PRAGMA synchronous=OFF",
}

// schema is the destination layout. It corrects the DDL as written in two places,
// both forced by SQLite: a table may declare only one PRIMARY KEY clause, so
// miners and watches — which each named several — take composite keys, and rates
// keys on its timestamp alone; and `RIMARY KEY` does not fail, it parses as part
// of the column's *type*, which would have left blocks with no primary key at all.
var schema = []string{
    `create table addrindex (shard INTEGER PRIMARY KEY, data BLOB NOT NULL)`,
    `create table blocks (height INTEGER PRIMARY KEY, hash TEXT NOT NULL, ts INTEGER NOT NULL,
        size INTEGER NOT NULL, txs INTEGER NOT NULL, miner TEXT NOT NULL, feesOK INTEGER NOT NULL,
        minFee INTEGER NOT NULL, avgFee INTEGER NOT NULL, maxFee INTEGER NOT NULL,
        txSizeMin INTEGER NOT NULL, txSizeAvg INTEGER NOT NULL, txSizeMax INTEGER NOT NULL,
        reward INTEGER NOT NULL, fees INTEGER NOT NULL, difficulty FLOAT NOT NULL)`,
    `create table market (ts INTEGER PRIMARY KEY, price INTEGER NOT NULL, cap INTEGER NOT NULL,
        volume24h INTEGER NOT NULL)`,
    `create table miners (name TEXT NOT NULL, address TEXT NOT NULL, tag TEXT NOT NULL,
        blocks INTEGER NOT NULL, reward INTEGER NOT NULL, fees INTEGER NOT NULL,
        totalWork FLOAT NOT NULL, lastWork FLOAT NOT NULL, PRIMARY KEY (name, address, tag))`,
    `create table rates (ts INTEGER PRIMARY KEY, cents INTEGER NOT NULL)`,
    `create table watches (chat INTEGER NOT NULL, addr TEXT NOT NULL, alias TEXT NOT NULL,
        created INTEGER NOT NULL, PRIMARY KEY (chat, addr))`,
}

// tables run smallest first so a mistake surfaces in the first second rather than
// after the address index, which is tens of millions of rows and hours long.
var tables = []struct {
    name string
    copy func(*bbolt.DB, *sql.DB) (int, int, error)
}{
    {"blocks", copyBlocks},
    {"market", copyMarket},
    {"miners", copyMiners},
    {"rates", copyRates},
    {"watches", copyWatches},
    {"addrindex", copyAddrindex},
}

func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
        flag.PrintDefaults()
    }
    flag.Parse()
    logging.SetVerbose(*verbose)
    if *src == "" { logging.Fatal("-src is required") }
    if *dst == "" { logging.Fatal("-dst is required") }
    if _, err := os.Stat(*dst); err == nil {
        logging.Fatal("%s already exists — remove it first", *dst)
    }
    // read-only so a mistake here cannot damage the bot's database. bbolt holds an
    // exclusive lock while a writer has the file open, so this fails outright when
    // the bot is running rather than reading a torn file; the timeout turns that
    // into an error instead of a wait that never ends.
    var source, err = bbolt.Open(*src, 0600, &bbolt.Options{ReadOnly: true, Timeout: 5 * time.Second})
    if err != nil {
        if errors.Is(err, bbolt.ErrTimeout) {
            logging.Fatal("%s is locked by another process — stop the bot, or migrate a -backup copy", *src)
        }
        logging.Fatal("open %s: %v", *src, err)
    }
    defer source.Close()
    target, err := sql.Open("sqlite", *dst)
    if err != nil { logging.Fatal("open %s: %v", *dst, err) }
    // a pragma applies to the connection that ran it, so one connection is what
    // makes the whole set hold for every statement after it
    target.SetMaxOpenConns(1)
    var began = time.Now()
    for _, s := range append(append([]string{}, pragmas...), schema...) {
        if _, err := target.Exec(s); err != nil { abort(target, "%v", err) }
    }
    for _, t := range tables {
        var rows, skipped, err = t.copy(source, target)
        if err != nil { abort(target, "%s: %v", t.name, err) }
        if skipped > 0 {
            logging.Warn("%s: %d rows, %d unreadable records skipped", t.name, rows, skipped)
        } else {
            logging.Status("%s: %d rows", t.name, rows)
        }
    }
    if err := target.Close(); err != nil { logging.Fatal("close %s: %v", *dst, err) }
    logging.Status("migrated %s to %s in %s", *src, *dst, time.Since(began).Round(time.Millisecond))
}

// abort removes the half-written destination before exiting. This run created it
// — main refuses to start otherwise — so nothing else can be lost, and a partial
// migration that looks complete is worse than no file at all.
func abort(target *sql.DB, format string, args ...any) {
    target.Close()
    os.Remove(*dst)
    logging.Fatal(format, args...)
}

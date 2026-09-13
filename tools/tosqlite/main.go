// Command tosqlite migrates the bot's bbolt database to SQLite, and checks a
// migration it has made:
//
//	tosqlite          -src bitnsbot.db -dst bitnsbot.sqlite.db
//	tosqlite validate -src bitnsbot.db -dst bitnsbot.sqlite.db
//
// validate walks the source again through the same mappings and compares every
// row the destination holds against the record it came from, reporting what is
// missing, what differs and what the table has that the bucket does not. It exits
// non-zero on any difference, and writes nothing.
//
// Eight buckets become eight tables: addrindex, addrstat, blocks, cursors, market,
// rates and watches map one to one, and miners — a record per pool, carrying its
// aggregate and the addresses and tags it is recognised by — is unzipped into
// rows. The source is opened read-only and never written; a migration's
// destination must not already exist.
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
import "strings"
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
        reward INTEGER NOT NULL, fees INTEGER NOT NULL, difficulty REAL NOT NULL)`,
    `create table market (ts INTEGER PRIMARY KEY, price INTEGER NOT NULL, cap INTEGER NOT NULL,
        volume24h INTEGER NOT NULL)`,
    `create table miners (name TEXT NOT NULL, address TEXT NOT NULL, tag TEXT NOT NULL,
        blocks INTEGER NOT NULL, reward INTEGER NOT NULL, fees INTEGER NOT NULL,
        totalWork REAL NOT NULL, lastWork REAL NOT NULL, PRIMARY KEY (name, address, tag))`,
    `create table rates (ts INTEGER PRIMARY KEY, cents INTEGER NOT NULL)`,
    `create table watches (chat INTEGER NOT NULL, addr TEXT NOT NULL, alias TEXT NOT NULL,
        created INTEGER NOT NULL, PRIMARY KEY (chat, addr))`,
    `create table addrstat (addr TEXT PRIMARY KEY, type TEXT NOT NULL, balance INTEGER NOT NULL,
        recv INTEGER NOT NULL, sent INTEGER NOT NULL, flow INTEGER NOT NULL, fees INTEGER NOT NULL,
        txs INTEGER NOT NULL, first INTEGER NOT NULL, last INTEGER NOT NULL)`,
    `create table cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)`,
}

// The indexes the three address rankings are read through, created **after the
// rows are loaded**: an index maintained across a bulk insert is three B-trees
// rebalanced per row, where building it once at the end sorts the column and
// writes it in order. Each serves an ORDER BY in either direction, so one index
// per column is enough for "largest balance first" and "oldest date first" alike.
//
// The cursors table gets none: it holds one row per scan, five of them.
var indexes = []string{
    `create index addrstat_balance on addrstat (balance)`,
    `create index addrstat_txs on addrstat (txs)`,
    `create index addrstat_last on addrstat (last)`,
}

// tables run smallest first so a mistake surfaces in the first second rather than
// after the address index, which is tens of millions of rows and hours long.
//
// cols is the order the copy emits its values in, and the first keys of them are
// the primary key — which is what lets validate find the row a bucket record
// should have become. replace is for the one table whose source can name the same
// key twice (see copyWatches).
//
// check is whether validate compares the table. cursors is the one that is
// migrated but not compared: a cursor is a scan's *place*, and the bot moves it —
// so a validate run against a database that has been used since the migration
// would report a difference that is not an error. The three ranked-address index
// buckets are not here at all, having no table: the bot rebuilds them from the
// addrstat records, so there is nothing in them a migration could lose.
var tables = []struct {
    name    string
    cols    []string
    keys    int
    replace bool
    check   bool
    copy    func(*bbolt.DB, sink) (int, int, error)
}{
    {"cursors", []string{"name", "place"}, 1, false, false, copyCursors},
    {"blocks", []string{"height", "hash", "ts", "size", "txs", "miner", "feesOK", "minFee", "avgFee",
        "maxFee", "txSizeMin", "txSizeAvg", "txSizeMax", "reward", "fees", "difficulty"}, 1, false, true, copyBlocks},
    {"market", []string{"ts", "price", "cap", "volume24h"}, 1, false, true, copyMarket},
    {"miners", []string{"name", "address", "tag", "blocks", "reward", "fees", "totalWork", "lastWork"}, 3, false, true, copyMiners},
    {"rates", []string{"ts", "cents"}, 1, false, true, copyRates},
    {"watches", []string{"chat", "addr", "alias", "created"}, 2, true, true, copyWatches},
    {"addrstat", []string{"addr", "type", "balance", "recv", "sent", "flow", "fees", "txs", "first", "last"}, 1, false, true, copyAddrstat},
    {"addrindex", []string{"shard", "data"}, 1, false, true, copyAddrindex},
}

// insertInto is the statement a migration writes a table's rows with, built from
// the column list rather than written out beside it — one place names the columns,
// so their order cannot drift from the order the copy emits them in.
func insertInto(name string, cols []string, replace bool) string {
    var verb = "insert into "
    if replace { verb = "insert or replace into " }
    var marks = make([]string, len(cols))
    for i := range marks { marks[i] = "?" }
    return verb + name + " (" + strings.Join(cols, ", ") + ") values (" + strings.Join(marks, ", ") + ")"
}

func main() {
    flag.Usage = func() {
        fmt.Fprintf(flag.CommandLine.Output(), "Usage:\n  %s [-src db -dst db.sqlite]\n  %s validate -src db -dst db.sqlite\n\n", os.Args[0], os.Args[0])
        flag.PrintDefaults()
    }
    // The migration is what this tool does, so it is the bare form and stays the
    // way it has always been invoked; validate is the one command that has to be
    // named. Its flags come after it, as they do for tools/addrindex.
    var args = os.Args[1:]
    var checking bool
    if len(args) > 0 && args[0] == "validate" {
        checking, args = true, args[1:]
    }
    flag.CommandLine.Parse(args)
    logging.SetVerbose(*verbose)
    if *src == "" { logging.Fatal("-src is required") }
    if *dst == "" { logging.Fatal("-dst is required") }
    var _, dstErr = os.Stat(*dst)
    if checking && dstErr != nil {
        logging.Fatal("%s does not exist — there is nothing to validate", *dst)
    }
    if !checking && dstErr == nil {
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
    if checking {
        defer target.Close()
        // an exit code, so a script can gate on it
        if !validate(source, target) { os.Exit(1) }
        return
    }
    // a pragma applies to the connection that ran it, so one connection is what
    // makes the whole set hold for every statement after it
    target.SetMaxOpenConns(1)
    var began = time.Now()
    for _, s := range append(append([]string{}, pragmas...), schema...) {
        if _, err := target.Exec(s); err != nil { abort(target, "%v", err) }
    }
    for _, t := range tables {
        var rows, skipped, err = t.copy(source, newWriter(target, t.name, insertInto(t.name, t.cols, t.replace)))
        if err != nil { abort(target, "%s: %v", t.name, err) }
        if skipped > 0 {
            logging.Warn("%s: %d rows, %d unreadable records skipped", t.name, rows, skipped)
        } else {
            logging.Status("%s: %d rows", t.name, rows)
        }
    }
    for _, s := range indexes {
        var began = time.Now()
        if _, err := target.Exec(s); err != nil { abort(target, "%v", err) }
        logging.Info("%s in %s", s, time.Since(began).Round(time.Millisecond))
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

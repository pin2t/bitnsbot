package main

import "bytes"
import "log"
import "os"
import "path/filepath"
import "strings"
import "testing"
import "bitnsbot/logging"

// Every package that owns tables is Init'd by openDB, and every table it stores
// into is created there. This is pinned because forgetting one fails *silently*:
// those packages guard on a nil handle, so the bot runs, logs progress, and
// stores nothing. The address index shipped in exactly that state.
//
// the block-info cache
//
// rates
//
// watches
//
// miners
//
// per-address statistics
//
// every scan's place
//
// the address index's touches
//
// and the three indexes the address rankings are read through
//
// and the one the miner chart reads a period of blocks through, which the
// planner has to actually use — without it the query is a scan of the chain
func TestOpenDBTables(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "bitnsbot.sqlite")
    if err := openDB(path); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var want = []string{
        "blocks",
        "rates", "market",
        "watches",
        "miners", "mineraddr", "minertag",
        "addrstat",
        "cursors",
        "addrindex",
        "addrbal",
    }
    var got = map[string]bool{}
    var rows, err = db.Query("select name from sqlite_master where type = 'table'")
    if err != nil { t.Fatal(err) }
    defer rows.Close()
    for rows.Next() {
        var name string
        if err := rows.Scan(&name); err != nil { t.Fatal(err) }
        got[name] = true
    }
    for _, name := range want {
        if !got[name] {
            t.Errorf("table %q was never created — is its package missing an Init in openDB?", name)
        }
    }
    if len(got) != len(want) {
        t.Errorf("table count = %d, want %d: %v", len(got), len(want), got)
    }
    for _, col := range []string{"balance", "txs", "last"} {
        var n int
        if err := db.QueryRow(`select count(*) from sqlite_master where type = 'index'
            and tbl_name = 'addrstat' and sql like ?`, "%("+col+")%").Scan(&n); err != nil {
            t.Fatal(err)
        }
        if n != 1 { t.Errorf("addrstat has %d indexes on %s, want 1", n, col) }
    }
    var plan string
    if err := db.QueryRow("explain query plan select ts, miner, difficulty from blocks where ts >= ?", 0).Scan(
        new(int), new(int), new(int), &plan); err != nil {
        t.Fatal(err)
    }
    if !strings.Contains(plan, "blocks_ts") {
        t.Errorf("the miner chart's query does not use blocks_ts: %s", plan)
    }
}

// Every start runs the schema, so it has to be safe to repeat against a database
// a migration — or an earlier start — already built.
func TestOpenDBIsRepeatable(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "bitnsbot.sqlite")
    for i := 0; i < 3; i++ {
        if err := openDB(path); err != nil { t.Fatalf("openDB %d: %v", i, err) }
        if err := closeDB(); err != nil { t.Fatalf("closeDB %d: %v", i, err) }
    }
}

// Every statement run on the bot's database is logged at DB level — straight
// through the handle, in a transaction, and each time a prepared statement runs
// — on one line and cut at 150 characters like any DB message. Below that
// verbosity nothing is.
func TestOpenDBLogsQueries(t *testing.T) {
    var buf bytes.Buffer
    log.SetOutput(&buf)
    defer log.SetOutput(os.Stderr)
    log.SetFlags(0)
    defer log.SetFlags(log.LstdFlags)
    logging.SetVerbose(2)
    defer logging.SetVerbose(0)
    if err := openDB(filepath.Join(t.TempDir(), "bitnsbot.sqlite")); err != nil { t.Fatal(err) }
    defer closeDB()
    if !strings.Contains(buf.String(), "[DB] create table if not exists blocks (height INTEGER PRIMARY KEY, hash TEXT NOT NULL, ts INTEGER NOT NULL, size INTEGER NOT NULL, txs INTEGER NOT NULL, m...\n") {
        t.Errorf("schema not logged on one line, cut at 150:\n%s", buf.String())
    }
    buf.Reset()
    if _, err := db.Exec("insert into cursors (name, place) values (?, ?)", "exec", 1); err != nil { t.Fatal(err) }
    var place int
    if err := db.QueryRow("select place from cursors where name = ?", "exec").Scan(&place); err != nil { t.Fatal(err) }
    var tx, err = db.Begin()
    if err != nil { t.Fatal(err) }
    if _, err := tx.Exec("update cursors set place = 2 where name = 'exec'"); err != nil { t.Fatal(err) }
    var stmt, perr = tx.Prepare("insert into cursors (name, place) values (?, 3)")
    if perr != nil { t.Fatal(perr) }
    for _, name := range []string{"one", "two"} {
        if _, err := stmt.Exec(name); err != nil { t.Fatal(err) }
    }
    stmt.Close()
    if err := tx.Commit(); err != nil { t.Fatal(err) }
    var want = "[DB] insert into cursors (name, place) values (?, ?)\n" +
        "[DB] select place from cursors where name = ?\n" +
        "[DB] update cursors set place = 2 where name = 'exec'\n" +
        "[DB] insert into cursors (name, place) values (?, 3)\n" +
        "[DB] insert into cursors (name, place) values (?, 3)\n"
    if got := buf.String(); got != want {
        t.Errorf("logged:\n%s\nwant:\n%s", got, want)
    }
    logging.SetVerbose(1)
    buf.Reset()
    if _, err := db.Exec("delete from cursors"); err != nil { t.Fatal(err) }
    if buf.Len() != 0 { t.Errorf("logged below DB level: %q", buf.String()) }
}

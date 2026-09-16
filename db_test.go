package main

import "path/filepath"
import "strings"
import "testing"

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

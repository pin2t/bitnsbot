package main

import "path/filepath"
import "testing"

// Every package that owns tables is Init'd by openDB, and every table it stores
// into is created there. This is pinned because forgetting one fails *silently*:
// those packages guard on a nil handle, so the bot runs, logs progress, and
// stores nothing. The address index shipped in exactly that state.
//
// The schema is a copy of tools/tosqlite's, which is what makes that tool the
// upgrade path from the bbolt database this replaced — so the set here is the set
// it writes.
func TestOpenDBTables(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "bitnsbot.sqlite")
    if err := openDB(path); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var want = []string{
        "blocks",                                          // the block-info cache
        "rates", "market",                                 // rates
        "watches",                                         // watches
        "miners", "mineraddr", "minertag",                 // miners
        "addrstat",                                        // per-address statistics
        "cursors",                                         // every scan's place
        "addrindex",                                       // the address index's touches
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
    // and the three indexes the address rankings are read through
    for _, col := range []string{"balance", "txs", "last"} {
        var n int
        if err := db.QueryRow(`select count(*) from sqlite_master where type = 'index'
            and tbl_name = 'addrstat' and sql like ?`, "%("+col+")%").Scan(&n); err != nil {
            t.Fatal(err)
        }
        if n != 1 { t.Errorf("addrstat has %d indexes on %s, want 1", n, col) }
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

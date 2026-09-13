package cursors

import "database/sql"
import "path/filepath"
import "testing"

import _ "modernc.org/sqlite"

// The table as openDB creates it, from the schema tools/tosqlite defines.
const ddl = `create table cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)`

func open(t *testing.T) *sql.DB {
    t.Helper()
    var handle, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { handle.Close(); db = nil })
    if _, err := handle.Exec(ddl); err != nil { t.Fatal(err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    return handle
}

func get(t *testing.T, name string, want int64) {
    t.Helper()
    var v, ok = Get(name)
    if !ok || v != want {
        t.Errorf("%s = %d (found %v), want %d", name, v, ok, want)
    }
}

func set(t *testing.T, handle *sql.DB, name string, v int64) {
    t.Helper()
    var tx, err = handle.Begin()
    if err != nil { t.Fatal(err) }
    if err := Set(tx, name, v); err != nil { t.Fatal(err) }
    if err := tx.Commit(); err != nil { t.Fatal(err) }
}

// Init runs on every start, so repeating it must never disturb a place already
// stored — and a scan advancing its place must not add a second row for itself.
func TestInitLeavesStoredCursorsAlone(t *testing.T) {
    var handle = open(t)
    set(t, handle, Blocks, 900)
    for i := 0; i < 3; i++ {
        if err := Init(handle); err != nil { t.Fatalf("init %d: %v", i, err) }
    }
    get(t, Blocks, 900)
    set(t, handle, Blocks, 901)
    get(t, Blocks, 901)
    var n int
    if err := handle.QueryRow("select count(*) from cursors").Scan(&n); err != nil { t.Fatal(err) }
    if n != 1 { t.Errorf("%d rows for one scan, want 1 — a place is updated, not appended", n) }
}

// A fresh database has no cursors at all: a scan that has never run is not one
// that stopped at height 0, and each picks its own starting point.
func TestFreshDatabaseHasNoCursors(t *testing.T) {
    open(t)
    for _, name := range []string{Blocks, Miners, AddrStat} {
        if v, ok := Get(name); ok {
            t.Errorf("%s came back as %d on a fresh database", name, v)
        }
    }
}

// Set takes the caller's transaction so a scan commits its place with the batch
// that reached it — a rollback must take both or neither.
func TestSetIsPartOfTheCallersTransaction(t *testing.T) {
    var handle = open(t)
    var tx, err = handle.Begin()
    if err != nil { t.Fatal(err) }
    if err := Set(tx, Miners, 500); err != nil { t.Fatal(err) }
    if err := tx.Rollback(); err != nil { t.Fatal(err) }
    if v, ok := Get(Miners); ok {
        t.Errorf("a rolled-back transaction still advanced the cursor to %d", v)
    }
    set(t, handle, Miners, 500)
    get(t, Miners, 500)
}

// Delete forgets a place, so the scan starts from its own beginning again.
func TestDeleteForgetsAPlace(t *testing.T) {
    var handle = open(t)
    set(t, handle, AddrStat, 700)
    var tx, err = handle.Begin()
    if err != nil { t.Fatal(err) }
    if err := Delete(tx, AddrStat); err != nil { t.Fatal(err) }
    if err := tx.Commit(); err != nil { t.Fatal(err) }
    if v, ok := Get(AddrStat); ok { t.Errorf("the place survived as %d", v) }
}

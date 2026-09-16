package main

import "database/sql"
import "fmt"
import "os"
import "path/filepath"
import "strings"
import "testing"

// csvFile writes a CSV of the given lines into a temp directory and returns its
// path, so a test names the table it wants by naming the file.
func csvFile(t *testing.T, name string, lines ...string) string {
    t.Helper()
    var path = filepath.Join(t.TempDir(), name)
    if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil { t.Fatal(err) }
    return path
}

func testDB(t *testing.T) *sql.DB {
    t.Helper()
    var db, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { db.Close() })
    return db
}

// dump reads a two-column table back as text, which is what every assertion
// here is really about: the rows that ended up on disk, not what the importer
// counted.
func dump(t *testing.T, db *sql.DB, name string) map[string]string {
    t.Helper()
    var rows, err = db.Query("select * from " + quote(name))
    if err != nil { t.Fatal(err) }
    defer rows.Close()
    var out = map[string]string{}
    for rows.Next() {
        var k, v string
        if err := rows.Scan(&k, &v); err != nil { t.Fatal(err) }
        out[k] = v
    }
    return out
}

func TestCsvImport(t *testing.T) {
    var db = testDB(t)
    var path = csvFile(t, "active.csv", "key,value", "1111,222", "abc,333")
    var put, over, err = importFile(db, path, tableName(path))
    if err != nil { t.Fatal(err) }
    if put != 2 || over != 0 { t.Fatalf("put %d overwritten %d, want 2 and 0", put, over) }
    var got = dump(t, db, "active")
    if got["1111"] != "222" || got["abc"] != "333" { t.Fatalf("table holds %v", got) }
    if len(got) != 2 { t.Fatalf("table holds %d rows, want 2", len(got)) }
}

// A value is stored as the text the file holds, not as whatever SQLite could read
// it as: a table this tool created has no column types to convert it by, so a
// leading zero survives.
func TestCsvImportKeepsTheText(t *testing.T) {
    var db = testDB(t)
    var path = csvFile(t, "active.csv", "key,value", "0001,0002")
    if _, _, err := importFile(db, path, tableName(path)); err != nil { t.Fatal(err) }
    var kind, value string
    if err := db.QueryRow(`select typeof(value), value from active`).Scan(&kind, &value); err != nil { t.Fatal(err) }
    if kind != "text" || value != "0002" { t.Errorf("value is %s %q, want text %q", kind, value, "0002") }
}

func TestCsvImportTableFromFileName(t *testing.T) {
    for name, want := range map[string]string{
        "active.csv":           "active",
        "/tmp/x/abandoned.csv": "abandoned",
        "./rich.csv":           "rich",
        "blocks-stat.csv":      "blocks-stat",
        "noext":                "noext",
    } {
        if got := tableName(name); got != want { t.Errorf("tableName(%q) = %q, want %q", name, got, want) }
    }
}

// A name that is not a plain identifier still has to reach SQLite as one name,
// since a file name is whatever somebody called the file.
func TestCsvImportQuotesNames(t *testing.T) {
    var db = testDB(t)
    var path = csvFile(t, `blocks-stat.csv`, `Key,"the ""value"""`, "a,1")
    if _, _, err := importFile(db, path, tableName(path)); err != nil { t.Fatal(err) }
    if got := dump(t, db, "blocks-stat"); got["a"] != "1" { t.Fatalf("table holds %v", got) }
}

// The header names the columns, so a file can land in a table the bot already
// has, where the table's own types apply.
func TestCsvImportIntoAnExistingTable(t *testing.T) {
    var db = testDB(t)
    if _, err := db.Exec(`create table rich (addr TEXT PRIMARY KEY, balance INTEGER NOT NULL)`); err != nil { t.Fatal(err) }
    var path = csvFile(t, "rich.csv", "addr,balance", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa,5000000000")
    if _, _, err := importFile(db, path, tableName(path)); err != nil { t.Fatal(err) }
    var balance int64
    var kind string
    if err := db.QueryRow(`select balance, typeof(balance) from rich`).Scan(&balance, &kind); err != nil { t.Fatal(err) }
    if balance != 5000000000 || kind != "integer" { t.Errorf("balance is %s %d", kind, balance) }
}

// A header naming a column the table does not have is a file meant for some other
// table, and must not write a row of it.
func TestCsvImportRejectsAnUnknownColumn(t *testing.T) {
    var db = testDB(t)
    if _, err := db.Exec(`create table rich (addr TEXT PRIMARY KEY, balance INTEGER NOT NULL)`); err != nil { t.Fatal(err) }
    var path = csvFile(t, "rich.csv", "key,value", "a,1")
    if _, _, err := importFile(db, path, tableName(path)); err == nil { t.Fatal("imported into columns the table does not have") }
    var n int
    if err := db.QueryRow(`select count(*) from rich`).Scan(&n); err != nil || n != 0 { t.Errorf("rich holds %d rows (%v)", n, err) }
}

// A second import must merge: a key the file names is overwritten, one it does
// not is left where it was. That is the whole difference between this and a
// reload, so it is asserted on the table rather than on the counts alone.
func TestCsvImportMerges(t *testing.T) {
    var db = testDB(t)
    var first = csvFile(t, "rich.csv", "key,value", "a,1", "b,2")
    if _, _, err := importFile(db, first, tableName(first)); err != nil { t.Fatal(err) }
    var second = csvFile(t, "rich.csv", "key,value", "b,22", "c,3")
    var put, over, err = importFile(db, second, tableName(second))
    if err != nil { t.Fatal(err) }
    if put != 2 || over != 1 { t.Fatalf("put %d overwritten %d, want 2 and 1", put, over) }
    var got = dump(t, db, "rich")
    if got["a"] != "1" || got["b"] != "22" || got["c"] != "3" { t.Fatalf("table holds %v", got) }
    if len(got) != 3 { t.Fatalf("table holds %d rows, want 3", len(got)) }
}

// A header of nothing but the key has no value to overwrite, so a key already
// there is counted and left alone rather than failing the import.
func TestCsvImportKeysOnly(t *testing.T) {
    var db = testDB(t)
    var first = csvFile(t, "addrs.csv", "addr", "a", "b")
    if _, _, err := importFile(db, first, tableName(first)); err != nil { t.Fatal(err) }
    var second = csvFile(t, "addrs.csv", "addr", "b", "c")
    var put, over, err = importFile(db, second, tableName(second))
    if err != nil { t.Fatal(err) }
    if put != 2 || over != 1 { t.Fatalf("put %d overwritten %d, want 2 and 1", put, over) }
    var n int
    if err := db.QueryRow(`select count(*) from addrs`).Scan(&n); err != nil || n != 3 { t.Errorf("addrs holds %d rows (%v)", n, err) }
}

// Committing per batch is what lets a chain-sized file through, so every row
// still has to land when the file is longer than one.
func TestCsvImportBatches(t *testing.T) {
    var db = testDB(t)
    var lines = []string{"key,value"}
    for i := 0; i < 25; i++ { lines = append(lines, fmt.Sprintf("k%d,v%d", i, i)) }
    var old = *batch
    *batch = 4
    defer func() { *batch = old }()
    var path = csvFile(t, "active.csv", lines...)
    var put, _, err = importFile(db, path, tableName(path))
    if err != nil { t.Fatal(err) }
    if put != 25 { t.Fatalf("put %d rows, want 25", put) }
    var got = dump(t, db, "active")
    if len(got) != 25 { t.Fatalf("table holds %d rows, want 25", len(got)) }
    if got["k24"] != "v24" { t.Fatalf("last row is %q", got["k24"]) }
}

func TestCsvImportEmptyFileMakesTheTable(t *testing.T) {
    var db = testDB(t)
    var path = csvFile(t, "active.csv", "key,value")
    var put, _, err = importFile(db, path, tableName(path))
    if err != nil { t.Fatal(err) }
    if put != 0 { t.Fatalf("put %d rows, want 0", put) }
    if got := dump(t, db, "active"); len(got) != 0 { t.Fatalf("table holds %v", got) }
}

func TestCsvImportRejectsBadInput(t *testing.T) {
    var cases = map[string][]string{
        "unnamed column": {"key,", "a,1"},
        "repeated column": {"key,Key", "a,1"},
        "one column":     {"key,value", "a"},
        "three columns":  {"key,value", "a,1,x"},
        "empty key":      {"key,value", ",1"},
        "no header":      {},
    }
    for name, lines := range cases {
        var db = testDB(t)
        var path = csvFile(t, "active.csv", lines...)
        if _, _, err := importFile(db, path, tableName(path)); err == nil {
            t.Errorf("%s: imported without an error", name)
        }
    }
}

// A row that fails mid-file leaves the batches before it committed, which is
// what makes re-running the fixed file the recovery — so the rows that did land
// must be readable rather than lost in an aborted transaction.
func TestCsvImportKeepsCommittedBatches(t *testing.T) {
    var db = testDB(t)
    var old = *batch
    *batch = 2
    defer func() { *batch = old }()
    var path = csvFile(t, "active.csv", "key,value", "a,1", "b,2", ",3")
    var put, _, err = importFile(db, path, tableName(path))
    if err == nil { t.Fatal("imported a row with an empty key") }
    if put != 2 { t.Fatalf("put %d rows before the error, want 2", put) }
    if got := dump(t, db, "active"); len(got) != 2 { t.Fatalf("table holds %v", got) }
}

package main

import "fmt"
import "os"
import "path/filepath"
import "strings"
import "testing"

import "go.etcd.io/bbolt"

// csvFile writes a CSV of the given lines into a temp directory and returns its
// path, so a test names the bucket it wants by naming the file.
func csvFile(t *testing.T, name string, lines ...string) string {
    t.Helper()
    var path = filepath.Join(t.TempDir(), name)
    if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil { t.Fatal(err) }
    return path
}

func testDB(t *testing.T) *bbolt.DB {
    t.Helper()
    var db, err = bbolt.Open(filepath.Join(t.TempDir(), "test.db"), 0600, nil)
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { db.Close() })
    return db
}

// dump reads a whole bucket back, which is what every assertion here is really
// about: the rows that ended up on disk, not what the importer counted.
func dump(t *testing.T, db *bbolt.DB, name string) map[string]string {
    t.Helper()
    var out = map[string]string{}
    var err = db.View(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte(name))
        if b == nil { return fmt.Errorf("bucket %s does not exist", name) }
        return b.ForEach(func(k, v []byte) error {
            out[string(k)] = string(v)
            return nil
        })
    })
    if err != nil { t.Fatal(err) }
    return out
}

func TestCsvImport(t *testing.T) {
    var db = testDB(t)
    var path = csvFile(t, "active.csv", "key,value", "1111,222", "abc,333")
    var put, over, err = importFile(db, path, bucketName(path))
    if err != nil { t.Fatal(err) }
    if put != 2 || over != 0 { t.Fatalf("put %d overwritten %d, want 2 and 0", put, over) }
    var got = dump(t, db, "active")
    if got["1111"] != "222" || got["abc"] != "333" { t.Fatalf("bucket holds %v", got) }
    if len(got) != 2 { t.Fatalf("bucket holds %d keys, want 2", len(got)) }
}

func TestCsvImportBucketFromFileName(t *testing.T) {
    for name, want := range map[string]string{
        "active.csv":              "active",
        "/tmp/x/abandoned.csv":    "abandoned",
        "./rich.csv":              "rich",
        "blocks-stat.csv":         "blocks-stat",
        "noext":                   "noext",
    } {
        if got := bucketName(name); got != want { t.Errorf("bucketName(%q) = %q, want %q", name, got, want) }
    }
}

// A second import must merge: a key the file names is overwritten, one it does
// not is left where it was. That is the whole difference between this and a
// reload, so it is asserted on the bucket rather than on the counts alone.
func TestCsvImportMerges(t *testing.T) {
    var db = testDB(t)
    var first = csvFile(t, "rich.csv", "key,value", "a,1", "b,2")
    if _, _, err := importFile(db, first, bucketName(first)); err != nil { t.Fatal(err) }
    var second = csvFile(t, "rich.csv", "key,value", "b,22", "c,3")
    var put, over, err = importFile(db, second, bucketName(second))
    if err != nil { t.Fatal(err) }
    if put != 2 || over != 1 { t.Fatalf("put %d overwritten %d, want 2 and 1", put, over) }
    var got = dump(t, db, "rich")
    if got["a"] != "1" || got["b"] != "22" || got["c"] != "3" { t.Fatalf("bucket holds %v", got) }
    if len(got) != 3 { t.Fatalf("bucket holds %d keys, want 3", len(got)) }
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
    var put, _, err = importFile(db, path, bucketName(path))
    if err != nil { t.Fatal(err) }
    if put != 25 { t.Fatalf("put %d rows, want 25", put) }
    var got = dump(t, db, "active")
    if len(got) != 25 { t.Fatalf("bucket holds %d keys, want 25", len(got)) }
    if got["k24"] != "v24" { t.Fatalf("last row is %q", got["k24"]) }
}

func TestCsvImportEmptyFileMakesTheBucket(t *testing.T) {
    var db = testDB(t)
    var path = csvFile(t, "active.csv", "key,value")
    var put, _, err = importFile(db, path, bucketName(path))
    if err != nil { t.Fatal(err) }
    if put != 0 { t.Fatalf("put %d rows, want 0", put) }
    if got := dump(t, db, "active"); len(got) != 0 { t.Fatalf("bucket holds %v", got) }
}

func TestCsvImportRejectsBadInput(t *testing.T) {
    var cases = map[string][]string{
        "wrong header":   {"addr,balance", "a,1"},
        "one column":     {"key,value", "a"},
        "three columns":  {"key,value", "a,1,x"},
        "empty key":      {"key,value", ",1"},
        "no header":      {},
    }
    for name, lines := range cases {
        var db = testDB(t)
        var path = csvFile(t, "active.csv", lines...)
        if _, _, err := importFile(db, path, bucketName(path)); err == nil {
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
    var put, _, err = importFile(db, path, bucketName(path))
    if err == nil { t.Fatal("imported a row with an empty key") }
    if put != 2 { t.Fatalf("put %d rows before the error, want 2", put) }
    if got := dump(t, db, "active"); len(got) != 2 { t.Fatalf("bucket holds %v", got) }
}

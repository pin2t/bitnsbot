package cursors

import "path/filepath"
import "testing"

import "go.etcd.io/bbolt"

func open(t *testing.T) *bbolt.DB {
    t.Helper()
    var db, err = bbolt.Open(filepath.Join(t.TempDir(), "test.db"), 0600, nil)
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { db.Close() })
    return db
}

// seedOld writes a cursor the way it was kept before this package: one bucket
// per scan, each holding a single key. Only the buckets survive into these
// tests; their values were carried across a version ago.
func seedOld(t *testing.T, db *bbolt.DB, bucket, key, value string) {
    t.Helper()
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b, berr = tx.CreateBucketIfNotExists([]byte(bucket))
        if berr != nil { return berr }
        return b.Put([]byte(key), []byte(value))
    })
    if err != nil { t.Fatal(err) }
}

func get(t *testing.T, name string, want int64) {
    t.Helper()
    var v, ok = Get(name)
    if !ok || v != want {
        t.Errorf("%s = %d (found %v), want %d", name, v, ok, want)
    }
}

// The buckets each cursor used to live in are dropped on the first start that
// finds them. Their values were carried across by the version before this one;
// what is left is an empty shell nothing reads.
func TestInitRemovesTheObsoleteBuckets(t *testing.T) {
    var db = open(t)
    seedOld(t, db, "blocks-cursor", "cursor", "812345")
    seedOld(t, db, "miners-cursor", "cursor", "964000")
    seedOld(t, db, "addrindex-cursor", "cursor", "700100")
    // a database already through the carrying version: its places are here
    if err := Init(db); err != nil { t.Fatalf("init: %v", err) }
    if err := db.Update(func(tx *bbolt.Tx) error {
        if err := Set(tx, Blocks, 812345); err != nil { return err }
        return Set(tx, AddrIndex, 700100)
    }); err != nil { t.Fatal(err) }
    if err := Init(db); err != nil { t.Fatalf("init again: %v", err) }
    db.View(func(tx *bbolt.Tx) error {
        for _, name := range obsolete {
            if tx.Bucket([]byte(name)) != nil { t.Errorf("%s is still there", name) }
        }
        return nil
    })
    // and dropping them left this bucket alone
    get(t, Blocks, 812345)
    get(t, AddrIndex, 700100)
}

// Init runs from every package that keeps a cursor, and again on every start, so
// repeating it must never disturb a place already stored.
func TestInitLeavesStoredCursorsAlone(t *testing.T) {
    var db = open(t)
    if err := Init(db); err != nil { t.Fatalf("init: %v", err) }
    if err := db.Update(func(tx *bbolt.Tx) error { return Set(tx, Blocks, 900) }); err != nil {
        t.Fatal(err)
    }
    for i := 0; i < 3; i++ {
        if err := Init(db); err != nil { t.Fatalf("init %d: %v", i, err) }
    }
    get(t, Blocks, 900)
}

// A fresh database has no old buckets and no cursors: a scan that has never run
// is not one that stopped at height 0, and each picks its own starting point.
func TestFreshDatabaseHasNoCursors(t *testing.T) {
    var db = open(t)
    if err := Init(db); err != nil { t.Fatalf("init: %v", err) }
    for _, name := range []string{Blocks, Miners, AddrIndex, ActBuild} {
        if v, ok := Get(name); ok {
            t.Errorf("%s came back as %d on a fresh database", name, v)
        }
    }
    db.View(func(tx *bbolt.Tx) error {
        for _, old := range []string{"blocks-cursor", "miners-cursor", "addrindex-cursor"} {
            if tx.Bucket([]byte(old)) != nil { t.Errorf("%s was created on a fresh database", old) }
        }
        return nil
    })
}

// Set takes the caller's transaction so a scan commits its place with the batch
// that reached it — a rollback must take both or neither.
func TestSetIsPartOfTheCallersTransaction(t *testing.T) {
    var db = open(t)
    if err := Init(db); err != nil { t.Fatalf("init: %v", err) }
    var boom = "no"
    db.Update(func(tx *bbolt.Tx) error {
        if err := Set(tx, Miners, 500); err != nil { return err }
        return &failure{boom}
    })
    if v, ok := Get(Miners); ok {
        t.Errorf("a rolled-back transaction still advanced the cursor to %d", v)
    }
    if err := db.Update(func(tx *bbolt.Tx) error { return Set(tx, Miners, 500) }); err != nil {
        t.Fatal(err)
    }
    get(t, Miners, 500)
}

type failure struct{ s string }

func (f *failure) Error() string { return f.s }

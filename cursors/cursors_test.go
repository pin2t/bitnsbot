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
// per scan, each holding a single key.
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

// A database written before this keeps its place. Losing one is not a lost
// setting: the address index would rebuild from genesis, which is hours.
func TestInitCarriesTheOldBucketsForward(t *testing.T) {
    var db = open(t)
    seedOld(t, db, "blocks-cursor", "cursor", "812345")
    seedOld(t, db, "miners-cursor", "cursor", "964000")
    seedOld(t, db, "addrindex-cursor", "cursor", "700100")
    seedOld(t, db, "addrindex-cursor", "actbuild-file", "57")
    if err := Init(db); err != nil { t.Fatalf("init: %v", err) }
    get(t, Blocks, 812345)
    get(t, Miners, 964000)
    get(t, AddrIndex, 700100)
    get(t, ActBuild, 57)
    // the old buckets are left alone, so going back to the previous binary
    // costs nothing
    db.View(func(tx *bbolt.Tx) error {
        if tx.Bucket([]byte("blocks-cursor")) == nil {
            t.Error("the old bucket was removed; a rollback would rescan the chain")
        }
        return nil
    })
}

// Init runs from every package that keeps a cursor, and again on every start, so
// it must never walk a scan backwards to where the old bucket was left.
func TestInitDoesNotOverwriteProgress(t *testing.T) {
    var db = open(t)
    seedOld(t, db, "blocks-cursor", "cursor", "100")
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

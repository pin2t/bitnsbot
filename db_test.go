package main

import "path/filepath"
import "testing"

import "go.etcd.io/bbolt"

// Every package that owns buckets is Init'd by openDB. This is pinned because
// forgetting one fails *silently*: those packages guard on a nil handle, so the
// bot runs, logs progress, and stores nothing. The address index shipped in
// exactly that state — it backfilled the chain into a bucket that was never
// created.
// The three buckets an address set used to be imported through are gone, and a
// database that has been through an earlier version must not keep carrying them.
// The indexes built from the records, which share their names' prefix, are live
// and must survive.
func TestOpenDBDropsTheUnusedBuckets(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "bitnsbot.db")
    var handle, err = bbolt.Open(path, 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    if err := handle.Update(func(tx *bbolt.Tx) error {
        for _, name := range []string{"active", "rich", "abandoned", "richindex"} {
            var b, berr = tx.CreateBucket([]byte(name))
            if berr != nil { return berr }
            if err := b.Put([]byte("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"), []byte("1")); err != nil { return err }
        }
        return nil
    }); err != nil { t.Fatalf("seed: %v", err) }
    handle.Close()
    if err := openDB(path); err != nil { t.Fatalf("openDB: %v", err) }
    defer closeDB()
    db.View(func(tx *bbolt.Tx) error {
        for _, name := range []string{"active", "rich", "abandoned"} {
            if tx.Bucket([]byte(name)) != nil { t.Errorf("%s survived", name) }
        }
        if tx.Bucket([]byte("richindex")) == nil { t.Error("richindex was dropped; it is the live ranking") }
        return nil
    })
    // and a second start has nothing to do
    closeDB()
    if err := openDB(path); err != nil { t.Fatalf("second openDB: %v", err) }
}

func TestOpenDBBuckets(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "bitnsbot.db")
    if err := openDB(path); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var want = []string{
        "blocks",                                          // blocks cache
        "rates", "market",                                 // rates
        "watches",                                         // watches
        "miners",                                          // miners
        "addrindex",                                       // addrindex
        "addrstat",                                        // per-address statistics
        "cursors",                                         // every scan's place
    }
    var got = map[string]bool{}
    db.View(func(tx *bbolt.Tx) error {
        return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
            got[string(name)] = true
            return nil
        })
    })
    for _, name := range want {
        if !got[name] {
            t.Errorf("bucket %q was never created — is its package missing an Init in openDB?", name)
        }
    }
    if len(got) != len(want) {
        t.Errorf("bucket count = %d, want %d: %v", len(got), len(want), got)
    }
}

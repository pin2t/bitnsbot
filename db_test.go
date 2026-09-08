package main

import "path/filepath"
import "strings"
import "testing"

import "go.etcd.io/bbolt"

// Every package that owns buckets is Init'd by openDB. This is pinned because
// forgetting one fails *silently*: those packages guard on a nil handle, so the
// bot runs, logs progress, and stores nothing. The address index shipped in
// exactly that state — it backfilled the chain into a bucket that was never
// created.
func TestOpenDBBuckets(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "bitnsbot.db")
    if err := openDB(path); err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    var want = []string{
        "blocks-stat",                                     // blocks cache
        "rates", "market",                                 // rates
        "watches",                                         // watches
        "miners", "miners-tag", "miners-stat",             // miners
        "addrindex",                                       // addrindex
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

// The three buckets each cursor used to live in are gone after a start, through
// main's own startup path — every package's Init drops them, so whichever runs
// first does it.
func TestOpenDBRemovesObsoleteCursorBuckets(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "old.db")
    var handle, err = bbolt.Open(path, 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    err = handle.Update(func(tx *bbolt.Tx) error {
        for _, name := range []string{"blocks-cursor", "miners-cursor", "addrindex-cursor"} {
            var b, berr = tx.CreateBucketIfNotExists([]byte(name))
            if berr != nil { return berr }
            if perr := b.Put([]byte("cursor"), []byte("812345")); perr != nil { return perr }
        }
        return nil
    })
    if err != nil { t.Fatalf("seed: %v", err) }
    if err := handle.Close(); err != nil { t.Fatalf("close: %v", err) }

    if err := openDB(path); err != nil { t.Fatalf("openDB: %v", err) }
    defer closeDB()
    db.View(func(tx *bbolt.Tx) error {
        return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
            if strings.HasSuffix(string(name), "-cursor") {
                t.Errorf("bucket %q survived the start", name)
            }
            return nil
        })
    })
}

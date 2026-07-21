package main

import "path/filepath"
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
        "blocks",                                          // db.go
        "rates",                                           // rates
        "watches",                                         // watches
        "miners", "miners-tag", "miners-stat", "miners-block", // miners
        "addrindex", "addrindex-cursor",                   // addrindex
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

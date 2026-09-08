package main

import "path/filepath"
import "testing"

import "bitnsbot/cursors"
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

// A database written before the cursors bucket existed keeps every scan's place
// through openDB. This is the end of the migration that matters: main's own
// startup path, with each package's Init doing the carrying. Losing the address
// index's place alone would be a rebuild measured in hours.
func TestOpenDBCarriesOldCursorsForward(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "old.db")
    var handle, err = bbolt.Open(path, 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    err = handle.Update(func(tx *bbolt.Tx) error {
        for bucket, rows := range map[string]map[string]string{
            "blocks-cursor":    {"cursor": "812345"},
            "miners-cursor":    {"cursor": "964000"},
            "addrindex-cursor": {"cursor": "700100", "actbuild-file": "57"},
        } {
            var b, berr = tx.CreateBucketIfNotExists([]byte(bucket))
            if berr != nil { return berr }
            for k, v := range rows {
                if perr := b.Put([]byte(k), []byte(v)); perr != nil { return perr }
            }
        }
        return nil
    })
    if err != nil { t.Fatalf("seed: %v", err) }
    if err := handle.Close(); err != nil { t.Fatalf("close: %v", err) }

    if err := openDB(path); err != nil { t.Fatalf("openDB: %v", err) }
    defer closeDB()
    for name, want := range map[string]int64{
        cursors.Blocks: 812345, cursors.Miners: 964000,
        cursors.AddrIndex: 700100, cursors.ActBuild: 57,
    } {
        var got, ok = cursors.Get(name)
        if !ok || got != want {
            t.Errorf("%s = %d (found %v), want %d", name, got, ok, want)
        }
    }
}

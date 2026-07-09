package main

import "encoding/json"
import "path/filepath"
import "testing"

import "go.etcd.io/bbolt"

func TestWatchStoreAdd(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "watches.db")
    var s, err = openWatchStore(path)
    if err != nil {
        t.Fatalf("openWatchStore: %v", err)
    }
    defer s.close()
    if err := s.add(42, watchTypeAddress, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"); err != nil {
        t.Fatalf("add: %v", err)
    }
    var records []watchRecord
    err = s.db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket(watchesBucket).ForEach(func(k, v []byte) error {
            var r watchRecord
            if err := json.Unmarshal(v, &r); err != nil {
                return err
            }
            records = append(records, r)
            return nil
        })
    })
    if err != nil {
        t.Fatalf("view: %v", err)
    }
    if len(records) != 1 {
        t.Fatalf("expected 1 record, got %d", len(records))
    }
    var r = records[0]
    if r.ChatID != 42 || r.Type != watchTypeAddress || r.WatchID != "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa" {
        t.Fatalf("unexpected record: %#v", r)
    }
    if r.CreatedAt.IsZero() {
        t.Fatalf("expected non-zero CreatedAt")
    }
}

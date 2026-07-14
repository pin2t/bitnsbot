package main

import "encoding/json"
import "path/filepath"
import "testing"

import "go.etcd.io/bbolt"

func TestWatchStoreAdd(t *testing.T) {
    var path = filepath.Join(t.TempDir(), "watches.db")
    var err = openDB(path)
    if err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    if err := addWatch(42, watchTypeAddress, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"); err != nil {
        t.Fatalf("add: %v", err)
    }
    var records []watchRecord
    err = db.View(func(tx *bbolt.Tx) error {
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

func TestWatchStoreList(t *testing.T) {
    var err = openDB(filepath.Join(t.TempDir(), "watches.db"))
    if err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    addWatch(1, watchTypeAddress, "addrA")
    addWatch(2, watchTypeTransaction, "txB")
    var records, listErr = listWatches()
    if listErr != nil {
        t.Fatalf("list: %v", listErr)
    }
    if len(records) != 2 {
        t.Fatalf("expected 2 records, got %d", len(records))
    }
    if records[0].ChatID != 1 || records[0].WatchID != "addrA" {
        t.Fatalf("unexpected first record: %#v", records[0])
    }
    if records[1].ChatID != 2 || records[1].Type != watchTypeTransaction {
        t.Fatalf("unexpected second record: %#v", records[1])
    }
}

func TestWatchStoreRemove(t *testing.T) {
    var err = openDB(filepath.Join(t.TempDir(), "watches.db"))
    if err != nil {
        t.Fatalf("openDB: %v", err)
    }
    defer closeDB()
    // two chats watch the same address; a third watch is unrelated
    addWatch(1, watchTypeAddress, "sharedAddr")
    addWatch(2, watchTypeAddress, "sharedAddr")
    addWatch(1, watchTypeAddress, "otherAddr")
    // removing chat 1's watch on sharedAddr must not touch chat 2's identical watch
    var removed, remErr = removeWatch(1, "sharedAddr")
    if remErr != nil {
        t.Fatalf("remove: %v", remErr)
    }
    if removed != 1 {
        t.Fatalf("expected 1 removed, got %d", removed)
    }
    var records, _ = listWatches()
    if len(records) != 2 {
        t.Fatalf("expected 2 remaining records, got %d: %#v", len(records), records)
    }
    for _, r := range records {
        if r.ChatID == 1 && r.WatchID == "sharedAddr" {
            t.Fatalf("chat 1's sharedAddr watch should be gone: %#v", records)
        }
    }
    // removing a watch that doesn't belong to the chat removes nothing
    var n, _ = removeWatch(999, "sharedAddr")
    if n != 0 {
        t.Fatalf("expected 0 removed for wrong chat, got %d", n)
    }
}

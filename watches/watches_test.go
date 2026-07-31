package watches

import "path/filepath"
import "testing"

import "go.etcd.io/bbolt"

func openTestDB(t *testing.T) {
    var d, err = bbolt.Open(filepath.Join(t.TempDir(), "watches.db"), 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    if err := Init(d); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { d.Close(); db = nil })
}

func TestAdd(t *testing.T) {
    openTestDB(t)
    if err := Add(42, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "Cold wallet"); err != nil {
        t.Fatalf("add: %v", err)
    }
    var list, err = List()
    if err != nil {
        t.Fatalf("list: %v", err)
    }
    if len(list) != 1 {
        t.Fatalf("expected 1 watch, got %d", len(list))
    }
    if w := list[0]; w.Chat != 42 || w.Address != "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa" || w.Alias != "Cold wallet" {
        t.Fatalf("unexpected watch: %#v", w)
    }
}

func TestList(t *testing.T) {
    openTestDB(t)
    Add(1, "addrA", "")
    Add(2, "addrB", "")
    var list, err = List()
    if err != nil {
        t.Fatalf("list: %v", err)
    }
    if len(list) != 2 {
        t.Fatalf("expected 2 watches, got %d", len(list))
    }
    if list[0].Chat != 1 || list[0].Address != "addrA" {
        t.Fatalf("unexpected first: %#v", list[0])
    }
    if list[1].Chat != 2 || list[1].Address != "addrB" {
        t.Fatalf("unexpected second: %#v", list[1])
    }
}

func TestRemove(t *testing.T) {
    openTestDB(t)
    // two chats watch the same address; a third watch is unrelated
    Add(1, "sharedAddr", "")
    Add(2, "sharedAddr", "")
    Add(1, "otherAddr", "")
    // removing chat 1's watch on sharedAddr must not touch chat 2's identical watch
    var removed, err = Remove(1, "sharedAddr")
    if err != nil {
        t.Fatalf("remove: %v", err)
    }
    if removed != 1 {
        t.Fatalf("expected 1 removed, got %d", removed)
    }
    var list, _ = List()
    if len(list) != 2 {
        t.Fatalf("expected 2 remaining, got %d: %#v", len(list), list)
    }
    for _, w := range list {
        if w.Chat == 1 && w.Address == "sharedAddr" {
            t.Fatalf("chat 1's sharedAddr watch should be gone: %#v", list)
        }
    }
    // removing a watch that doesn't belong to the chat removes nothing
    if n, _ := Remove(999, "sharedAddr"); n != 0 {
        t.Fatalf("expected 0 removed for wrong chat, got %d", n)
    }
}

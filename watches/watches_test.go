package watches

import "encoding/binary"
import "encoding/json"
import "path/filepath"
import "strings"
import "testing"
import "time"

import "go.etcd.io/bbolt"

// open returns the handle as well, which the format and migration tests need to
// look at the bucket directly.
func open(t *testing.T) *bbolt.DB {
    var d, err = bbolt.Open(filepath.Join(t.TempDir(), "watches.db"), 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    if err := Init(d); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { d.Close(); db = nil })
    return d
}

func openTestDB(t *testing.T) { open(t) }

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

// The key is "<chat>,<address>" and the value holds only what is left, so a
// record can be read without decoding anything to find out whose it is.
func TestKeyFormat(t *testing.T) {
    var db = open(t)
    if err := Add(260439275, "bc1q5rasj5fedy3f9vgh9x84jqlgtvj964k0xn5z6r", "Cold"); err != nil {
        t.Fatal(err)
    }
    db.View(func(tx *bbolt.Tx) error {
        var k, v = tx.Bucket([]byte("watches")).Cursor().First()
        if string(k) != "260439275,bc1q5rasj5fedy3f9vgh9x84jqlgtvj964k0xn5z6r" {
            t.Errorf("key = %q", k)
        }
        var fields map[string]any
        if err := json.Unmarshal(v, &fields); err != nil { t.Fatal(err) }
        if len(fields) != 2 || fields["alias"] != "Cold" {
            t.Errorf("value = %s, want only created and alias", v)
        }
        if _, ok := fields["created"]; !ok { t.Errorf("value has no created: %s", v) }
        return nil
    })
    // a negative chat id — a Telegram group — round-trips too
    if err := Add(-1001234567890, "addrG", ""); err != nil { t.Fatal(err) }
    var list, err = List()
    if err != nil { t.Fatal(err) }
    var found bool
    for _, w := range list {
        if w.Chat == -1001234567890 && w.Address == "addrG" { found = true }
    }
    if !found { t.Errorf("a group chat's watch did not round-trip: %+v", list) }
}

// The pair is the key, so watching the same address twice is one watch, not two
// — and it keeps the time it was first made rather than looking newly created.
func TestAddIsIdempotent(t *testing.T) {
    var db = open(t)
    if err := Add(7, "addrA", "First"); err != nil { t.Fatal(err) }
    var created int64
    db.View(func(tx *bbolt.Tx) error {
        var _, v = tx.Bucket([]byte("watches")).Cursor().First()
        var r struct{ Created int64 `json:"created"` }
        json.Unmarshal(v, &r)
        created = r.Created
        return nil
    })
    time.Sleep(1100 * time.Millisecond)
    if err := Add(7, "addrA", "Second"); err != nil { t.Fatal(err) }
    var list, _ = List()
    if len(list) != 1 || list[0].Alias != "Second" {
        t.Fatalf("re-watching gave %+v, want one watch with the newer alias", list)
    }
    db.View(func(tx *bbolt.Tx) error {
        var _, v = tx.Bucket([]byte("watches")).Cursor().First()
        var r struct{ Created int64 `json:"created"` }
        json.Unmarshal(v, &r)
        if r.Created != created {
            t.Errorf("created moved from %d to %d; it is the same watch", created, r.Created)
        }
        return nil
    })
}

// Count reads the chat's own keys, and the comma in the prefix is what keeps
// chat 26 out of chat 260's count.
func TestCountIsScopedByPrefix(t *testing.T) {
    open(t)
    for _, w := range []struct {
        chat int64
        addr string
    }{{26, "a"}, {26, "b"}, {260, "c"}, {260, "d"}, {260, "e"}, {-26, "f"}} {
        if err := Add(w.chat, w.addr, ""); err != nil { t.Fatal(err) }
    }
    for chat, want := range map[int64]int{26: 2, 260: 3, -26: 1, 99: 0} {
        var got, err = Count(chat)
        if err != nil { t.Fatal(err) }
        if got != want { t.Errorf("Count(%d) = %d, want %d", chat, got, want) }
    }
}

// seedOldFormat writes a record the way the store used to: an auto-incrementing
// numeric key, with the chat and the address inside the value.
func seedOldFormat(t *testing.T, db *bbolt.DB, created, chat int64, address, alias string) {
    t.Helper()
    var err = db.Update(func(tx *bbolt.Tx) error {
        var b = tx.Bucket([]byte("watches"))
        var id, err = b.NextSequence()
        if err != nil { return err }
        var buf = make([]byte, 8)
        binary.BigEndian.PutUint64(buf, id)
        var data, merr = json.Marshal(map[string]any{
            "created": created, "chat": chat, "watch": address, "alias": alias,
        })
        if merr != nil { return merr }
        return b.Put(buf, data)
    })
    if err != nil { t.Fatal(err) }
}

// A database written under the old format keeps every watch through Init, under
// the new key — losing one means a chat silently stops being notified.
func TestInitMigratesOldRecords(t *testing.T) {
    var db = open(t)
    seedOldFormat(t, db, 1000, 42, "addrA", "Savings")
    seedOldFormat(t, db, 2000, 42, "addrB", "")
    seedOldFormat(t, db, 3000, -99, "addrC", "Group")
    if err := Init(db); err != nil { t.Fatalf("init: %v", err) }
    var list, err = List()
    if err != nil { t.Fatal(err) }
    if len(list) != 3 { t.Fatalf("got %d watches, want 3: %+v", len(list), list) }
    // oldest first, by the stored time the old records carried
    if list[0].Address != "addrA" || list[2].Address != "addrC" {
        t.Errorf("order = %+v, want oldest first", list)
    }
    if list[0].Chat != 42 || list[0].Alias != "Savings" || list[2].Chat != -99 {
        t.Errorf("fields did not survive: %+v", list)
    }
    // every key is the new form, and nothing numeric is left behind
    db.View(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("watches")).ForEach(func(k, v []byte) error {
            if _, _, ok := parseKey(k); !ok { t.Errorf("key %x survived the migration", k) }
            if strings.Contains(string(v), `"chat"`) || strings.Contains(string(v), `"watch"`) {
                t.Errorf("value still carries the moved fields: %s", v)
            }
            return nil
        })
    })
    // and the chat-scoped operations work on what came across
    if n, _ := Count(42); n != 2 { t.Errorf("Count(42) = %d after migrating, want 2", n) }
    if n, _ := Remove(42, "addrA"); n != 1 { t.Errorf("Remove of a migrated watch removed %d", n) }
}

// Init runs on every start, so a second one must find nothing to do and must not
// disturb what is there.
func TestInitMigrationIsIdempotent(t *testing.T) {
    var db = open(t)
    seedOldFormat(t, db, 1000, 42, "addrA", "Savings")
    if err := Init(db); err != nil { t.Fatal(err) }
    if err := Add(42, "addrNew", "Fresh"); err != nil { t.Fatal(err) }
    for i := 0; i < 3; i++ {
        if err := Init(db); err != nil { t.Fatalf("init %d: %v", i, err) }
    }
    var list, _ = List()
    if len(list) != 2 { t.Fatalf("got %d watches after re-running Init, want 2: %+v", len(list), list) }
}

// The old format allowed a chat to hold the same address twice; the new key
// cannot, so the newest of them survives with its alias.
func TestMigrationCollapsesDuplicates(t *testing.T) {
    var db = open(t)
    seedOldFormat(t, db, 1000, 42, "addrA", "Old name")
    seedOldFormat(t, db, 2000, 42, "addrA", "New name")
    if err := Init(db); err != nil { t.Fatal(err) }
    var list, _ = List()
    if len(list) != 1 || list[0].Alias != "New name" {
        t.Fatalf("duplicates gave %+v, want one watch with the newer alias", list)
    }
}

// A value this cannot read is left where it is: dropping records silently is
// worse than a stray key nothing serves.
func TestMigrationKeepsUnreadableRecords(t *testing.T) {
    var db = open(t)
    if err := db.Update(func(tx *bbolt.Tx) error {
        return tx.Bucket([]byte("watches")).Put([]byte{0, 0, 0, 0, 0, 0, 0, 9}, []byte("not json"))
    }); err != nil { t.Fatal(err) }
    if err := Init(db); err != nil { t.Fatal(err) }
    var kept bool
    db.View(func(tx *bbolt.Tx) error {
        kept = tx.Bucket([]byte("watches")).Get([]byte{0, 0, 0, 0, 0, 0, 0, 9}) != nil
        return nil
    })
    if !kept { t.Error("an unreadable record was thrown away") }
    if list, _ := List(); len(list) != 0 { t.Errorf("it was also served: %+v", list) }
}

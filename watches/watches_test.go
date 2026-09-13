package watches

import "database/sql"
import "path/filepath"
import "testing"
import "time"

import _ "modernc.org/sqlite"

// The table as openDB creates it, from the schema tools/tosqlite defines.
const ddl = `create table watches (chat INTEGER NOT NULL, addr TEXT NOT NULL, alias TEXT NOT NULL,
    created INTEGER NOT NULL, PRIMARY KEY (chat, addr))`

// open returns the handle as well, which the format tests need to look at the
// table directly.
func open(t *testing.T) *sql.DB {
    t.Helper()
    var handle, err = sql.Open("sqlite", filepath.Join(t.TempDir(), "watches.db"))
    if err != nil { t.Fatalf("open: %v", err) }
    if _, err := handle.Exec(ddl); err != nil { t.Fatal(err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { handle.Close(); db = nil })
    return handle
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

// The pair is the primary key — `(chat, addr)` — so a row says whose it is
// without anything being decoded, and a chat id is an integer rather than text.
func TestKeyFormat(t *testing.T) {
    var handle = open(t)
    if err := Add(260439275, "bc1q5rasj5fedy3f9vgh9x84jqlgtvj964k0xn5z6r", "Cold"); err != nil {
        t.Fatal(err)
    }
    var chat, created int64
    var addr, alias string
    if err := handle.QueryRow("select chat, addr, alias, created from watches").Scan(
        &chat, &addr, &alias, &created); err != nil {
        t.Fatal(err)
    }
    if chat != 260439275 || addr != "bc1q5rasj5fedy3f9vgh9x84jqlgtvj964k0xn5z6r" || alias != "Cold" {
        t.Errorf("row = %d %q %q", chat, addr, alias)
    }
    if created == 0 { t.Error("created was not stored") }
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
    var handle = open(t)
    if err := Add(7, "addrA", "First"); err != nil { t.Fatal(err) }
    var created int64
    if err := handle.QueryRow("select created from watches").Scan(&created); err != nil { t.Fatal(err) }
    time.Sleep(1100 * time.Millisecond)
    if err := Add(7, "addrA", "Second"); err != nil { t.Fatal(err) }
    var list, _ = List()
    if len(list) != 1 || list[0].Alias != "Second" {
        t.Fatalf("re-watching gave %+v, want one watch with the newer alias", list)
    }
    var again int64
    if err := handle.QueryRow("select created from watches").Scan(&again); err != nil { t.Fatal(err) }
    if again != created {
        t.Errorf("created moved from %d to %d; it is the same watch", created, again)
    }
}

// Count is a chat's own rows, and the chat being an integer column is what keeps
// chat 26 out of chat 260's count — where a text key prefix had to carry a comma
// to do it.
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

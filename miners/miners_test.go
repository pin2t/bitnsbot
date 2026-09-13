package miners

import "encoding/hex"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "testing"

import "database/sql"

import _ "modernc.org/sqlite"
import "bitnsbot/cursors"

// The tables as openDB creates them, from the schema tools/tosqlite defines: a row
// per pool, and a row per address and per tag naming the pool it belongs to.
const ddl = `create table miners (name TEXT PRIMARY KEY, blocks INTEGER NOT NULL,
    reward INTEGER NOT NULL, fees INTEGER NOT NULL, totalWork REAL NOT NULL, lastWork REAL NOT NULL);
    create table mineraddr (address TEXT PRIMARY KEY, name TEXT NOT NULL references miners(name));
    create table minertag (tag TEXT PRIMARY KEY, name TEXT NOT NULL references miners(name));
    create table cursors (name TEXT PRIMARY KEY, place INTEGER NOT NULL)`

func openTestDB(t *testing.T) *sql.DB {
    t.Helper()
    // foreign keys on, as the bot runs them: mineraddr and minertag cannot name a
    // pool the miners table does not have
    var handle, err = sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "miners.db")+"?_pragma=foreign_keys(on)")
    if err != nil { t.Fatalf("open: %v", err) }
    if _, err := handle.Exec(ddl); err != nil { t.Fatal(err) }
    if err := cursors.Init(handle); err != nil { t.Fatalf("cursors: %v", err) }
    if err := Init(handle); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { handle.Close(); db = nil })
    return handle
}

func serve(t *testing.T, payload *string) {
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(*payload))
    }))
    t.Cleanup(srv.Close)
    var saved = sourceURL
    t.Cleanup(func() { sourceURL = saved })
    sourceURL = srv.URL
}

func TestUpdateAndName(t *testing.T) {
    openTestDB(t)
    var payload = `[{"name":"PoolA","addresses":["addrA"]},{"name":"PoolB","addresses":["addrB1","addrB2"]}]`
    serve(t, &payload)
    if !empty() {
        t.Fatal("expected empty bucket before update")
    }
    update()
    if empty() {
        t.Fatal("expected populated bucket after update")
    }
    if got := Name("addrB2"); got != "PoolB" {
        t.Fatalf("addrB2 → %q, want PoolB", got)
    }
    if got := Name("addrA"); got != "PoolA" {
        t.Fatalf("addrA → %q, want PoolA", got)
    }
    // an unknown coinbase address resolves to "" (the caller supplies "Unknown")
    if got := Name("someoneElse"); got != "" {
        t.Fatalf("unknown → %q, want empty", got)
    }
}

func TestUpdateOnlyAdds(t *testing.T) {
    openTestDB(t)
    // a first fetch has addrA; a later fetch drops it and adds addrB — because we
    // never delete, addrA must still resolve while addrB is added.
    var payload = `[{"name":"PoolA","addresses":["addrA"]}]`
    serve(t, &payload)
    update()
    payload = `[{"name":"PoolB","addresses":["addrB"]}]`
    update()
    if got := Name("addrA"); got != "PoolA" {
        t.Fatalf("addrA should survive (no delete): %q", got)
    }
    if got := Name("addrB"); got != "PoolB" {
        t.Fatalf("addrB should be added: %q", got)
    }
}

func TestAttribute(t *testing.T) {
    openTestDB(t)
    var payload = `[{"name":"AntPool","addresses":["3AntAddr"],"tags":["/AntPool/","Mined by AntPool"]},
                    {"name":"Foundry USA","addresses":[],"tags":["Foundry USA Pool"]}]`
    serve(t, &payload)
    update()
    // an address hit wins outright, no tag needed
    if got := Attribute([]string{"3AntAddr"}, ""); got != "AntPool" {
        t.Fatalf("by address = %q, want AntPool", got)
    }
    // the payout address is not always the first coinbase output
    if got := Attribute([]string{"unrelated", "3AntAddr"}, ""); got != "AntPool" {
        t.Fatalf("by later address = %q, want AntPool", got)
    }
    // a rotated (unlisted) payout address still attributes via the coinbase tag —
    // the real mainnet case for Foundry, which has no usable address in the list
    var script = hex.EncodeToString([]byte("\x03abcd/Foundry USA Pool #dropgold/\xfa\x01"))
    if got := Attribute([]string{"bc1qUnlisted"}, script); got != "Foundry USA" {
        t.Fatalf("by tag = %q, want Foundry USA", got)
    }
    // neither address nor tag → unattributed, and the caller decides what to show
    if got := Attribute([]string{"bc1qUnlisted"}, hex.EncodeToString([]byte("nothing here"))); got != "" {
        t.Fatalf("unknown = %q, want empty", got)
    }
    if got := Attribute(nil, "not hex"); got != "" {
        t.Fatalf("bad hex = %q, want empty", got)
    }
    if got := Attribute(nil, ""); got != "" {
        t.Fatalf("no script = %q, want empty", got)
    }
}

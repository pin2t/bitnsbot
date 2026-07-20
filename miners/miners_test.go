package miners

import "net/http"
import "net/http/httptest"
import "path/filepath"
import "testing"

import "go.etcd.io/bbolt"

func openTestDB(t *testing.T) {
    var d, err = bbolt.Open(filepath.Join(t.TempDir(), "miners.db"), 0600, nil)
    if err != nil { t.Fatalf("open: %v", err) }
    if err := Init(d); err != nil { t.Fatalf("init: %v", err) }
    t.Cleanup(func() { d.Close(); db = nil })
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

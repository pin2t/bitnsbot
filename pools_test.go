package main

import "net/http"
import "net/http/httptest"
import "testing"

func TestMinerName(t *testing.T) {
    poolsMu.Lock()
    var savedAddr, savedTags = poolByAddress, poolTags
    poolByAddress = map[string]string{"1FoundryAddr": "Foundry USA"}
    poolTags = []poolTag{{tag: "/AntPool/", name: "AntPool"}}
    poolsMu.Unlock()
    defer func() {
        poolsMu.Lock()
        poolByAddress, poolTags = savedAddr, savedTags
        poolsMu.Unlock()
    }()
    // coinbase output address match wins
    if got := minerName("", []string{"1FoundryAddr"}); got != "Foundry USA" {
        t.Fatalf("address match: %q", got)
    }
    // coinbase-script tag substring match ("/AntPool/" = hex 2f416e74506f6f6c2f)
    if got := minerName("00112f416e74506f6f6c2f99", nil); got != "AntPool" {
        t.Fatalf("tag match: %q", got)
    }
    // nothing matches
    if got := minerName("deadbeef", []string{"1Someone"}); got != "Unknown" {
        t.Fatalf("unknown: %q", got)
    }
}

func TestLoadPools(t *testing.T) {
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Write([]byte(`[{"name":"PoolA","addresses":["addrA"],"tags":["/tagA/"]},{"name":"PoolB","addresses":["addrB1","addrB2"],"tags":[]}]`))
    }))
    defer srv.Close()
    poolsMu.Lock()
    var savedAddr, savedTags = poolByAddress, poolTags
    poolsMu.Unlock()
    var savedURL = poolsURL
    defer func() {
        poolsMu.Lock()
        poolByAddress, poolTags = savedAddr, savedTags
        poolsMu.Unlock()
        poolsURL = savedURL
    }()
    poolsURL = srv.URL
    if err := loadPools(); err != nil {
        t.Fatalf("loadPools: %v", err)
    }
    if got := minerName("", []string{"addrB2"}); got != "PoolB" {
        t.Fatalf("addrB2 → %q, want PoolB", got)
    }
    if got := minerName("2f746167412f", nil); got != "PoolA" { // "/tagA/"
        t.Fatalf("tagA → %q, want PoolA", got)
    }
}

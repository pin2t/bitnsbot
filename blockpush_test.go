package main

import "sync"
import "bitnsbot/app"
import "testing"

// fakeChain answers the three calls the Blockchain card makes, counting each so
// a test can assert which ones a refresh actually issued.
func fakeChain(t *testing.T, height int64) (*coreConn, func(string) int) {
    var mu sync.Mutex
    var calls = map[string]int{}
    var server = newFakeCoreServer(t, func(method string, params []interface{}) (interface{}, error) {
        mu.Lock()
        calls[method]++
        mu.Unlock()
        switch method {
        case "getblockchaininfo":
            return map[string]interface{}{"blocks": height, "size_on_disk": 869000000000}, nil
        case "getchaintxstats":
            return map[string]interface{}{"txcount": 1421109968}, nil
        case "getnodeaddresses":
            return []interface{}{
                map[string]interface{}{"time": 1e12, "network": "ipv4"},
                map[string]interface{}{"time": 1e12, "network": "ipv4"},
            }, nil
        }
        return nil, nil
    })
    t.Cleanup(server.Close)
    var conn, err = newCoreConn(server.URL, "testuser", "testpass", "")
    if err != nil { t.Fatalf("core conn: %v", err) }
    return conn, func(m string) int { mu.Lock(); defer mu.Unlock(); return calls[m] }
}

// A block must not trigger the peer scan: getnodeaddresses returns tens of
// thousands of entries and several megabytes, and the count barely moves
// between blocks. The ticker owns it.
func TestBlockRefreshSkipsNodeScan(t *testing.T) {
    var conn, count = fakeChain(t, 963400)
    core = conn
    defer func() { core = nil }()
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true, Nodes: "31 751"}
    networkMu.Unlock()
    refreshNetwork(false)
    if n := count("getnodeaddresses"); n != 0 {
        t.Errorf("a block refresh called getnodeaddresses %d times; it is the expensive one", n)
    }
    for _, m := range []string{"getblockchaininfo", "getchaintxstats"} {
        if count(m) != 1 {
            t.Errorf("%s called %d times, want 1", m, count(m))
        }
    }
    networkMu.Lock()
    var got = cachedNetwork
    networkMu.Unlock()
    if got.Nodes != "31 751" {
        t.Errorf("peer count = %q, want the previous 31 751 carried forward", got.Nodes)
    }
    if got.Blocks != "963 400" {
        t.Errorf("height = %q, want 963 400 — the block should have moved it", got.Blocks)
    }
}

// The ticker does the full refresh, peer scan included.
func TestTickerRefreshScansNodes(t *testing.T) {
    var conn, count = fakeChain(t, 963400)
    core = conn
    defer func() { core = nil }()
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true, Nodes: ""}
    networkMu.Unlock()
    refreshNetwork(true)
    if count("getnodeaddresses") != 1 {
        t.Fatalf("the ticker refresh must scan peers; called %d times", count("getnodeaddresses"))
    }
    networkMu.Lock()
    var got = cachedNetwork
    networkMu.Unlock()
    if got.Nodes != "2" {
        t.Errorf("peer count = %q, want 2 from the scan", got.Nodes)
    }
}

// With no previous count and no scan, the field must not read "0" — that would
// claim the network has no peers.
func TestBlockRefreshNeverReportsZeroNodes(t *testing.T) {
    var conn, _ = fakeChain(t, 963400)
    core = conn
    defer func() { core = nil }()
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true, Nodes: ""}
    networkMu.Unlock()
    refreshNetwork(false)
    networkMu.Lock()
    var got = cachedNetwork
    networkMu.Unlock()
    if got.Nodes == "0" {
        t.Fatal(`peer count rendered "0", which claims the network has no peers`)
    }
    if got.Nodes != "—" {
        t.Errorf("peer count = %q, want the em dash placeholder", got.Nodes)
    }
}

package main

import "net/http"
import "net/http/httptest"
import "sync"
import "testing"
import "time"
import "bitnsbot/app"
import "bitnsbot/core/coretest"

// fakeChain points core at a node answering the three calls the Blockchain card
// makes, counting each so a test can assert which ones a refresh actually issued.
func fakeChain(t *testing.T, height int64) func(string) int {
    var mu sync.Mutex
    var calls = map[string]int{}
    coretest.Start(t, func(method string, params []interface{}) (interface{}, error) {
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
    return func(m string) int { mu.Lock(); defer mu.Unlock(); return calls[m] }
}

// fakeBtcnodes points the refresh at a local server answering the btcnodes.io
// count endpoint with the given status and body, restoring the real URL after.
// It returns a hit counter so a test can assert whether a fetch happened.
func fakeBtcnodes(t *testing.T, status int, body string) func() int {
    var mu sync.Mutex
    var hits int
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        mu.Lock()
        hits++
        mu.Unlock()
        w.WriteHeader(status)
        w.Write([]byte(body))
    }))
    t.Cleanup(srv.Close)
    var saved = btcnodesURL
    t.Cleanup(func() { btcnodesURL = saved })
    btcnodesURL = srv.URL
    return func() int { mu.Lock(); defer mu.Unlock(); return hits }
}

// resetBtcnodes clears the cached btcnodes count so each test starts cold.
func resetBtcnodes() {
    btcnodesCount = 0
    btcnodesSeen = time.Time{}
    btcnodesNext = time.Time{}
}

// A block must not trigger the peer scan: getnodeaddresses returns tens of
// thousands of entries and several megabytes, and the count barely moves
// between blocks. The ticker owns it.
func TestBlockRefreshSkipsNodeScan(t *testing.T) {
    var count = fakeChain(t, 963400)
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

// The ticker prefers btcnodes.io's crawl over the node's own address manager:
// when the API answers, the count comes from it and the expensive
// getnodeaddresses scan is skipped entirely.
func TestTickerRefreshUsesBtcnodes(t *testing.T) {
    resetBtcnodes()
    var count = fakeChain(t, 963400)
    fakeBtcnodes(t, http.StatusOK, `{"count":26105,"results":[]}`)
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true, Nodes: ""}
    networkMu.Unlock()
    refreshNetwork(true)
    if n := count("getnodeaddresses"); n != 0 {
        t.Fatalf("btcnodes answered, yet getnodeaddresses was still called %d times", n)
    }
    networkMu.Lock()
    var got = cachedNetwork
    networkMu.Unlock()
    if got.Nodes != "26 105" {
        t.Errorf("peer count = %q, want 26 105 from btcnodes.io", got.Nodes)
    }
}

// A successful count younger than btcnodesTTL is reused without asking the API
// again — that is what keeps it to four requests a day.
func TestTickerRefreshReusesCachedBtcnodes(t *testing.T) {
    resetBtcnodes()
    var count = fakeChain(t, 963400)
    var hits = fakeBtcnodes(t, http.StatusOK, `{"count":99999}`)
    btcnodesCount, btcnodesSeen = 30000, time.Now()
    btcnodesNext = time.Now().Add(time.Hour)
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true, Nodes: ""}
    networkMu.Unlock()
    refreshNetwork(true)
    if hits() != 0 {
        t.Fatalf("a fresh count was reused, yet btcnodes.io was fetched %d times", hits())
    }
    if n := count("getnodeaddresses"); n != 0 {
        t.Fatalf("a fresh count was reused, yet getnodeaddresses was still called %d times", n)
    }
    networkMu.Lock()
    var got = cachedNetwork
    networkMu.Unlock()
    if got.Nodes != "30 000" {
        t.Errorf("peer count = %q, want the cached 30 000", got.Nodes)
    }
}

// A fetch that fails while the cached count is still under two days old keeps
// serving that count — no fallback to the node's scan yet.
func TestTickerRefreshKeepsCachedWhenYoung(t *testing.T) {
    resetBtcnodes()
    var count = fakeChain(t, 963400)
    fakeBtcnodes(t, http.StatusTooManyRequests, `rate limited`)
    btcnodesCount, btcnodesSeen = 30000, time.Now().Add(-time.Hour)
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true, Nodes: ""}
    networkMu.Unlock()
    refreshNetwork(true)
    if n := count("getnodeaddresses"); n != 0 {
        t.Fatalf("a day-old count should be trusted, yet getnodeaddresses was called %d times", n)
    }
    networkMu.Lock()
    var got = cachedNetwork
    networkMu.Unlock()
    if got.Nodes != "30 000" {
        t.Errorf("peer count = %q, want the cached 30 000", got.Nodes)
    }
}

// Only once the cached count is more than two days old does a failed fetch fall
// back to the node's own scan.
func TestTickerRefreshStaleBtcnodesFallsBack(t *testing.T) {
    resetBtcnodes()
    var count = fakeChain(t, 963400)
    fakeBtcnodes(t, http.StatusTooManyRequests, `rate limited`)
    btcnodesCount, btcnodesSeen = 30000, time.Now().Add(-3*24*time.Hour)
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true, Nodes: ""}
    networkMu.Unlock()
    refreshNetwork(true)
    if count("getnodeaddresses") != 1 {
        t.Fatalf("a three-day-old count must fall back to the scan; called %d times", count("getnodeaddresses"))
    }
    networkMu.Lock()
    var got = cachedNetwork
    networkMu.Unlock()
    if got.Nodes != "2" {
        t.Errorf("peer count = %q, want 2 from the scan", got.Nodes)
    }
}

// With no cached count at all and the API failing, the scan is the fallback.
func TestTickerRefreshFallsBackToNodeScan(t *testing.T) {
    resetBtcnodes()
    var count = fakeChain(t, 963400)
    fakeBtcnodes(t, http.StatusTooManyRequests, `rate limited`)
    networkMu.Lock()
    cachedNetwork = app.Network{OK: true, Nodes: ""}
    networkMu.Unlock()
    refreshNetwork(true)
    if count("getnodeaddresses") != 1 {
        t.Fatalf("the ticker refresh must scan peers after btcnodes failed; called %d times", count("getnodeaddresses"))
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
    fakeChain(t, 963400)
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

package main

import "encoding/json"
import "net/http"
import "net/http/httptest"
import "strings"
import "testing"

// appHandler rebuilds the mux startApp serves, without binding a port.
func appHandler() http.Handler {
    var mux = http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" {
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Write(appHTML)
    })
    mux.HandleFunc("/api/fees", appFees)
    return mux
}

func TestAppServesPage(t *testing.T) {
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
    if w.Code != 200 {
        t.Fatalf("GET / = %d, want 200", w.Code)
    }
    var body = w.Body.String()
    for _, want := range []string{"telegram-web-app.js", `data-panel="home"`, `data-panel="blocks"`,
        `data-panel="addresses"`, `data-panel="miners"`, `id="q"`, "Network fees"} {
        if !strings.Contains(body, want) {
            t.Errorf("page is missing %q", want)
        }
    }
}

// Anything other than the single page is a 404 rather than the page itself, so a
// mistyped path doesn't silently render the app.
func TestAppRejectsOtherPaths(t *testing.T) {
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
    if w.Code != 404 {
        t.Fatalf("GET /nope = %d, want 404", w.Code)
    }
}

// A cold cache must report unavailable rather than serving zeroed rates as if
// they were real fee estimates.
func TestAppFeesColdCache(t *testing.T) {
    feesMu.Lock()
    cachedFeesOK = false
    feesMu.Unlock()
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, httptest.NewRequest("GET", "/api/fees", nil))
    var got struct {
        OK   bool            `json:"ok"`
        Fast json.RawMessage `json:"fast"`
    }
    json.NewDecoder(w.Body).Decode(&got)
    if got.OK || got.Fast != nil {
        t.Fatalf("cold cache returned ok=%v fast=%s, want ok=false and no tiers", got.OK, got.Fast)
    }
}

// The three tiers are the ones /fees prints — fastest, hour and minimum — and
// they must come from the cache rather than a live mempool read.
func TestAppFeesServesCachedTiers(t *testing.T) {
    feesMu.Lock()
    cachedFees = recommendedFees{fastest: 12, halfHour: 8, hour: 4, economy: 2, minimum: 1}
    cachedFeesOK, cachedFeesCount = true, 36552
    feesMu.Unlock()
    defer func() { feesMu.Lock(); cachedFeesOK = false; feesMu.Unlock() }()
    var w = httptest.NewRecorder()
    appHandler().ServeHTTP(w, httptest.NewRequest("GET", "/api/fees", nil))
    var got struct {
        OK   bool              `json:"ok"`
        Txs  string            `json:"txs"`
        Fast map[string]string `json:"fast"`
        Hour map[string]string `json:"hour"`
        Slow map[string]string `json:"slow"`
    }
    if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
        t.Fatalf("decode: %v", err)
    }
    if !got.OK || got.Fast["rate"] != "12" || got.Hour["rate"] != "4" || got.Slow["rate"] != "1" {
        t.Fatalf("tiers = fast %q hour %q slow %q, want 12/4/1", got.Fast["rate"], got.Hour["rate"], got.Slow["rate"])
    }
    if got.Txs != "36 552" {
        t.Errorf("tx count = %q, want space-grouped 36 552", got.Txs)
    }
}

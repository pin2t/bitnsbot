package main

import _ "embed"
import "encoding/json"
import "math"
import "net/http"
import "bitnsbot/logging"
import "bitnsbot/rates"

//go:embed app.html
var appHTML []byte

// startApp serves the Telegram Mini App on addr and returns the server so
// shutdown can drain it. Bind to localhost: the page reaches the outside world
// through the Cloudflare tunnel, which is what faces the network — nothing here
// should be exposed directly.
func startApp(addr string) *http.Server {
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
    var srv = &http.Server{Addr: addr, Handler: mux}
    go func() {
        logging.Status("mini app listening on %s", addr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logging.Err("mini app: %v", err)
        }
    }()
    return srv
}

// appFees serves the same three tiers /fees prints, read from the background
// cache rather than the mempool, so opening the app costs no node round trip.
func appFees(w http.ResponseWriter, r *http.Request) {
    feesMu.Lock()
    var rec, ok, count = cachedFees, cachedFeesOK, cachedFeesCount
    feesMu.Unlock()
    var out = map[string]any{"ok": ok, "txs": group(int64(count))}
    if ok {
        var price, havePrice = rates.Last()
        var tier = func(rate float64) map[string]string {
            var m = map[string]string{"rate": trimNum(rate, 2)}
            if havePrice { m["usd"] = usd(int64(math.Round(rate*typicalTxVsize)), price) }
            return m
        }
        out["fast"] = tier(rec.fastest)
        out["hour"] = tier(rec.hour)
        out["slow"] = tier(rec.minimum)
    }
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(out)
}

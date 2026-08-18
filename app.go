package main

import _ "embed"
import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "fmt"
import "math"
import "net/http"
import "net/url"
import "sort"
import "strconv"
import "strings"
import "time"
import "bitnsbot/logging"
import "bitnsbot/rates"

//go:embed app.html
var appHTML []byte

//go:embed htmx.min.js
var htmxJS []byte

// initDataTTL bounds how old Telegram's signed payload may be. A valid
// signature is forever valid without it, so a payload lifted from a log or a
// shared URL would keep working; a var so tests can shrink it.
var initDataTTL = 24 * time.Hour

// startApp serves the Telegram Mini App on addr and returns the server so
// shutdown can drain it. Bind to localhost: the page reaches the outside world
// through the Cloudflare tunnel, which is what faces the network — nothing here
// should be exposed directly.
//
// The shape is HTMX's: the server renders HTML, the page swaps it in. There is
// no JSON API and no client-side rendering, which suits a UI that is mostly
// server-side data in tables — and lets the Go formatters the bot already has
// (trimNum, usd, group) produce exactly what the page displays.
func startApp(addr string) *http.Server {
    var srv = &http.Server{Addr: addr, Handler: appHandler()}
    go func() {
        logging.Status("mini app listening on %s", addr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logging.Err("mini app: %v", err)
        }
    }()
    return srv
}

// appHandler builds the routes, separately from binding a port, so tests drive
// the same mux the server serves rather than a copy that can drift.
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
    mux.HandleFunc("/htmx.min.js", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
        w.Header().Set("Cache-Control", "public, max-age=86400")
        w.Write(htmxJS)
    })
    mux.HandleFunc("/fees", requireInitData(appFees))
    return mux
}

// checkInitData verifies the signed payload Telegram hands the Mini App's
// webview, and is the only thing standing between this server and anyone who
// knows the URL. The scheme: the secret is HMAC-SHA256 of the bot token keyed by
// the literal "WebAppData", and the signature covers every field except `hash`,
// sorted by key and joined with newlines.
//
// Note this is NOT the Login Widget scheme, which keys the secret as
// SHA256(token) — the two look interchangeable and are not.
func checkInitData(initData, token string) (url.Values, bool) {
    var v, err = url.ParseQuery(initData)
    if err != nil { return nil, false }
    var want = v.Get("hash")
    if want == "" { return nil, false }
    v.Del("hash")
    var keys = make([]string, 0, len(v))
    for k := range v { keys = append(keys, k) }
    sort.Strings(keys)
    var pairs = make([]string, 0, len(keys))
    for _, k := range keys { pairs = append(pairs, k+"="+v.Get(k)) }
    var mac = hmac.New(sha256.New, []byte("WebAppData"))
    mac.Write([]byte(token))
    var secret = mac.Sum(nil)
    mac = hmac.New(sha256.New, secret)
    mac.Write([]byte(strings.Join(pairs, "\n")))
    if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(want)) { return nil, false }
    var ts, terr = strconv.ParseInt(v.Get("auth_date"), 10, 64)
    if terr != nil || time.Since(time.Unix(ts, 0)) > initDataTTL { return nil, false }
    return v, true
}

// requireInitData rejects anything without a currently-valid signature. The
// shell page at / is deliberately not behind this: it carries no data, and the
// webview must load it before any script can read initData at all.
func requireInitData(h http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if _, ok := checkInitData(r.Header.Get("X-Telegram-Init-Data"), *botToken); !ok {
            logging.Info("mini app: rejected %s without valid initData", r.URL.Path)
            http.Error(w, "open this from Telegram", http.StatusUnauthorized)
            return
        }
        h(w, r)
    }
}

// appFees renders the fees card's contents as HTML for HTMX to swap in. The
// three tiers are the ones /fees prints, read from the cachedFees background
// cache rather than the mempool, so opening the app costs no node round trip.
func appFees(w http.ResponseWriter, r *http.Request) {
    feesMu.Lock()
    var rec, ok, count = cachedFees, cachedFeesOK, cachedFeesCount
    feesMu.Unlock()
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    if !ok {
        fmt.Fprint(w, `<h2>Network fees</h2><p class="note">fees unavailable</p>`)
        return
    }
    var price, havePrice = rates.Last()
    var tier = func(label string, rate float64) string {
        var usdCell string
        if havePrice { usdCell = usd(int64(math.Round(rate*typicalTxVsize)), price) }
        return fmt.Sprintf(`<div class="tier"><div class="label">%s</div>`+
            `<div class="rate">%s <span class="unit">sat/vB</span></div>`+
            `<div class="usd">%s</div></div>`, label, trimNum(rate, 2), usdCell)
    }
    fmt.Fprintf(w, `<h2>Network fees</h2><div class="tiers">%s%s%s</div>`+
        `<p class="note">projected from %s mempool transactions</p>`,
        tier("Fast", rec.fastest), tier("1 hour", rec.hour), tier("2+ hours", rec.minimum),
        group(int64(count)))
}

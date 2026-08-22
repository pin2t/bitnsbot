// Package app serves the Telegram Mini App — a web page Telegram opens in its
// in-app webview. It renders HTML fragments that HTMX swaps into the page (no
// JSON, no client-side templating), and validates the signed initData Telegram
// hands the webview so the data endpoints are not open to anyone who knows the
// URL.
//
// It cannot reach package main's fee cache or formatters, so the chain data it
// needs arrives through the small Source interface, implemented in main — the
// same seam the miners package uses for its own collector.
package app

import _ "embed"
import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "html/template"
import "net/http"
import "net/url"
import "sort"
import "strconv"
import "strings"
import "time"
import "bitnsbot/logging"

//go:embed app.html
var appHTML []byte

//go:embed htmx.min.js
var htmxJS []byte

// appTmpl is the page and, inside it, the "fees" and "network" blocks. The
// initial render and each refresh execute those same blocks, so the card the
// page ships with and the card that replaces it cannot drift apart.
var appTmpl = template.Must(template.New("app").Parse(string(appHTML)))

// page is what the whole template renders from: one field per card.
type page struct {
    Fees    Fees
    Network Network
}

// initDataTTL bounds how old Telegram's signed payload may be. A valid
// signature is forever valid without it, so a payload lifted from a log or a
// shared URL would keep working; a var so tests can shrink it.
var initDataTTL = 24 * time.Hour

// Tier is one fee estimate, already formatted for display: the sat/vB rate and
// the USD cost of a typical transaction (empty when no price is known).
type Tier struct {
    Rate string
    USD  string
}

// Fees is the fee card's data. OK is false when the background cache is cold, in
// which case the tiers are ignored and the card says so.
type Fees struct {
    OK      bool
    Fast    Tier
    Hour    Tier
    Slow    Tier
    TxCount string
}

// Network is the chain-at-a-glance card: circulating supply against the 21M cap,
// chain height, on-disk size and how many peers the node has heard from
// recently. Every field is already formatted; OK is false while the background
// cache is still cold.
type Network struct {
    OK        bool
    Coins     string
    Cap       string
    Blocks    string
    Size      string
    Nodes     string
    Txs       string
    Addresses string
}

// Source supplies the chain data the app renders. main implements it; the app
// package stays unaware of the fee cache, Bitcoin Core and the price feeds.
type Source interface {
    Fees() Fees
    Network() Network
}

// Start serves the Mini App on addr and returns the server so the caller can
// drain it before closing anything it depends on. token is the bot token, used
// only to verify initData. Bind addr to localhost: the page reaches the outside
// world through the Cloudflare tunnel, which is what faces the network — nothing
// here should be exposed directly.
func Start(addr, token string, src Source) *http.Server {
    var mux = http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" {
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        if err := appTmpl.Execute(w, page{Fees: src.Fees(), Network: src.Network()}); err != nil {
            logging.Err("mini app: render page: %v", err)
        }
    })
    mux.HandleFunc("/htmx.min.js", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
        w.Header().Set("Cache-Control", "public, max-age=86400")
        w.Write(htmxJS)
    })
    mux.HandleFunc("/fees", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        render(w, "fees", src.Fees())
    }))
    mux.HandleFunc("/network", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        render(w, "network", src.Network())
    }))
    var srv = &http.Server{Addr: addr, Handler: mux}
    go func() {
        logging.Status("mini app listening on %s", addr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logging.Err("mini app: %v", err)
        }
    }()
    return srv
}

// isValid verifies the signed payload Telegram hands the Mini App's
// webview, and is the only thing standing between this server and anyone who
// knows the URL. The scheme: the secret is HMAC-SHA256 of the bot token keyed by
// the literal "WebAppData", and the signature covers every field except `hash`,
// sorted by key and joined with newlines.
//
// Note this is NOT the Login Widget scheme, which keys the secret as
// SHA256(token) — the two look interchangeable and are not.
func isValid(initData, token string) bool {
    var v, err = url.ParseQuery(initData)
    if err != nil { return false }
    var want = v.Get("hash")
    if want == "" { return false }
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
    if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(want)) { return false }
    var ts, terr = strconv.ParseInt(v.Get("auth_date"), 10, 64)
    if terr != nil || time.Since(time.Unix(ts, 0)) > initDataTTL { return false }
    return true
}

// requireInitData rejects anything without a currently-valid signature. The
// shell page at / is deliberately not behind this: it carries no data, and the
// webview must load it before any script can read initData at all.
func requireInitData(token string, h http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if !isValid(r.Header.Get("X-Telegram-Init-Data"), token) {
            logging.Info("mini app: rejected %s without valid initData", r.URL.Path)
            http.Error(w, "open this from Telegram", http.StatusUnauthorized)
            return
        }
        h(w, r)
    }
}

// render writes one card's block, for the periodic refresh to swap in. The page
// ships the same blocks already rendered, so this is only ever an update — never
// the first paint.
func render(w http.ResponseWriter, block string, data any) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    if err := appTmpl.ExecuteTemplate(w, block, data); err != nil {
        logging.Err("mini app: render %s: %v", block, err)
    }
}

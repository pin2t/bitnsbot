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
import "bytes"
import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "fmt"
import "html/template"
import "net/http"
import "net/url"
import "sort"
import "sync"
import "strconv"
import "strings"
import "time"
import "bitnsbot/logging"
import "bitnsbot/lru"

//go:embed app.html
var appHTML []byte

//go:embed htmx.min.js
var htmxJS []byte

//go:embed htmx-ext-sse.js
var sseJS []byte

// appTmpl is the page and, inside it, the "fees" and "network" blocks. The
// initial render and each refresh execute those same blocks, so the card the
// page ships with and the card that replaces it cannot drift apart.
var appTmpl = template.Must(template.New("app").Parse(string(appHTML)))

// page is what the whole template renders from: one field per card.
type page struct {
    Fees    Fees
    Network Network
    Market  Market
    Blocks  Blocks
}

// cacheTTL is the longest a rendered card is served from memory. The expiry
// belongs to the entry, checked when it is next read — nothing sweeps on a
// timer, so there is no goroutine to start and stop alongside the server. It
// backstops the event-driven clearing below: the block collector, for one, fills
// the blocks bucket without announcing it.
var cacheTTL = 10 * time.Minute

// blocksCached is how many pages of the block list are kept. A reader pages down
// a few screens and stops, so twenty covers the depth anyone reaches, while
// bounding what an edited page= parameter can make the process hold.
const blocksCached = 20

// cardsCached is exactly the number of single-valued renders — the three cardsCache
// plus the shell page — so nothing is ever evicted for space. This cache is
// keyed and expiring, not bounded; blocksCache is the one that needs the bound.
const cardsCached = 4

var cacheMu sync.Mutex

// cardsCache holds the single-valued renders, keyed by the template block each comes
// from. "app" is the whole shell page: one copy serves everyone, which is only
// sound because / carries no per-user data — the rule that already keeps it
// servable without a signature at all. Anything per-chat must stay behind the
// requireInitData endpoints, and caching here is a second reason why.
var cardsCache = lru.New[string, []byte](cardsCached)
var blocksCache = lru.New[string, []byte](blocksCached)

// invalidate drops one card's rendered HTML. Notify calls it *before* announcing
// the event, so a page reacting immediately cannot be handed the very copy it
// was told to replace.
func invalidate(event string) {
    cacheMu.Lock()
    defer cacheMu.Unlock()
    switch event {
    case "fees", "network", "market":
        cardsCache.Delete("/" + event)
    case "blocks":
        blocksCache.Clear()
        cardsCache.Clear()
    default:
        return
    }
    // The page embeds every card, so whichever one moved, the page it would
    // serve to the next visitor is stale.
    cardsCache.Delete("/")
}

// resetCache empties every cache, for tests that share this package state.
func resetCache() {
    cacheMu.Lock()
    cardsCache.Clear()
    blocksCache.Clear()
    cacheMu.Unlock()
}

// serve writes a cached render, producing it only on a miss. data is called
// outside cacheMu because it reaches into main's Source, which holds locks of
// its own; two concurrent misses simply render the same bytes twice, which is
// cheaper than serialising every request behind one mutex.
func serveCached(c *lru.Cache[string, []byte], w http.ResponseWriter, r *http.Request, render func() []byte) {
    cacheMu.Lock()
    var b, hit = c.Get(r.RequestURI)
    cacheMu.Unlock()
    logging.Info("serving cached request for %s: hit = %v", r.RequestURI, hit)
    if !hit {
        b = render()
        if b == nil {
            http.Error(w, "internal server error", http.StatusInternalServerError)
            return
        }
        cacheMu.Lock()
        c.PutTTL(r.RequestURI, b, cacheTTL)
        cacheMu.Unlock()
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Write(b)
}

func execute(name string, data any) []byte {
    var buf bytes.Buffer
    var err = appTmpl.ExecuteTemplate(&buf, name, data)
    if err != nil {
        logging.Err("mini app: render %s: %v", name, err)
        return nil
    }
    return buf.Bytes()
}

// keepAlive is how often the event stream emits a comment line. An idle SSE
// connection is dropped by proxies — Cloudflare's own idle timeout is well under
// the gap between two cache refreshes — so without this the stream would die
// between updates and rely on the browser reconnecting.
var keepAlive = 30 * time.Second

var subsMu sync.Mutex
var subs = map[chan string]struct{}{}

// Notify tells every connected page that a card's data changed. main calls it
// when a cache is actually refreshed, so the page updates when the numbers move
// rather than on a timer that mostly re-fetches the same values.
//
// The rendered card is dropped before the event goes out, or a page reacting to
// it would race the invalidation and be handed the copy it was told to replace.
//
// Sends are non-blocking: a slow or dead client must not stall the caller, which
// is a background refresh goroutine.
func Notify(event string) {
    invalidate(event)
    subsMu.Lock()
    defer subsMu.Unlock()
    for ch := range subs {
        select {
        case ch <- event:
        default:
        }
    }
}

// subscriberCount reports how many streams are connected; used by the tests to
// wait for a handler to register rather than sleeping a fixed time.
func subscriberCount() int {
    subsMu.Lock()
    defer subsMu.Unlock()
    return len(subs)
}

func subscribe() chan string {
    var ch = make(chan string, 4)
    subsMu.Lock()
    subs[ch] = struct{}{}
    subsMu.Unlock()
    return ch
}

func unsubscribe(ch chan string) {
    subsMu.Lock()
    delete(subs, ch)
    subsMu.Unlock()
}

// events is the SSE stream. It carries only event *names* — never data — which
// is what lets it sit outside requireInitData: EventSource cannot set custom
// headers, so a stream that needed the signature could not be opened at all. The
// cardsCache react by issuing their ordinary hx-get, and those still go through
// requireInitData with the header attached.
func events(w http.ResponseWriter, r *http.Request) {
    var rc = http.NewResponseController(w)
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    w.Header().Set("X-Accel-Buffering", "no")
    w.WriteHeader(http.StatusOK)
    if err := rc.Flush(); err != nil {
        logging.Err("mini app: event stream needs a flushable writer: %v", err)
        return
    }
    var ch = subscribe()
    defer unsubscribe(ch)
    var t = time.NewTicker(keepAlive)
    defer t.Stop()
    for {
        select {
        case <-r.Context().Done():
            return
        case name := <-ch:
            fmt.Fprintf(w, "event: %s\ndata: 1\n\n", name)
            if rc.Flush() != nil { return }
        case <-t.C:
            fmt.Fprint(w, ": keepalive\n\n")
            if rc.Flush() != nil { return }
        }
    }
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

// Change is one period's price move, already formatted: a short label, the
// signed percentage, and which way it went so the template can colour it. Up is
// meaningless when Pct is the placeholder — see Neutral.
type Change struct {
    Label   string
    Pct     string
    Up      bool
    Neutral bool
}

// Market is the price card: the current rate and how it has moved over each
// period. Changes always has one entry per period, so a gap in the rate history
// leaves a placeholder rather than collapsing the columns.
type Market struct {
    OK      bool
    Price   string
    Changes []Change
}

// Block is one row of the Blocks tab, already formatted: height, size,
// transaction count and the pool that mined it.
type Block struct {
    Height string
    Size   string
    Txs    string
    Miner  string
}

// Blocks is one page of the recent-block list, newest first. Prev and Next carry
// the page numbers the buttons link to, so the template does no arithmetic.
type Blocks struct {
    OK   bool
    Rows []Block
    // Page is the zero-based index the URL uses; Num is the same page as a
    // reader counts them, from 1. Keeping both means the URL contract does not
    // change just because the label does.
    Page    int
    Num     int
    Prev    int
    Next    int
    HasPrev bool
    HasNext bool
}

// Source supplies the chain data the app renders. main implements it; the app
// package stays unaware of the fee cache, Bitcoin Core and the price feeds.
type Source interface {
    Fees() Fees
    Network() Network
    Market() Market
    Blocks(page int) Blocks
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
        serveCached(cardsCache, w, r, func() []byte {
            return execute("app", page{Fees: src.Fees(), Network: src.Network(), Market: src.Market(), Blocks: src.Blocks(0)})
        })
    })
    mux.HandleFunc("/htmx.min.js", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
        w.Header().Set("Cache-Control", "public, max-age=86400")
        w.Write(htmxJS)
    })
    mux.HandleFunc("/htmx-ext-sse.js", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
        w.Header().Set("Cache-Control", "public, max-age=86400")
        w.Write(sseJS)
    })
    mux.HandleFunc("/events", events)
    mux.HandleFunc("/fees", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        serveCached(cardsCache, w, r, func() []byte { return  execute("fees", src.Fees()) })
    }))
    mux.HandleFunc("/network", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        serveCached(cardsCache, w, r, func() []byte { return  execute("network", src.Network()) })
    }))
    mux.HandleFunc("/market", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        serveCached(cardsCache, w, r, func() []byte { return  execute("market", src.Market()) })
    }))
    mux.HandleFunc("/blocks", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var page, err = strconv.Atoi(r.URL.Query().Get("page"))
        if err != nil || page < 0 { page = 0 }
        serveCached(blocksCache, w, r, func() []byte { return  execute("blocks", src.Blocks(page)) })
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

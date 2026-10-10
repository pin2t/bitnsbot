package max

import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "encoding/json"
import "fmt"
import "net/http"
import "net/url"
import "sort"
import "strconv"
import "strings"
import "sync"
import "time"
import _ "embed"
import "bitnsbot/app"
import "bitnsbot/logging"
import "bitnsbot/lru"

// page is the whole MAX Mini App: one HTML file whose script renders every tab
// in the browser with React and MAX UI, both loaded from a CDN, and fetches its
// data from the JSON endpoints below. It carries no data of its own, so it is
// served to anyone without a signature — the webview has to load it before any
// script can read the launch data at all.
//
//go:embed app.html
var page []byte

// lang is the one language the MAX Mini App speaks. The page's own words are
// Russian, and every Options function that renders words is asked in it too, so
// a details row reads the same as the labels around it.
const lang = "ru"

// initHeader carries the launch data MAX signs — window.WebApp.initData — on
// every request the page makes, the way X-Telegram-Init-Data does in the
// Telegram Mini App.
const initHeader = "X-Max-Init-Data"

// initDataTTL bounds how old MAX's signed launch data may be. A valid signature
// is otherwise valid forever, so a payload lifted from a log would keep
// working; a var so tests can shrink it.
var initDataTTL = 24 * time.Hour

// keepAlive is how often the event stream emits a comment line, so a proxy does
// not drop the connection between two cache refreshes.
var keepAlive = 30 * time.Second

// cacheTTL is the longest an answer is served from memory, the Telegram Mini
// App's figure. Notify clears what an event makes stale sooner; this covers
// what moves without one.
var cacheTTL = 10 * time.Minute

// cached bounds how many answers are kept: the three cards, batches of the two
// lists as a reader scrolls, and the details pages opened from them.
const cached = 64

var cacheMu sync.Mutex

// cache holds marshalled answers keyed by request URI. Every answer in it is the
// same for every reader — the page has one language and nothing per-user goes
// in here — so one copy serves everyone. The watch list, a watch's state and
// the mempool are never cached: the first two are per-user, the last moves
// every few seconds.
var cache = lru.New[string, []byte](cached)

var subsMu sync.Mutex
var subs = map[chan string]struct{}{}

// Notify tells every open page that a card's data changed, so it fetches the new
// figures. main has it called for every event the Telegram Mini App is told
// of, after that app's own throttling, so the two show the same changes at the
// same time.
//
// What the event makes stale is dropped before it goes out, or a page reacting
// to it would be handed the copy it was told to replace. A new block moves the
// block list, every details page and the miner charts, so it clears everything.
//
// Sends are non-blocking: a slow page must not stall the caller.
func Notify(event string) {
    cacheMu.Lock()
    switch event {
    case "fees", "network", "market":
        cache.Delete("/api/" + event)
    case "blocks":
        cache.Clear()
    }
    cacheMu.Unlock()
    subsMu.Lock()
    defer subsMu.Unlock()
    for ch := range subs {
        select {
        case ch <- event:
        default:
        }
    }
}

// events is the event stream. It carries event names only — never data — which
// is what lets it go without a signature: EventSource cannot set a header. The
// page answers an event with an ordinary signed fetch.
//
// closing is what ends the stream when the server is shutting down.
func events(w http.ResponseWriter, r *http.Request, closing <-chan struct{}) {
    var rc = http.NewResponseController(w)
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("X-Accel-Buffering", "no")
    w.WriteHeader(http.StatusOK)
    if err := rc.Flush(); err != nil {
        logging.Err("max app: event stream needs a flushable writer: %v", err)
        return
    }
    var ch = make(chan string, 4)
    subsMu.Lock()
    subs[ch] = struct{}{}
    subsMu.Unlock()
    defer func() {
        subsMu.Lock()
        delete(subs, ch)
        subsMu.Unlock()
    }()
    var t = time.NewTicker(keepAlive)
    defer t.Stop()
    for {
        select {
        case <-r.Context().Done(): return
        case <-closing:            return
        case name := <-ch:
            fmt.Fprintf(w, "event: %s\ndata: 1\n\n", name)
            if rc.Flush() != nil { return }
        case <-t.C:
            fmt.Fprint(w, ": keepalive\n\n")
            if rc.Flush() != nil { return }
        }
    }
}

// serve answers with load's result as JSON, from the cache when it holds a fresh
// copy. load runs outside cacheMu, since it reaches into main's Options
// functions and their locks; two concurrent misses just marshal twice.
func serve(w http.ResponseWriter, r *http.Request, load func() any) {
    var started = time.Now()
    var key = r.URL.RequestURI()
    cacheMu.Lock()
    var b, hit = cache.Get(key)
    cacheMu.Unlock()
    if !hit {
        var err error
        b, err = json.Marshal(load())
        if err != nil {
            logging.Err("max app: encode %s: %v", key, err)
            http.Error(w, "internal server error", http.StatusInternalServerError)
            return
        }
        cacheMu.Lock()
        cache.PutTTL(key, b, cacheTTL)
        cacheMu.Unlock()
    }
    write(w, r, started, b, hit)
}

// fresh answers with v as JSON, never from the cache and never into it.
func fresh(w http.ResponseWriter, r *http.Request, v any) {
    var started = time.Now()
    var b, err = json.Marshal(v)
    if err != nil {
        logging.Err("max app: encode %s: %v", r.URL.RequestURI(), err)
        http.Error(w, "internal server error", http.StatusInternalServerError)
        return
    }
    write(w, r, started, b, false)
}

func write(w http.ResponseWriter, r *http.Request, started time.Time, b []byte, hit bool) {
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.Header().Set("Cache-Control", "no-store")
    w.Write(b)
    var mark = ""
    if hit { mark = " [cached]" }
    logging.Info("max app: %s %s [%.2f ms]%s", r.Method, r.URL.RequestURI(), float64(time.Since(started).Microseconds())/1000, mark)
}

// requireInitData rejects anything without a currently valid signature on the
// launch data MAX hands the Mini App, and is the only thing standing between
// these endpoints and anyone who knows the URL. The secret depends on the token
// alone, so it is computed once, when the handler is built.
func requireInitData(token string, h http.HandlerFunc) http.HandlerFunc {
    var mac = hmac.New(sha256.New, []byte("WebAppData"))
    mac.Write([]byte(token))
    var secret = mac.Sum(nil)
    return func(w http.ResponseWriter, r *http.Request) {
        if !valid(secret, r.Header.Get(initHeader), time.Now()) {
            logging.Info("max app: rejected %s without valid initData", r.URL.Path)
            http.Error(w, "open this from MAX", http.StatusUnauthorized)
            return
        }
        h(w, r)
    }
}

// valid checks launch data the way MAX's documentation lays it out: every
// parameter present exactly once, hash among them; the rest sorted by key,
// joined as key=value lines and signed with HMAC-SHA256 under secret — which is
// HMAC-SHA256 of the bot token keyed by the literal "WebAppData", the scheme
// Telegram's Mini Apps use. A signature is otherwise valid forever, so
// auth_date must be within initDataTTL of now.
func valid(secret []byte, initData string, now time.Time) bool {
    var v, err = url.ParseQuery(initData)
    if err != nil || len(v["hash"]) != 1 { return false }
    var want = strings.ToLower(v.Get("hash"))
    v.Del("hash")
    var keys = make([]string, 0, len(v))
    for k, values := range v {
        if len(values) != 1 { return false }
        keys = append(keys, k)
    }
    sort.Strings(keys)
    var pairs = make([]string, 0, len(keys))
    for _, k := range keys { pairs = append(pairs, k+"="+v.Get(k)) }
    var sig = hmac.New(sha256.New, secret)
    sig.Write([]byte(strings.Join(pairs, "\n")))
    if !hmac.Equal([]byte(hex.EncodeToString(sig.Sum(nil))), []byte(want)) { return false }
    var ts, terr = strconv.ParseInt(v.Get("auth_date"), 10, 64)
    return terr == nil && now.Sub(time.Unix(ts, 0)) <= initDataTTL
}

// chatOf is the chat a watch made from the Mini App is filed under: the dialog
// with the bot it was opened from, which MAX signs into the launch data as
// chat. That is the chat id the bot's own /watch files a watch under, and the
// one a notification is sent to, so the app and the chat share one watch list.
// Opened anywhere but a dialog — a group, a channel, or with no chat at all —
// there is no such chat, and 0 says so. Meaningful only on launch data
// requireInitData has accepted.
func chatOf(initData string) int64 {
    var v, err = url.ParseQuery(initData)
    if err != nil { return 0 }
    var c struct {
        ID   int64  `json:"id"`
        Type string `json:"type"`
    }
    if json.Unmarshal([]byte(v.Get("chat")), &c) != nil || !strings.EqualFold(c.Type, "dialog") { return 0 }
    return c.ID
}

// watchable reports whether a page can be watched: an address or a transaction.
func watchable(kind string) bool { return kind == "address" || kind == "tx" }

// details finishes a details page the way the Telegram Mini App does. The URL
// says what was asked for and the page what came back, and they differ for a
// block hash: it has a txid's shape, so it is asked for as a transaction, and
// main resolves it to the block. The watch button follows what the page turned
// out to be, so a block never carries one.
func details(info app.Info, kind, id string) app.Info {
    if info.Kind == "" { info.Kind, info.Id = kind, id }
    if !info.OK || !watchable(info.Kind) { info.Kind, info.Id = "", "" }
    return info
}

// isHex64 is the shape of a txid or a block hash.
func isHex64(s string) bool {
    if len(s) != 64 { return false }
    for _, c := range s {
        if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') { return false }
    }
    return true
}

// number reads a non-negative integer out of the query, or def when it is
// absent or anything else.
func number(r *http.Request, name string, def int64) int64 {
    var n, err = strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
    if err != nil || n < 0 { return def }
    return n
}

// addrKind, chartData and chartPeriod are the selections a request names. Each
// arrives in a URL a user can edit, so anything unrecognised becomes what the
// page opens on.
func addrKind(r *http.Request) string {
    switch k := r.URL.Query().Get("kind"); k {
    case "active", "abandoned":
        return k
    }
    return "rich"
}

func chartData(r *http.Request) string {
    if r.URL.Query().Get("data") == "consumption" { return "consumption" }
    return "blocks"
}

func chartPeriod(r *http.Request) string {
    switch p := r.URL.Query().Get("period"); p {
    case "quarter", "year":
        return p
    }
    return "month"
}

// aliasMax bounds the name a watch can be given from the app, in runes, as the
// Telegram Mini App does.
const aliasMax = 64

// watchState is what the watch button shows: whether the reader watches the
// page, and whether setting it failed, so the button can say so rather than
// claim a state the store does not have.
type watchState struct {
    On    bool
    Error bool
}

// Start serves the MAX Mini App on addr and returns the server, so the caller
// can drain it before closing anything it depends on. token is the MAX bot
// token, used only to verify the launch data. Bind addr to localhost and put a
// tunnel in front, as with the Telegram Mini App.
//
// Shutdown runs the hook before it starts waiting, so the open event streams
// end instead of holding it open until the deadline.
func Start(addr, token string, opt app.Options) *http.Server {
    var closing = make(chan struct{})
    var srv = &http.Server{Addr: addr, Handler: routes(token, opt, closing)}
    srv.RegisterOnShutdown(func() { close(closing) })
    go func() {
        logging.Status("MAX mini app listening on %s", addr)
        if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            logging.Err("max app: %v", err)
        }
    }()
    return srv
}

// routes is the Mini App's handler. The data comes from opt, the same functions
// the Telegram Mini App renders from, asked in Russian. Every endpoint under
// /api/ answers JSON and needs signed launch data; the page and the event
// stream need none. closing ends the event streams.
//
// The block list: the newest batch, the next one below a height as the reader
// scrolls, or what a new block put above the top one. That last is never
// cached — it is keyed by a height that moves with every block.
//
// A details page. A 64-hex id goes to the transaction endpoint whatever it
// is, and main shows a block hash as its block.
//
// The miner page carries its chart, opened on blocks over the last month.
//
// The mempool page, or what an open one prepends: after is the sequence number
// the page has seen up to, absent for the whole page.
//
// The watch list, the watch button and the alias are the reader's own, so
// they are answered for the dialog the app was opened from and never cached.
// The desired state rides in the request rather than being toggled, so a
// stale button cannot flip a watch the reader did not mean to touch.
//
// An empty alias leaves the watch alone, since Save on an empty field is how
// a reader dismisses the dialog; a long one is cut rather than refused.
func routes(token string, opt app.Options, closing <-chan struct{}) *http.ServeMux {
    var mux = http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path != "/" {
            http.NotFound(w, r)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Header().Set("Content-Language", lang)
        w.Header().Set("Cache-Control", "no-cache")
        w.Write(page)
    })
    mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) { events(w, r, closing) })
    var api = func(name string, h http.HandlerFunc) { mux.HandleFunc("/api/"+name, requireInitData(token, h)) }
    api("fees", func(w http.ResponseWriter, r *http.Request) { serve(w, r, func() any { return opt.Fees() }) })
    api("network", func(w http.ResponseWriter, r *http.Request) { serve(w, r, func() any { return opt.Network() }) })
    api("market", func(w http.ResponseWriter, r *http.Request) { serve(w, r, func() any { return opt.Market(lang) }) })
    api("blocks", func(w http.ResponseWriter, r *http.Request) {
        if after := number(r, "after", 0); after > 0 {
            fresh(w, r, opt.Blocks(lang, app.Range{After: after}))
            return
        }
        var before = number(r, "before", 0)
        serve(w, r, func() any { return opt.Blocks(lang, app.Range{Before: before}) })
    })
    api("addresses", func(w http.ResponseWriter, r *http.Request) {
        var rng = app.AddrRange{Kind: addrKind(r), From: int(number(r, "from", 0))}
        serve(w, r, func() any { return opt.Addresses(lang, rng) })
    })
    api("block", func(w http.ResponseWriter, r *http.Request) {
        var height = number(r, "height", -1)
        if height < 0 {
            http.Error(w, "no such block", http.StatusBadRequest)
            return
        }
        serve(w, r, func() any { return details(opt.BlockInfo(lang, height), "", "") })
    })
    api("tx", func(w http.ResponseWriter, r *http.Request) {
        var id = strings.TrimSpace(r.URL.Query().Get("id"))
        if !isHex64(id) {
            http.Error(w, "no such transaction", http.StatusBadRequest)
            return
        }
        serve(w, r, func() any { return details(opt.TxInfo(lang, id), "tx", id) })
    })
    api("address", func(w http.ResponseWriter, r *http.Request) {
        var a = strings.TrimSpace(r.URL.Query().Get("a"))
        if a == "" {
            http.Error(w, "no address", http.StatusBadRequest)
            return
        }
        serve(w, r, func() any { return details(opt.AddrInfo(lang, a), "address", a) })
    })
    api("addrtxs", func(w http.ResponseWriter, r *http.Request) {
        var a = strings.TrimSpace(r.URL.Query().Get("a"))
        var from = int(number(r, "from", 0))
        if a == "" || from <= 0 {
            http.Error(w, "no such transactions", http.StatusBadRequest)
            return
        }
        serve(w, r, func() any { return opt.AddrTxs(lang, a, from) })
    })
    api("miner", func(w http.ResponseWriter, r *http.Request) {
        var name = strings.TrimSpace(r.URL.Query().Get("name"))
        if name == "" {
            http.Error(w, "no miner", http.StatusBadRequest)
            return
        }
        serve(w, r, func() any {
            var info = details(opt.MinerInfo(lang, name), "", "")
            if info.OK {
                var chart = opt.MinerChart(lang, name, "blocks", "month")
                info.Chart = &chart
            }
            return info
        })
    })
    api("minerchart", func(w http.ResponseWriter, r *http.Request) {
        var name = strings.TrimSpace(r.URL.Query().Get("name"))
        if name == "" {
            http.Error(w, "no miner", http.StatusBadRequest)
            return
        }
        var data, period = chartData(r), chartPeriod(r)
        serve(w, r, func() any { return opt.MinerChart(lang, name, data, period) })
    })
    api("mempool", func(w http.ResponseWriter, r *http.Request) {
        fresh(w, r, opt.Mempool(lang, number(r, "after", -1)))
    })
    api("watches", func(w http.ResponseWriter, r *http.Request) {
        var chat = chatOf(r.Header.Get(initHeader))
        if chat == 0 {
            http.Error(w, "open this from the chat with the bot", http.StatusForbidden)
            return
        }
        fresh(w, r, opt.Watches(chat))
    })
    api("watch", func(w http.ResponseWriter, r *http.Request) {
        var kind, id = r.FormValue("kind"), strings.TrimSpace(r.FormValue("id"))
        if !watchable(kind) || id == "" {
            http.Error(w, "nothing to watch", http.StatusBadRequest)
            return
        }
        var chat = chatOf(r.Header.Get(initHeader))
        if chat == 0 {
            http.Error(w, "open this from the chat with the bot", http.StatusForbidden)
            return
        }
        if r.Method != http.MethodPost {
            fresh(w, r, watchState{On: opt.Watching(chat, kind, id)})
            return
        }
        var on, err = opt.SetWatch(chat, kind, id, r.FormValue("on") == "1")
        if err != nil { logging.Err("max app: set watch %s: %v", id, err) }
        fresh(w, r, watchState{On: on, Error: err != nil})
    })
    api("alias", func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "post an alias", http.StatusMethodNotAllowed)
            return
        }
        var kind, id = r.FormValue("kind"), strings.TrimSpace(r.FormValue("id"))
        if !watchable(kind) || id == "" {
            http.Error(w, "nothing to name", http.StatusBadRequest)
            return
        }
        var chat = chatOf(r.Header.Get(initHeader))
        if chat == 0 {
            http.Error(w, "open this from the chat with the bot", http.StatusForbidden)
            return
        }
        var alias = strings.TrimSpace(r.FormValue("alias"))
        if len([]rune(alias)) > aliasMax { alias = string([]rune(alias)[:aliasMax]) }
        if alias == "" {
            w.WriteHeader(http.StatusNoContent)
            return
        }
        if _, err := opt.SetAlias(chat, kind, id, alias); err != nil {
            logging.Err("max app: set alias %s: %v", id, err)
            http.Error(w, "internal server error", http.StatusInternalServerError)
            return
        }
        w.WriteHeader(http.StatusNoContent)
    })
    return mux
}

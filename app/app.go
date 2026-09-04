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

import "embed"
import "bytes"
import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "encoding/json"
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

//go:embed app.html app.*.html
var htmls embed.FS

//go:embed htmx.min.js
var htmxJS []byte

//go:embed htmx-ext-sse.js
var sseJS []byte

// langs are the languages the page is translated into, English first because it
// is the reference the others are checked against and the fallback for every
// language there is no page for. tools/i18n-html is what keeps the translated
// files structurally identical to app.html — nothing else notices when a whole
// second copy of a page drifts.
var langs = []string{"en", "ru", "es"}

// templates is one parsed page per language: the shell and, inside it, the "fees",
// "network" and the other blocks. The initial render and each refresh render
// those same blocks from the same file, so the card the page ships with and the
// card that replaces it cannot drift apart.
var templates = parse()

func parse() map[string]*template.Template {
    var out = map[string]*template.Template{}
    for _, lang := range langs {
        var name = "app.html"
        if lang != langs[0] { name = "app." + lang + ".html" }
        var b, err = htmls.ReadFile(name)
        if err != nil { panic("mini app: " + err.Error()) }
        out[lang] = template.Must(template.New("app").Parse(string(b)))
    }
    return out
}

// language picks the page to render. Telegram signs the user's own setting into
// initData, so when the app is opened from the bot that is the answer — it is
// the same language the bot's own replies use. A plain browser has only
// Accept-Language, and anything we have no page for falls back to English.
func language(r *http.Request) string {
    if l := langOf(r.Header.Get("X-Telegram-Init-Data")); supported(l) { return l }
    for _, tag := range accepted(r.Header.Get("Accept-Language")) {
        if supported(tag) { return tag }
    }
    return langs[0]
}

func supported(lang string) bool {
    for _, l := range langs {
        if l == lang { return true }
    }
    return false
}

// langOf pulls language_code out of an initData payload — "ru", or "pt-br" for a
// regional variant, of which only the base is ours to match. Meaningful only on
// a payload isValid has accepted, since the field is part of what Telegram signs.
func langOf(initData string) string {
    var v, err = url.ParseQuery(initData)
    if err != nil { return "" }
    var u struct {
        Lang string `json:"language_code"`
    }
    if json.Unmarshal([]byte(v.Get("user")), &u) != nil { return "" }
    return base(u.Lang)
}

// accepted reads an Accept-Language header into the languages it asks for, best
// first. Quality drives the order rather than position: "en;q=0.8,ru" wants
// Russian, and reading it in written order would answer in English.
func accepted(header string) []string {
    type want struct {
        lang string
        q    float64
    }
    var list []want
    for _, part := range strings.Split(header, ",") {
        var tag, params, _ = strings.Cut(strings.TrimSpace(part), ";")
        if tag = base(tag); tag == "" { continue }
        var q = 1.0
        // A malformed or absent q is 1.0 — the default the header's grammar
        // gives it — so a broken parameter cannot silently demote a language.
        if _, after, ok := strings.Cut(params, "q="); ok {
            if f, err := strconv.ParseFloat(strings.TrimSpace(after), 64); err == nil { q = f }
        }
        list = append(list, want{tag, q})
    }
    sort.SliceStable(list, func(i, j int) bool { return list[i].q > list[j].q })
    var out []string
    for _, w := range list { out = append(out, w.lang) }
    return out
}

// base is the primary subtag, lowercased: the "pt" of "pt-BR". A page is per
// language, not per region.
func base(tag string) string {
    tag, _, _ = strings.Cut(strings.TrimSpace(tag), "-")
    if tag == "*" { return "" }
    return strings.ToLower(tag)
}

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

// blocksCached is how many block-list renders are kept — batches as the reader
// scrolls, plus the restored lists a Back returns to. A reader scrolls a few
// screens and stops, so twenty covers the depth anyone reaches, while bounding
// what an edited before= parameter can make the process hold.
const blocksCached = 20

// cardsCached is exactly the number of single-valued renders — the three cards
// plus the shell page — in every language, so nothing is ever evicted for space.
// This cache is keyed and expiring, not bounded; blocksCache is the one that
// needs the bound.
var cardsCached = 4 * len(langs)

var cacheMu sync.Mutex

// cardsCache holds the single-valued renders, keyed by the template block each comes
// from. "app" is the whole shell page: one copy serves everyone, which is only
// sound because / carries no per-user data — the rule that already keeps it
// servable without a signature at all. Anything per-chat must stay behind the
// requireInitData endpoints, and caching here is a second reason why.
var cardsCache = lru.New[string, []byte](cardsCached)

// key is what a rendered page is filed under. The language belongs in it: the
// same URL renders in three of them, and serving one reader's language to
// another is exactly what a URL-only key would do.
func key(lang, uri string) string { return lang + uri }
var blocksCache = lru.New[string, []byte](blocksCached)

// invalidate drops one card's rendered HTML. Notify calls it *before* announcing
// the event, so a page reacting immediately cannot be handed the very copy it
// was told to replace.
func invalidate(event string) {
    cacheMu.Lock()
    defer cacheMu.Unlock()
    switch event {
    case "fees", "network", "market":
        for _, lang := range langs { cardsCache.Delete(key(lang, "/"+event)) }
    case "blocks":
        blocksCache.Clear()
        cardsCache.Clear()
    default: return
    }
    // The page embeds every card, so whichever one moved, the page it would
    // serve to the next visitor is stale — in every language, since the card
    // that moved is in all of them.
    for _, lang := range langs { cardsCache.Delete(key(lang, "/")) }
}

// for testing
func invalidateAll() {
    cacheMu.Lock()
    cardsCache.Clear()
    blocksCache.Clear()
    cacheMu.Unlock()
}

// serve writes a cached render, producing it only on a miss. data is called
// outside cacheMu because it reaches into main's Source, which holds locks of
// its own; two concurrent misses simply render the same bytes twice, which is
// cheaper than serialising every request behind one mutex.
func cached(c *lru.Cache[string, []byte], w http.ResponseWriter, r *http.Request, render func(string) []byte) {
    var started = time.Now().UnixNano()
    var lang = language(r)
    cacheMu.Lock()
    var b, hit = c.Get(key(lang, r.RequestURI))
    cacheMu.Unlock()
    if !hit {
        b = render(lang)
        if b == nil {
            http.Error(w, "internal server error", http.StatusInternalServerError)
            return
        }
        cacheMu.Lock()
        c.PutTTL(key(lang, r.RequestURI), b, cacheTTL)
        cacheMu.Unlock()
    }
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    w.Header().Set("Content-Language", lang)
    w.Write(b)
    logging.Info("mini app: %s %s [%s] %.2f ms cached = %v", r.Method, r.RequestURI, lang, float64(time.Now().UnixNano() - started) / 1e6, hit)
}

func render(lang, name string, data any) []byte {
    var t, ok = templates[lang]
    if !ok { t = templates[langs[0]] }
    var buf bytes.Buffer
    var err = t.ExecuteTemplate(&buf, name, data)
    if err != nil {
        logging.Err("mini app: render error %s [%s]: %v", name, lang, err)
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

// subscribes reports how many streams are connected; used by the tests to
// wait for a handler to register rather than sleeping a fixed time.
func subscribes() int {
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
//
// closing is what ends the stream when the server is shutting down; see Start.
func events(w http.ResponseWriter, r *http.Request, closing <-chan struct{}) {
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
// transaction count and the pool that mined it. MinerKnown is false when
// attribution failed and Miner is the "Unknown" placeholder, which is not a
// link — main owns that string, so the template does not compare against it.
type Block struct {
    // Height is the grouped display form ("963 268"); Num is the same height
    // raw, because the display form has spaces in it and the details link needs
    // something that survives a URL.
    Height     string
    Num        int64
    Size       string
    Txs        string
    Miner      string
    MinerKnown bool
}

// Field is one line of the block details page. An empty Value marks a heading
// ("Fees", "Tx sizes") rather than a field, which is how the bot's own reply is
// built — see blockPairs in main.
//
// Parts, when main sets them, is the same value cut into the ids it mentions and
// the text between them, so a reader can tap an address in a transaction's
// output list or a block's hash and land on that page. Value still carries the
// whole line: it is what a row without ids renders, and an empty one is still
// what marks a heading.
type Field struct {
    Label string
    Value string
    Parts []Part
}

// Part is one piece of a row's value. An empty Id is plain text; otherwise the
// part is tappable and Id is what it opens — handed to /search, which classifies
// it the way the bot's info() does, so a part needs to carry no kind of its own.
type Part struct {
    Text string
    Id   string
}

// Info is a details page — a block, a transaction or an address. Title and Rows
// come from main, and are the same lines /info prints. Slot and Back are filled
// in by the handler: which container the fragment replaces, and where Back
// returns to, are the app's business rather than main's. OK is false when there
// is no such thing to show, or the node is not configured to look one up.
type Info struct {
    OK    bool
    Title string
    Rows  []Field
    Slot  string
    Back  string
    // Kind and Id name what the watch button acts on — "address" or "tx" and
    // the full id. Empty on a block or miner page, which has nothing to watch.
    // The button itself is loaded separately: whether *this* reader watches it
    // is per-user, and the page around it is one cached copy shared by all.
    Kind string
    Id   string
    // Swap is the Back button's hx-swap. It carries a show: modifier when the
    // page was opened from the block list, which is what scrolls the row the
    // reader tapped back into view.
    Swap string
    // From is where a tapped id inside this page sends its own Back: the origin
    // this page was opened with, carried on so a whole chain of pages returns to
    // where the reader started rather than to whichever one they came through.
    From string
}
// Blocks is one batch of the recent-block list, newest first. Top and Next are
// the heights the two sentinels ask about, so the template does no arithmetic.
type Blocks struct {
    OK   bool
    Rows []Block
    // Top is the highest height the list holds — what the sentinel above the
    // rows asks for anything newer than. On a batch that came back empty it is
    // the height that was asked about, so the sentinel keeps its place.
    Top int64
    // Next is the lowest height in this batch: where the sentinel below the rows
    // continues. More says whether the batch was cut short — there are older
    // blocks to append, or (on a newer-than request) more new ones than fit.
    Next int64
    More bool
}

// Range is the window of the block list a request wants. At most one field is
// set; all three zero is the newest batch, which is what the tab opens on.
type Range struct {
    // Before is the next batch as the reader scrolls: the blocks below this
    // height. It is a height rather than a page number because the list grows at
    // the head — an offset would shift under a reader mid-scroll and repeat the
    // row that crossed the boundary.
    Before int64
    // After is what a new block prepends: the blocks above this height.
    After int64
    // Down restores a list Back returns to: everything from the tip down to the
    // block the reader had opened, so the rows they had scrolled through are
    // there again.
    Down int64
}

// watchButton is the bell in a details page's title row: what it acts on, and
// whether this reader is currently watching it. Error marks a set that failed,
// so the button can say so instead of silently lying about the state.
type watchButton struct {
    Kind  string
    Id    string
    On    bool
    Error bool
}

// Watch is one watched id on the Watches tab: the shortened form the row shows,
// the full id its link carries, and the alias the user gave it, if any.
type Watch struct {
    Short string
    Id    string
    Alias string
}

// Watches is one user's watch list. Alone among the Source calls this one is
// per-user, which is why it is never cached and never rendered into the shell
// page — both of those are shared by every visitor. OK is false when the lookup
// failed, which is a different answer from watching nothing.
type Watches struct {
    OK        bool
    Addresses []Watch
    Txs       []Watch
}

// Source supplies the chain data the app renders. main implements it; the app
// package stays unaware of the fee cache, Bitcoin Core and the price feeds.
//
// Everything a call renders text into takes the reader's language, because the
// words are main's rather than the page's: the details rows are the very lines
// the bot prints, and a block row's "Unknown" miner or a period's "1w" is a
// string main chose. The calls that carry no words of their own — the fee tiers
// and the chain figures are numbers, a watch list is ids — take none, and their
// labels come from the translated page around them.
type Source interface {
    Fees() Fees
    Network() Network
    Market(lang string) Market
    Blocks(lang string, rng Range) Blocks
    BlockInfo(lang string, height int64) Info
    TxInfo(lang, txid string) Info
    AddrInfo(lang, address string) Info
    MinerInfo(lang, name string) Info
    Watches(chat int64) Watches
    Watching(chat int64, kind, id string) bool
    SetWatch(chat int64, kind, id string, on bool) (bool, error)
    SetAlias(chat int64, kind, id, alias string) (bool, error)
}

// aliasMax bounds the name a watch can be given from the app, in runes: it is a
// label on a row and in a notification, and nothing else keeps it small.
const aliasMax = 64

// The two containers a details page can replace, each the content of one tab.
const blocksSlot = "blocklist"
const addressSlot = "addrpanel"

// tabOf is the panel a slot's content lives in.
func tabOf(slot string) string {
    if slot == addressSlot { return "addresses" }
    return "blocks"
}

// showtab is the HX-Trigger that moves the reader to a panel. The JSON form
// carries the name, so one listener handles every tab.
func showtab(panel string) string { return `{"showtab":"` + panel + `"}` }

// trigger builds an HX-Trigger header from a set of events. It is marshalled
// rather than concatenated because these carry ids a user chose — an address
// from a URL — where showtab carries only a whitelisted panel name.
func trigger(events map[string]any) string {
    var b, err = json.Marshal(events)
    if err != nil { return "" }
    return string(b)
}

// isPanel guards what may go into that header: the name arrives in a URL a user
// can edit, and it is interpolated into JSON.
func isPanel(name string) bool {
    switch name {
    case "home", "blocks", "addresses", "watches":
        return true
    }
    return false
}

// origin is the tab the reader came from, which is where Back should put them —
// the search field on Home, the list they were reading, or their watch list. It
// rides in the URL because only the link that opened the page knows it. An
// absent or unrecognised value falls back to the tab the page itself lives in,
// which is the old behaviour.
func origin(r *http.Request, slot string) string {
    var from = r.URL.Query().Get("from")
    if isPanel(from) { return from }
    return tabOf(slot)
}

// backToList is the Back target for a page shown in the Blocks tab, and the swap
// that goes with it: restore the list down to the block the reader opened, scroll
// that row back into view, then hand them to whichever tab they came from. An
// infinite list has no page number to return to, so the row itself is the mark —
// and restoring the rows without restoring the position would land the reader at
// the top of a list they had scrolled a long way down.
func backToList(r *http.Request) (string, string) {
    var to = origin(r, blocksSlot)
    var down = downOf(r)
    if down == "" { return "blocks?to=" + to, "outerHTML" }
    return "blocks?down=" + down + "&to=" + to, "outerHTML show:#blk" + down + ":top"
}

// downOf is the block a details page was opened from, as it rides in the URL:
// only the row that was tapped knows it. A search or a watch list arrives
// without one, and lands on the newest blocks. Genesis is excluded along with
// the nonsense, which also keeps a down=0 from asking for the whole chain.
func downOf(r *http.Request) string {
    var h, err = strconv.ParseInt(r.URL.Query().Get("down"), 10, 64)
    if err != nil || h <= 0 { return "" }
    return strconv.FormatInt(h, 10)
}

// heightOf reads a block height out of the query. Zero — absent, or nonsense
// from an edited URL — is what every caller reads as "from the tip".
func heightOf(r *http.Request, name string) int64 {
    var h, err = strconv.ParseInt(r.URL.Query().Get(name), 10, 64)
    if err != nil || h < 0 { return 0 }
    return h
}

// watchable reports whether a details page can be watched, and is what keeps the
// button off block and miner htmls.
func watchable(kind string) bool { return kind == "address" || kind == "tx" }

// details renders one of the three detail htmls. They differ only in which
// container they replace and where Back goes, so they share a template.
//
// HX-Retarget is what lets one search field reach three tabs: the field cannot
// know which container the answer belongs in — only the server, having
// classified the query, does — so it names a target and the response corrects
// it. Without this an address page lands in the Blocks tab, replacing the list.
func details(w http.ResponseWriter, r *http.Request, slot, back, swap, kind, id string, load func(lang string) Info) {
    w.Header().Set("HX-Retarget", "#"+slot)
    w.Header().Set("HX-Trigger", showtab(tabOf(slot)))
    var from = origin(r, slot)
    cached(blocksCache, w, r, func(lang string) []byte {
        var info = load(lang)
        info.Slot, info.Back, info.Swap, info.From = slot, back, swap, from
        if info.OK { info.Kind, info.Id = kind, id }
        return render(lang, "details", info)
    })
}

// chatOf pulls the user id out of an initData payload. It is only meaningful on
// a payload isValid has already accepted — the id is part of what Telegram
// signs — and for the private chat a Mini App is opened from it is also the chat
// id the bot files watches under.
func chatOf(initData string) int64 {
    var v, err = url.ParseQuery(initData)
    if err != nil { return 0 }
    var u struct {
        ID int64 `json:"id"`
    }
    if json.Unmarshal([]byte(v.Get("user")), &u) != nil { return 0 }
    return u.ID
}

// isTxid reports the 64-hex shape a txid has. A block hash has exactly the same
// shape, which only the node can tell apart — main's TxInfo resolves that, the
// way info() does for the bot.
func isTxid(s string) bool {
    if len(s) != 64 { return false }
    for _, c := range s {
        if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') { return false }
    }
    return true
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
        cached(cardsCache, w, r, func(lang string) []byte {
            return render(lang, "app", page{Fees: src.Fees(), Network: src.Network(), Market: src.Market(lang), Blocks: src.Blocks(lang, Range{})})
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
    // Closed when the server begins shutting down, which is what lets the open
    // event streams return. Per-server rather than package-level: a second Start
    // (the tests make several) would otherwise close the same channel twice.
    var closing = make(chan struct{})
    mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
        events(w, r, closing)
    })
    mux.HandleFunc("/fees", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        cached(cardsCache, w, r, func(lang string) []byte { return render(lang, "fees", src.Fees()) })
    }))
    mux.HandleFunc("/network", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        cached(cardsCache, w, r, func(lang string) []byte { return render(lang, "network", src.Network()) })
    }))
    mux.HandleFunc("/market", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        cached(cardsCache, w, r, func(lang string) []byte { return render(lang, "market", src.Market(lang)) })
    }))
    // The list itself: the newest blocks, or — when Back asked — everything down
    // to the block the reader had opened from it.
    mux.HandleFunc("/blocks", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        // Only when Back explicitly asked: an empty list also refreshes through
        // here, and that must never move a reader off their tab.
        if to := r.URL.Query().Get("to"); isPanel(to) { w.Header().Set("HX-Trigger", showtab(to)) }
        cached(blocksCache, w, r, func(lang string) []byte { return render(lang, "blocks", src.Blocks(lang, Range{Down: heightOf(r, "down")})) })
    }))
    // The batch the sentinel below the rows appends as the reader reaches it.
    mux.HandleFunc("/moreblocks", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var before = heightOf(r, "before")
        if before <= 0 {
            http.Error(w, "no such block", http.StatusBadRequest)
            return
        }
        cached(blocksCache, w, r, func(lang string) []byte { return render(lang, "blockrows", src.Blocks(lang, Range{Before: before})) })
    }))
    // What the sentinel above the rows prepends when a block is mined. Inserting
    // above the reader leaves the rows they are looking at where they are, where
    // re-rendering the list would throw them back to the top of it.
    //
    // Not cached: the answer decides a response header, which a cache hit would
    // not set — and it is keyed by a height that moves with every block, so an
    // entry would be read about once anyway.
    mux.HandleFunc("/newblocks", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var started = time.Now().UnixNano()
        var after = heightOf(r, "after")
        if after <= 0 {
            http.Error(w, "no such block", http.StatusBadRequest)
            return
        }
        var lang = language(r)
        var blocks, name = src.Blocks(lang, Range{After: after}), "newblocks"
        // More new blocks than one batch holds — the tab sat open through a
        // catch-up, or the stream was down for hours. Prepending would leave a
        // gap between them and the rows already on screen, so replace the whole
        // list instead: it costs the reader their place, which is the right
        // trade against showing a list with a hole in it.
        if blocks.More {
            blocks, name = src.Blocks(lang, Range{}), "blocks"
            w.Header().Set("HX-Retarget", "#"+blocksSlot)
        }
        var b = render(lang, name, blocks)
        if b == nil {
            http.Error(w, "internal server error", http.StatusInternalServerError)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Write(b)
        logging.Info("mini app: %s %s %.2f ms", r.Method, r.RequestURI, float64(time.Now().UnixNano() - started) / 1e6)
    }))
    // A details page replaces its tab's container in place, so the tab it
    // belongs to stays selected and Back can swap the original straight back in.
    // HX-Trigger moves the reader to that tab, which is what a search from Home
    // needs; opened from within the tab it lands on an already-active one and
    // does nothing.
    mux.HandleFunc("/block", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var height, err = strconv.ParseInt(r.URL.Query().Get("height"), 10, 64)
        if err != nil || height < 0 {
            http.Error(w, "no such block", http.StatusBadRequest)
            return
        }
        var back, swap = backToList(r)
        details(w, r, blocksSlot, back, swap, "", "", func(lang string) Info { return src.BlockInfo(lang, height) })
    }))
    mux.HandleFunc("/tx", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var id = strings.TrimSpace(r.URL.Query().Get("id"))
        if !isTxid(id) {
            http.Error(w, "no such transaction", http.StatusBadRequest)
            return
        }
        var back, swap = backToList(r)
        details(w, r, blocksSlot, back, swap, "tx", id, func(lang string) Info { return src.TxInfo(lang, id) })
    }))
    mux.HandleFunc("/miner", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var name = strings.TrimSpace(r.URL.Query().Get("name"))
        if name == "" {
            http.Error(w, "no miner", http.StatusBadRequest)
            return
        }
        var back, swap = backToList(r)
        details(w, r, blocksSlot, back, swap, "", "", func(lang string) Info { return src.MinerInfo(lang, name) })
    }))
    mux.HandleFunc("/address", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var a = strings.TrimSpace(r.URL.Query().Get("a"))
        if a == "" {
            http.Error(w, "no address", http.StatusBadRequest)
            return
        }
        details(w, r, addressSlot, "addresses?to="+origin(r, addressSlot), "outerHTML", "address", a, func(lang string) Info { return src.AddrInfo(lang, a) })
    }))
    // what Back on an address page returns to: the tab's own content, which is
    // still a placeholder
    mux.HandleFunc("/addresses", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var to = r.URL.Query().Get("to")
        if !isPanel(to) { to = "addresses" }
        w.Header().Set("HX-Retarget", "#"+addressSlot)
        w.Header().Set("HX-Trigger", showtab(to))
        cached(cardsCache, w, r, func(lang string) []byte { return render(lang, "addresses", nil) })
    }))
    // Never cached: every cache here is keyed by URL, which is identical for
    // every user, so a cached watch list would be handed to the wrong person.
    // This is also why the shell page ships an empty container rather than the
    // rendered list — / is one copy shared by every visitor.
    mux.HandleFunc("/watches", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var started = time.Now().UnixNano()
        var b = render(language(r), "watches", src.Watches(chatOf(r.Header.Get("X-Telegram-Init-Data"))))
        if b == nil {
            http.Error(w, "internal server error", http.StatusInternalServerError)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Write(b)
        logging.Info("mini app: %s %s %.2f ms", r.Method, r.RequestURI, float64(time.Now().UnixNano() - started) / 1e6)
    }))
    // The watch button. GET renders it for the calling user, POST sets the watch
    // and renders the result. Never cached: whether a given reader watches
    // something is per-user, and every cache here is keyed by URL alone.
    mux.HandleFunc("/watch", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var started = time.Now().UnixNano()
        // The bell names what it acts on in its URL; the alias dialog's Delete
        // button posts from inside a form, so there they arrive as form values.
        var kind = r.URL.Query().Get("kind")
        var id = strings.TrimSpace(r.URL.Query().Get("id"))
        if kind == "" { kind = r.PostFormValue("kind") }
        if id == "" { id = strings.TrimSpace(r.PostFormValue("id")) }
        if !watchable(kind) || id == "" {
            http.Error(w, "nothing to watch", http.StatusBadRequest)
            return
        }
        var chat = chatOf(r.Header.Get("X-Telegram-Init-Data"))
        var btn = watchButton{Kind: kind, Id: id}
        if r.Method == http.MethodPost {
            // The desired state rides in the request rather than being toggled
            // server-side, so a stale button cannot flip a watch the reader did
            // not mean to touch: setting it twice is a no-op, not an undo.
            var on, serr = src.SetWatch(chat, kind, id, r.URL.Query().Get("on") == "1")
            if serr != nil {
                logging.Err("mini app: set watch %s: %v", id, serr)
                btn.Error = true
            }
            btn.On = on
            // A watch that was just filed has no name yet, so the page is asked
            // to open the alias dialog — the second half of the two-step add.
            // Either way the watch list has changed and re-fetches itself.
            var events = map[string]any{"watchtab": ""}
            if on && !btn.Error { events["askalias"] = map[string]string{"kind": kind, "id": id} }
            w.Header().Set("HX-Trigger", trigger(events))
        } else {
            btn.On = src.Watching(chat, kind, id)
        }
        var b = render(language(r), "watchbtn", btn)
        if b == nil {
            http.Error(w, "internal server error", http.StatusInternalServerError)
            return
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Write(b)
        logging.Info("mini app: %s %s %.2f ms", r.Method, r.RequestURI, float64(time.Now().UnixNano() - started) / 1e6)
    }))
    // Naming a watch the reader already has: the dialog the bell opens after
    // filing one, and the same dialog the Watches tab's edit icon opens. Both
    // post from inside a form, so everything arrives as form values. The answer
    // is no content and a watchtab event — the list re-fetches itself, and the
    // dialog is closed by the page.
    mux.HandleFunc("/alias", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var started = time.Now().UnixNano()
        if r.Method != http.MethodPost {
            http.Error(w, "post an alias", http.StatusMethodNotAllowed)
            return
        }
        var kind = r.PostFormValue("kind")
        var id = strings.TrimSpace(r.PostFormValue("id"))
        if !watchable(kind) || id == "" {
            http.Error(w, "nothing to name", http.StatusBadRequest)
            return
        }
        // An empty alias leaves the watch alone rather than clearing its name:
        // Save on an empty field is what a reader taps to dismiss the dialog.
        // A long one is cut rather than refused — it is a label, and the field
        // is capped in the page too, but this endpoint is reachable directly.
        var alias = strings.TrimSpace(r.PostFormValue("alias"))
        if len([]rune(alias)) > aliasMax { alias = string([]rune(alias)[:aliasMax]) }
        if alias == "" {
            w.WriteHeader(http.StatusNoContent)
            return
        }
        if _, err := src.SetAlias(chatOf(r.Header.Get("X-Telegram-Init-Data")), kind, id, alias); err != nil {
            logging.Err("mini app: set alias %s: %v", id, err)
            http.Error(w, "internal server error", http.StatusInternalServerError)
            return
        }
        w.Header().Set("HX-Trigger", trigger(map[string]any{"watchtab": ""}))
        w.WriteHeader(http.StatusNoContent)
        logging.Info("mini app: %s %s %.2f ms", r.Method, r.RequestURI, float64(time.Now().UnixNano() - started) / 1e6)
    }))
    // search classifies the query and hands off, in the same order info() does:
    // the 64-hex shape first, because a string of 64 digits is also a valid
    // height, then a height, then an address as the catch-all.
    mux.HandleFunc("/search", requireInitData(token, func(w http.ResponseWriter, r *http.Request) {
        var q = strings.TrimSpace(r.URL.Query().Get("q"))
        // The search field is on Home and says nothing, so home is the default;
        // an id tapped inside a details page passes that page's own origin, so
        // Back still returns where the reader started.
        var from = r.URL.Query().Get("from")
        if !isPanel(from) { from = "home" }
        switch {
        case q == "":
            w.WriteHeader(http.StatusNoContent)
        case isTxid(q):
            http.Redirect(w, r, "tx?id="+url.QueryEscape(q)+"&from="+from, http.StatusSeeOther)
        default:
            if height, err := strconv.ParseInt(q, 10, 64); err == nil && height >= 0 {
                http.Redirect(w, r, "block?height="+strconv.FormatInt(height, 10)+"&from="+from, http.StatusSeeOther)
                return
            }
            http.Redirect(w, r, "address?a="+url.QueryEscape(q)+"&from="+from, http.StatusSeeOther)
        }
    }))
    var srv = &http.Server{Addr: addr, Handler: mux}
    // Shutdown calls this before it starts waiting, so the streams end and the
    // connections go idle instead of holding it open until the deadline.
    srv.RegisterOnShutdown(func() { close(closing) })
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

package app

import "context"
import "html"
import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "net/http"
import "net/http/httptest"
import "net/url"
import "sort"
import "strconv"
import "strings"
import "testing"
import "time"

// signInitData produces the payload Telegram would hand the webview, so the
// tests exercise the real signature rather than a stub.
func signInitData(token string, fields map[string]string) string {
    var keys = make([]string, 0, len(fields))
    for k := range fields { keys = append(keys, k) }
    sort.Strings(keys)
    var pairs = make([]string, 0, len(keys))
    for _, k := range keys { pairs = append(pairs, k+"="+fields[k]) }
    var mac = hmac.New(sha256.New, []byte("WebAppData"))
    mac.Write([]byte(token))
    var secret = mac.Sum(nil)
    mac = hmac.New(sha256.New, secret)
    mac.Write([]byte(strings.Join(pairs, "\n")))
    var v = url.Values{}
    for k, val := range fields { v.Set(k, val) }
    v.Set("hash", hex.EncodeToString(mac.Sum(nil)))
    return v.Encode()
}

func freshInitData(token string) string {
    return signInitData(token, map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Unix(), 10),
        "user":      `{"id":42,"first_name":"Pin"}`,
        "query_id":  "AAF",
    })
}

// fakeSource stands in for main's caches.
type fakeSource struct {
    f Fees
    n Network
    m Market
    b map[int]Blocks
    d map[int64]Info
    t map[string]Info
    a map[string]Info
    w map[int64]Watches
}

func (s fakeSource) Fees() Fees       { return s.f }
func (s fakeSource) Network() Network { return s.n }
func (s fakeSource) Market() Market   { return s.m }
func (s fakeSource) Blocks(page int) Blocks { return s.b[page] }

func (s fakeSource) BlockInfo(height int64) Info {
    if d, ok := s.d[height]; ok { return d }
    return Info{Title: "Block " + strconv.FormatInt(height, 10)}
}

func (s fakeSource) TxInfo(txid string) Info {
    if d, ok := s.t[txid]; ok { return d }
    return Info{Title: txid[:6] + "..." + txid[58:]}
}

func (s fakeSource) Watches(chat int64) Watches {
    if w, ok := s.w[chat]; ok { return w }
    return Watches{OK: true}
}

// two users with different lists, so a leak between them is visible
func liveWatches() map[int64]Watches {
    return map[int64]Watches{
        42: {OK: true,
            Addresses: []Watch{{Short: "bc1qxy...hx0wlh", Id: liveAddress, Alias: "John"}},
            Txs:       []Watch{{Short: "32e43e...870b16", Id: liveTxid}}},
        99: {OK: true,
            Addresses: []Watch{{Short: "1A1zP1...DivfNa", Id: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}}},
    }
}

func (s fakeSource) AddrInfo(addr string) Info {
    if d, ok := s.a[addr]; ok { return d }
    return Info{Title: addr}
}

// liveTx and liveAddr mirror what main builds from txPairs and addrPairs.
func liveTx() map[string]Info {
    return map[string]Info{liveTxid: {OK: true, Title: "32e43e...870b16", Rows: []Field{
        {Label: "Confirmations", Value: "412 (block #963268)"},
        {Label: "Amount", Value: "9 990 000 sats (≈ $6,614)"},
        {Label: "Fee", Value: "1 410 sats (10.0 sat/vB)"},
        {Label: "Size", Value: "223 B (141 vB)"},
        {Label: "Inputs", Value: "bc1qxy...dayd2g"},
        {Label: "Outputs", Value: "1A1zP1...DivfNa, bc1qxy...dayd2g"},
    }}}
}

func liveAddr() map[string]Info {
    return map[string]Info{liveAddress: {OK: true, Title: "bc1qxy...dayd2g", Rows: []Field{
        {Label: "Type", Value: "segwit (bech32)"},
        {Label: "Balance", Value: "0.09990000 BTC"},
        {Label: "Total received", Value: "1.20000000 BTC"},
        {Label: "Total sent", Value: "1.10010000 BTC"},
        {Label: "Total flow", Value: "2.30010000 BTC"},
        {Label: "Total fees", Value: "0.00014100 BTC"},
        {Label: "Transactions", Value: "42"},
        {Label: "First tx", Value: "14 november 2023 22:13"},
        {Label: "Last tx", Value: "2 days ago"},
        {Label: "Activity period", Value: "1 year 8 months"},
    }}}
}

const liveTxid = "32e43e6f2b1c4d5a8f9e0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5e6f7a8b870b16"
const liveAddress = "bc1qxy2kgdygjrsqtzq2n0yrf2493p83kkfjhx0wlh"


// liveBlockInfo mirrors what main builds from blockPairs: capitalised fields,
// with "Fees" and "Tx sizes" as headings whose value is empty.
func liveBlockInfo() map[int64]Info {
    return map[int64]Info{963268: {OK: true, Title: "Block 963 268", Rows: []Field{
        {Label: "Hash", Value: "0000ab...9f21cd"},
        {Label: "Time", Value: "2 hours ago"},
        {Label: "Size", Value: "1.56 MB"},
        {Label: "Transactions", Value: "4000"},
        {Label: "Miner", Value: "AntPool"},
        {Label: "Difficulty", Value: "142.34 T"},
        {Label: "Fees"},
        {Label: "lowest", Value: "141 sats (1.0 sat/vB)"},
        {Label: "average", Value: "3 208 sats (12.4 sat/vB)"},
        {Label: "highest", Value: "91 004 sats (204.1 sat/vB)"},
        {Label: "Tx sizes"},
        {Label: "minimum", Value: "141 B"},
        {Label: "average", Value: "258 B"},
        {Label: "maximum", Value: "84 122 B"},
        {Label: "Reward", Value: "312 500 000 sats (≈ $206,881)"},
        {Label: "Reward + fees", Value: "325 118 004 sats (≈ $215,235)"},
    }}}
}

// two pages of ten, so pagination has something to move between
func liveBlocks() map[int]Blocks {
    var mk = func(page, from int, hasNext bool) Blocks {
        var b = Blocks{OK: true, Page: page, Num: page + 1, Prev: page - 1, Next: page + 1,
            HasPrev: page > 0, HasNext: hasNext}
        for i := 0; i < 12; i++ {
            // the last row of each page is unattributed, so every page carries
            // both the linked and the plain form of the miner field
            var row = Block{Height: strconv.Itoa(from - i), Num: int64(from - i),
                Size: "1.56 MB", Txs: "4 000 txs", Miner: "AntPool", MinerKnown: true}
            if i == 11 { row.Miner, row.MinerKnown = "Unknown", false }
            b.Rows = append(b.Rows, row)
        }
        return b
    }
    return map[int]Blocks{0: mk(0, 963268, true), 1: mk(1, 963256, false)}
}

func liveMarket() Market {
    return Market{OK: true, Price: "$66,202.00", Changes: []Change{
        {Label: "1d", Pct: "+2.5%", Up: true},
        {Label: "1w", Pct: "-3.1%"},
        {Label: "1mo", Pct: "+12.4%", Up: true},
        {Label: "3mo", Pct: "-8.7%"},
        {Label: "1y", Pct: "+64.2%", Up: true},
        {Label: "5y", Pct: "+512.9%", Up: true},
    }}
}

func liveNetwork() Network {
    return Network{OK: true, Coins: "20.1 M", Cap: "21 M",
        Blocks: "963 166", Size: "869 GB", Nodes: "31 751", Txs: "1.4 B", Addresses: "1.5 B"}
}

// handler returns the routes Start wires. Start inlines the routing and always
// launches a listener, so tests take its Handler and drive it with a recorder
// rather than over a socket; the ephemeral listener is closed via t.Cleanup.
func handler(t *testing.T, token string, src Source) http.Handler {
    // The rendered-card caches are package state shared across tests, so each
    // test starts from empty rather than seeing the previous one's fixture.
    resetCache()
    var srv = Start("127.0.0.1:0", token, src)
    t.Cleanup(func() { srv.Close() })
    return srv.Handler
}

func get(h http.Handler, path, initData string) *httptest.ResponseRecorder {
    var r = httptest.NewRequest("GET", path, nil)
    if initData != "" { r.Header.Set("X-Telegram-Init-Data", initData) }
    var w = httptest.NewRecorder()
    h.ServeHTTP(w, r)
    return w
}

func liveFees() Fees {
    return Fees{OK: true, TxCount: "36 552",
        Fast: Tier{Rate: "12", USD: "$0.45"},
        Hour: Tier{Rate: "4", USD: "$0.15"},
        Slow: Tier{Rate: "1", USD: "$0.04"}}
}

func TestServesPage(t *testing.T) {
    var w = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "")
    if w.Code != 200 {
        t.Fatalf("GET / = %d, want 200", w.Code)
    }
    var body = w.Body.String()
    for _, want := range []string{"telegram-web-app.js", "htmx.min.js", `hx-get="fees"`,
        "X-Telegram-Init-Data", `data-panel="home"`, `data-panel="watches"`, `id="q"`} {
        if !strings.Contains(body, want) {
            t.Errorf("page is missing %q", want)
        }
    }
}

// The card must arrive already rendered: the page carries the fees, rather than
// shipping a placeholder and fetching them in a second round trip.
func TestPageRendersFeesInline(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    for _, want := range []string{"<h2>Network fees</h2>", ">Fast<", ">1 hour<", ">2+ hours<",
        ">12 <", ">4 <", ">1 <", "36 552"} {
        if !strings.Contains(body, want) {
            t.Errorf("page did not render %q inline", want)
        }
    }
    // scoped to the fees card: the Watches panel legitimately ships a
    // placeholder, since its content is per-user and cannot be in a shared page
    var card = body[strings.Index(body, `id="fees"`):strings.Index(body, `id="network"`)]
    if strings.Contains(card, "loading…") {
        t.Error(`the fees card still ships a "loading…" placeholder; it should be rendered server-side`)
    }
}

// A cold cache is reported in the page itself, not left blank until a fetch.
func TestPageRendersColdCacheInline(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: Fees{OK: false}}), "/", "").Body.String()
    if !strings.Contains(body, "fees unavailable") {
        t.Error("cold cache should render \"fees unavailable\" into the page")
    }
    if strings.Contains(body, `class="tier"`) {
        t.Error("cold cache rendered fee tiers; there are no numbers to show")
    }
}

// The network card carries the four fields, rendered into the page rather than
// fetched, and sits between the fees card and the search field.
func TestPageRendersNetworkInline(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees(), n: liveNetwork()}), "/", "").Body.String()
    for _, want := range []string{"<h2>Blockchain</h2>", ">Coins<", ">Blocks<", ">Size<",
        ">Active nodes<", ">Transactions<", ">Addresses<",
        "20.1 M", "/ 21 M", "963 166", "869 GB", "31 751", "1.4 B", "1.5 B"} {
        if !strings.Contains(body, want) {
            t.Errorf("page did not render %q inline", want)
        }
    }
    var fees = strings.Index(body, `id="fees"`)
    var net = strings.Index(body, `id="network"`)
    var search = strings.Index(body, `id="q"`)
    if !(fees < net && net < search) {
        t.Errorf("wrong order: fees at %d, network at %d, search at %d — network belongs between them",
            fees, net, search)
    }
}

// html/template executes actions wherever they appear — including inside a CSS
// comment. A stray {{...}} referencing a field that does not exist fails the
// render mid-page, and because the writer has already emitted everything above
// it, the response is a silently truncated page rather than an error. Assert the
// page is whole.
func TestPageRendersToCompletion(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees(), n: liveNetwork()}), "/", "").Body.String()
    if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
        t.Fatalf("page is truncated — a template action probably failed mid-render; tail: %q",
            body[max(0, len(body)-120):])
    }
    for _, want := range []string{"</style>", `id="q"`, `data-panel="watches"`, "</nav>", "</body>"} {
        if !strings.Contains(body, want) {
            t.Errorf("page is missing %q, so it did not render to the end", want)
        }
    }
}

// A cold cache says so rather than rendering an empty card or zeroed counts.
func TestNetworkColdCache(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    if !strings.Contains(body, "network stats unavailable") {
        t.Error("a cold network cache should say so in the page")
    }
    if strings.Contains(body, `class="stat"`) {
        t.Error("cold cache rendered stat cells; there are no numbers to show")
    }
}

// /network is data, so it needs a signature like /fees does.
func TestNetworkNeedsInitData(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{n: liveNetwork()})
    if w := get(h, "/network", ""); w.Code != 401 {
        t.Errorf("unauthenticated /network = %d, want 401", w.Code)
    }
    var w = get(h, "/network", freshInitData("TESTTOKEN"))
    if w.Code != 200 || !strings.Contains(w.Body.String(), "31 751") {
        t.Errorf("signed /network = %d: %s", w.Code, w.Body.String())
    }
}

// The Market card carries the price and one column per period, rendered into
// the page, and sits between the Blockchain card and the search field.
func TestPageRendersMarketInline(t *testing.T) {
    var src = fakeSource{f: liveFees(), n: liveNetwork(), m: liveMarket()}
    var body = html.UnescapeString(get(handler(t, "TESTTOKEN", src), "/", "").Body.String())
    for _, want := range []string{"<h2>Market</h2>", "$66,202.00",
        ">1d<", ">1w<", ">1mo<", ">3mo<", ">1y<", ">5y<",
        "+2.5%", "-3.1%", "+512.9%"} {
        if !strings.Contains(body, want) {
            t.Errorf("page did not render %q inline", want)
        }
    }
    if n := strings.Count(body, `class="chg"`); n != 6 {
        t.Errorf("rendered %d change columns, want 6", n)
    }
    var net = strings.Index(body, `id="network"`)
    var mkt = strings.Index(body, `id="market"`)
    var search = strings.Index(body, `id="q"`)
    if !(net < mkt && mkt < search) {
        t.Errorf("wrong order: network at %d, market at %d, search at %d — market belongs between them",
            net, mkt, search)
    }
}

// A rise is green and a fall is red; a period with no baseline is neither.
func TestMarketColoursDirection(t *testing.T) {
    var src = fakeSource{m: Market{OK: true, Price: "$1.00", Changes: []Change{
        {Label: "1d", Pct: "+2.5%", Up: true},
        {Label: "1w", Pct: "-3.1%"},
        {Label: "5y", Pct: "—", Neutral: true},
    }}}
    var body = html.UnescapeString(get(handler(t, "TESTTOKEN", src), "/", "").Body.String())
    for _, want := range []string{`class="pct up">+2.5%`, `class="pct down">-3.1%`, `class="pct flat">—`} {
        if !strings.Contains(body, want) {
            t.Errorf("missing %q — direction must drive the colour", want)
        }
    }
}

// The card updates on its own event, with the same slow poll as a fallback.
func TestMarketRefreshesOnSSE(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{m: liveMarket()}), "/", "").Body.String()
    if !strings.Contains(body, `hx-trigger="sse:market, every 10m"`) {
        t.Error(`the market card must refresh on sse:market with a 10m fallback`)
    }
    var h = handler(t, "TESTTOKEN", fakeSource{m: liveMarket()})
    if w := get(h, "/market", ""); w.Code != 401 {
        t.Errorf("unauthenticated /market = %d, want 401", w.Code)
    }
    var w = get(h, "/market", freshInitData("TESTTOKEN"))
    if w.Code != 200 || !strings.Contains(w.Body.String(), "$66,202.00") {
        t.Errorf("signed /market = %d: %s", w.Code, w.Body.String())
    }
}

// A cold rate history says so rather than rendering an empty card.
func TestMarketColdCache(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    if !strings.Contains(body, "market data unavailable") {
        t.Error("with no rates the card should say so")
    }
}

// The Blocks tab lists recent blocks newest first, each row carrying the four
// fields, with the pager below.
func TestBlocksListRenders(t *testing.T) {
    var src = fakeSource{f: liveFees(), b: liveBlocks()}
    var body = get(handler(t, "TESTTOKEN", src), "/", "").Body.String()
    if n := strings.Count(body, `class="blk"`); n != 12 {
        t.Errorf("rendered %d block rows, want 12", n)
    }
    for _, want := range []string{"963268", "1.56 MB", "4 000 txs", "AntPool", "Prev &lt;", "&gt; Next"} {
        if !strings.Contains(body, want) {
            t.Errorf("block list is missing %q", want)
        }
    }
    // descending: the newest height must appear before the one below it
    if strings.Index(body, "963268") > strings.Index(body, "963267") {
        t.Error("blocks are not in descending order")
    }
    if strings.Contains(body, `class="go"`) {
        t.Error("the row chevron should be gone; the two link fields replace it")
    }
}

// Height and miner are the tappable fields, so both carry the link class — but
// an unattributed miner is not a link, since there is no pool to open.
func TestBlockRowLinks(t *testing.T) {
    var src = fakeSource{f: liveFees(), b: liveBlocks()}
    var body = get(handler(t, "TESTTOKEN", src), "/", "").Body.String()
    if n := strings.Count(body, `class="h lnk"`); n != 12 {
        t.Errorf("%d heights are links, want all 12", n)
    }
    if n := strings.Count(body, `class="mn lnk"`); n != 11 {
        t.Errorf("%d miners are links, want the 11 attributed ones", n)
    }
    if !strings.Contains(body, `<span class="mn">Unknown</span>`) {
        t.Error(`an unattributed miner must render as plain text, not a link`)
    }
    if !strings.Contains(body, "--tg-theme-link-color") {
        t.Error("links should take Telegram's link colour, not the button colour")
    }
}

// Paging moves the window and flips the buttons at the ends.
func TestBlocksPagination(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var first = get(h, "/blocks?page=0", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(first, "963268") || strings.Contains(first, "963256") {
        t.Error("page 0 should hold the newest twelve")
    }
    // the disabled attribute sits immediately before the label, so this pins it
    // to the Prev button rather than to "disabled" appearing anywhere on the page
    if !strings.Contains(first, `disabled>Prev &lt;</button>`) {
        t.Error("Prev must be disabled on the first page")
    }
    var second = get(h, "/blocks?page=1", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(second, "963256") {
        t.Error("page 1 should hold the next twelve")
    }
    if !strings.Contains(second, `hx-get="blocks?page=0"`) {
        t.Error("Prev on page 1 must link back to page 0")
    }
    if strings.Contains(second, `disabled>Prev &lt;</button>`) {
        t.Error("Prev must be enabled once past the first page")
    }
}

// The page number comes from a URL a user can edit, so nonsense must land on
// page 0 rather than erroring or panicking.
func TestBlocksBadPageFallsBackToFirst(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    for _, q := range []string{"", "?page=", "?page=abc", "?page=-3"} {
        var w = get(h, "/blocks"+q, freshInitData("TESTTOKEN"))
        if w.Code != 200 || !strings.Contains(w.Body.String(), "963268") {
            t.Errorf("%q gave %d; want page 0", q, w.Code)
        }
    }
}

// The list re-fetches on a new block, and keeps whichever page is showing —
// re-rendering the container is what carries the page number across the swap.
func TestBlocksRefreshKeepsPage(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var second = get(h, "/blocks?page=1", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(second, `hx-trigger="sse:blocks"`) {
        t.Error("the list must refresh on the sse:blocks event")
    }
    if !strings.Contains(second, `hx-get="blocks?page=1"`) {
        t.Error("the refreshed container must ask for the page it is showing, not page 0")
    }
}

// The pager reads "Prev < 1 > Next": the label counts from 1 the way a reader
// does, while the URL keeps its zero-based page so the contract is unchanged.
func TestPagerLabelsCountFromOne(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var first = get(h, "/blocks?page=0", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(first, `<span class="page">1</span>`) {
        t.Error("the first page should be labelled 1, not 0")
    }
    if strings.Contains(first, "page 0") || strings.Contains(first, ">page ") {
        t.Error(`the "page" word should be gone from the indicator`)
    }
    var second = get(h, "/blocks?page=1", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(second, `<span class="page">2</span>`) {
        t.Error("the second page should be labelled 2")
    }
    // the zero-based URL contract is unchanged
    if !strings.Contains(second, `hx-get="blocks?page=0"`) {
        t.Error("Prev on the second page must still link to page=0")
    }
}

// /blocks is data, so it needs a signature like the cards do.
func TestBlocksNeedsInitData(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    if w := get(h, "/blocks?page=0", ""); w.Code != 401 {
        t.Errorf("unauthenticated /blocks = %d, want 401", w.Code)
    }
}

// An empty cache says so rather than rendering an empty list with a pager.
func TestBlocksEmptyCache(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    if !strings.Contains(body, "no blocks cached yet") {
        t.Error("with no cached blocks the tab should say so")
    }
}

// The template block the page renders and the one the refresh returns must be
// the same block, or the card would change shape when it updates.
func TestInlineAndRefreshMatch(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees(), n: liveNetwork(), m: liveMarket()})
    var page = get(h, "/", "").Body.String()
    for _, path := range []string{"/fees", "/network", "/market"} {
        var fragment = strings.TrimSpace(get(h, path, freshInitData("TESTTOKEN")).Body.String())
        if !strings.Contains(page, fragment) {
            t.Errorf("the page does not embed the exact %s fragment:\n--- fragment ---\n%s", path, fragment)
        }
    }
}

// The fees card must let HTMX own its trigger. Firing a custom event from an
// inline script races HTMX's own DOMContentLoaded processing: the event is
// dispatched before HTMX has registered a listener for it, so it is lost and the
// card sits on "loading…" forever. Verified in a browser — with the custom-event
// version, zero requests were made; with hx-trigger="load", the request fires.
//
// The error handlers matter for the same symptom: HTMX does not swap a failed
// response, so without them any failure also leaves "loading…" on screen.
func TestFeesTriggerIsNotRacy(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{}), "/", "").Body.String()
    if !strings.Contains(body, `hx-trigger="sse:fees, every 10m"`) {
        t.Error(`the fees card must refresh on sse:fees, with a slow poll as a fallback`)
    }
    if strings.Contains(body, `hx-trigger="every 60s"`) {
        t.Error("the 60s poll should be gone: the server pushes now")
    }
    // The fallback exists because a proxy that buffers the stream would
    // otherwise leave the cards frozen with no sign anything is wrong.
    if !strings.Contains(body, "every 10m") {
        t.Error("no polling fallback: a silently broken SSE stream would freeze the cards")
    }
    if !strings.Contains(body, `sse-connect="events"`) {
        t.Error("nothing opens the event stream")
    }
    if strings.Contains(body, "htmx.trigger(") {
        t.Error("an inline htmx.trigger() races HTMX's DOM processing and is lost")
    }
    for _, want := range []string{"htmx:responseError", "htmx:sendError"} {
        if !strings.Contains(body, want) {
            t.Errorf("missing %s handler: a failed request would leave \"loading…\" on screen", want)
        }
    }
}

// HTMX is served from the binary, not a CDN, so the page stays self-contained
// apart from Telegram's own SDK.
func TestServesHtmx(t *testing.T) {
    var w = get(handler(t, "TESTTOKEN", fakeSource{}), "/htmx.min.js", "")
    if w.Code != 200 {
        t.Fatalf("GET /htmx.min.js = %d, want 200", w.Code)
    }
}

func TestRejectsOtherPaths(t *testing.T) {
    var w = get(handler(t, "TESTTOKEN", fakeSource{}), "/nope", "")
    if w.Code != 404 {
        t.Fatalf("GET /nope = %d, want 404", w.Code)
    }
}

// The whole point of the validation: without a valid signature there is no data.
func TestFeesNeedsInitData(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees()})
    var cases = []struct {
        name string
        data string
    }{
        {"absent", ""},
        {"garbage", "hash=deadbeef&auth_date=1"},
        {"signed with another token", freshInitData("SOMEONE-ELSES-TOKEN")},
    }
    for _, c := range cases {
        if w := get(h, "/fees", c.data); w.Code != 401 {
            t.Errorf("%s: got %d, want 401", c.name, w.Code)
        }
    }
}

// A tampered field must fail even though the hash itself is well-formed —
// otherwise the signature would be decoration.
func TestFeesRejectsTamperedField(t *testing.T) {
    var v, _ = url.ParseQuery(freshInitData("TESTTOKEN"))
    v.Set("user", `{"id":999,"first_name":"Mallory"}`)
    var w = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/fees", v.Encode())
    if w.Code != 401 {
        t.Fatalf("tampered user field accepted with %d, want 401", w.Code)
    }
}

// A signature stays valid forever on its own, so auth_date is what stops a
// payload lifted from a log or a shared link from working indefinitely.
func TestFeesRejectsStaleInitData(t *testing.T) {
    var old = signInitData("TESTTOKEN", map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Add(-48*time.Hour).Unix(), 10),
        "user":      `{"id":42}`,
    })
    if isValid(old, "TESTTOKEN") {
        t.Fatal("a 48h-old payload was accepted; auth_date is not being checked")
    }
    if !isValid(freshInitData("TESTTOKEN"), "TESTTOKEN") {
        t.Fatal("a fresh payload was rejected")
    }
}

// With a valid signature the fragment is HTML for HTMX to swap in — not JSON —
// carrying the same three tiers /fees prints.
func TestFeesRendersHTML(t *testing.T) {
    var w = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/fees", freshInitData("TESTTOKEN"))
    if w.Code != 200 {
        t.Fatalf("GET /fees = %d, want 200", w.Code)
    }
    if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
        t.Errorf("Content-Type = %q, want text/html", ct)
    }
    var body = w.Body.String()
    for _, want := range []string{"<h2>Network fees</h2>", ">Fast<", ">1 hour<", ">2+ hours<",
        ">12 <", ">4 <", ">1 <", "36 552"} {
        if !strings.Contains(body, want) {
            t.Errorf("fragment is missing %q in: %s", want, body)
        }
    }
}

// A cold cache says so rather than rendering zeros as if they were estimates.
func TestFeesColdCache(t *testing.T) {
    var w = get(handler(t, "TESTTOKEN", fakeSource{f: Fees{OK: false}}), "/fees", freshInitData("TESTTOKEN"))
    if !strings.Contains(w.Body.String(), "fees unavailable") {
        t.Fatalf("cold cache rendered %q", w.Body.String())
    }
}

// The stream carries event names only. That is what lets it sit outside
// requireInitData — EventSource cannot set headers — so it must never leak a
// figure that the authenticated endpoints are there to protect.
func TestEventStreamCarriesNoData(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees(), n: liveNetwork()})
    var r = httptest.NewRequest("GET", "/events", nil)
    var ctx, cancel = context.WithCancel(r.Context())
    r = r.WithContext(ctx)
    var w = httptest.NewRecorder()
    var done = make(chan struct{})
    go func() { h.ServeHTTP(w, r); close(done) }()
    // let the handler subscribe before notifying
    var deadline = time.Now().Add(2 * time.Second)
    for subscriberCount() == 0 && time.Now().Before(deadline) {
        time.Sleep(5 * time.Millisecond)
    }
    Notify("fees")
    Notify("network")
    time.Sleep(150 * time.Millisecond)
    cancel()
    <-done
    var body = w.Body.String()
    if !strings.Contains(body, "event: fees\ndata: 1\n\n") {
        t.Errorf("fees event not framed correctly; got %q", body)
    }
    if !strings.Contains(body, "event: network\ndata: 1\n\n") {
        t.Errorf("network event not framed correctly; got %q", body)
    }
    for _, leak := range []string{"31 751", "1.4 B", "869 GB", "sat/vB", "<div"} {
        if strings.Contains(body, leak) {
            t.Errorf("the stream leaked %q — it must carry names only", leak)
        }
    }
    if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
        t.Errorf("Content-Type = %q, want text/event-stream", ct)
    }
}

// A disconnected client must not be left in the subscriber set, or every page
// load would leak a channel for the life of the process.
func TestEventStreamUnsubscribesOnDisconnect(t *testing.T) {
    var before = subscriberCount()
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var r = httptest.NewRequest("GET", "/events", nil)
    var ctx, cancel = context.WithCancel(r.Context())
    r = r.WithContext(ctx)
    var done = make(chan struct{})
    go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
    var deadline = time.Now().Add(2 * time.Second)
    for subscriberCount() == before && time.Now().Before(deadline) {
        time.Sleep(5 * time.Millisecond)
    }
    if subscriberCount() != before+1 {
        t.Fatalf("subscriber not registered: %d, want %d", subscriberCount(), before+1)
    }
    cancel()
    <-done
    if subscriberCount() != before {
        t.Fatalf("subscriber left behind after disconnect: %d, want %d", subscriberCount(), before)
    }
}

// Notify must not block when a client is not reading, or a background refresh
// goroutine in main would stall behind a stuck page.
func TestNotifyDoesNotBlockOnSlowClient(t *testing.T) {
    var ch = subscribe()
    defer unsubscribe(ch)
    var done = make(chan struct{})
    go func() {
        for i := 0; i < 100; i++ { Notify("fees") }
        close(done)
    }()
    select {
    case <-done:
    case <-time.After(2 * time.Second):
        t.Fatal("Notify blocked on a client that is not reading")
    }
}

// A block height in the list links into the details page, swapping the whole
// container so the Blocks tab stays selected — and carrying the page it was on,
// which is what Back needs.
func TestBlockHeightLinksToDetails(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks(), d: liveBlockInfo()})
    var list = get(h, "/blocks?page=1", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(list, `hx-get="block?height=963256&page=1"`) {
        t.Error("the height does not link to its details page, carrying the current page")
    }
    if !strings.Contains(list, `hx-target="#blocklist" hx-swap="outerHTML"`) {
        t.Error("the link must replace the list container, not swap into it")
    }
}

// The details page: the title, the Back button on the same row, and the same
// lines the bot's /info prints for a block.
func TestBlockDetailsRender(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks(), d: liveBlockInfo()})
    // html/template escapes "+" as &#43;, so "Reward + fees" needs unescaping
    var body = html.UnescapeString(get(h, "/block?height=963268&page=2", freshInitData("TESTTOKEN")).Body.String())
    if !strings.Contains(body, "<h1>Block 963 268</h1>") {
        t.Errorf("missing the title: %s", body)
    }
    if !strings.Contains(body, `<div id="blocklist" class="blocklist det">`) {
        t.Error("the block page must replace the list container, so the Blocks tab stays selected")
    }
    for _, want := range []string{">Hash<", ">Miner<", ">Difficulty<", ">Reward + fees<",
        "0000ab...9f21cd", "AntPool", "142.34 T", "325 118 004 sats"} {
        if !strings.Contains(body, want) {
            t.Errorf("details page is missing %q", want)
        }
    }
    // Back sits on the title row and returns to the page the reader came from
    var head = body[strings.Index(body, `class="head"`):strings.Index(body, `class="fields"`)]
    if !strings.Contains(head, "< Back") {
        t.Errorf("no Back button on the title row: %s", head)
    }
    if !strings.Contains(head, `hx-get="blocks?page=2"`) {
        t.Errorf("Back returns to the wrong page: %s", head)
    }
    // Back sits to the left of the title, which is centred between two equal
    // sides rather than filling the space the button leaves
    if strings.Index(head, "< Back") > strings.Index(head, "<h1>") {
        t.Errorf("Back should come before the title: %s", head)
    }
    if !strings.Contains(head, `class="ghost"`) {
        t.Errorf("the title needs a hidden button opposite Back to stay centred: %s", head)
    }
}

// A pair with no value is a heading for the lines under it, not a field with a
// blank value — that is how blockPairs builds "Fees" and "Tx sizes".
func TestBlockDetailsMarksHeadings(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{d: liveBlockInfo()})
    var body = get(h, "/block?height=963268", freshInitData("TESTTOKEN")).Body.String()
    if n := strings.Count(body, `class="f hd"`); n != 2 {
        t.Errorf("%d headings, want 2 (Fees and Tx sizes)", n)
    }
    if !strings.Contains(body, `class="f hd"><span class="lbl">Fees</span>`) {
        t.Error("Fees should be a heading")
    }
    if !strings.Contains(body, `class="f"><span class="lbl">lowest</span>`) {
        t.Error("lowest should be an ordinary field")
    }
}

// A height the node has no block for says so rather than rendering an empty
// field list, and still titles itself with what was asked for.
func TestBlockDetailsNotFound(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{d: liveBlockInfo()})
    var body = get(h, "/block?height=99999999", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(body, "nothing found") {
        t.Errorf("an unknown height should say so: %s", body)
    }
    if !strings.Contains(body, "<h1>Block 99999999</h1>") {
        t.Error("the title should still name the height that was asked for")
    }
    if strings.Contains(body, `class="fields"`) {
        t.Error("nothing to show, so there should be no field list")
    }
}

// The height comes from a URL a user can edit.
func TestBlockDetailsRejectsBadHeight(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{d: liveBlockInfo()})
    for _, q := range []string{"", "?height=", "?height=abc", "?height=-4"} {
        if w := get(h, "/block"+q, freshInitData("TESTTOKEN")); w.Code != 400 {
            t.Errorf("/block%s = %d, want 400", q, w.Code)
        }
    }
}

// Searching a block height from Home opens its details, and HX-Trigger is what
// moves the reader to the Blocks tab — the fragment lands in that panel.
func TestSearchOpensBlock(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks(), d: liveBlockInfo()})
    var w = get(h, "/search?q=963268", freshInitData("TESTTOKEN"))
    if w.Code != http.StatusSeeOther {
        t.Fatalf("/search = %d, want a redirect to the block page", w.Code)
    }
    if loc := w.Header().Get("Location"); loc != "/block?height=963268" {
        t.Errorf("Location = %q, want /block?height=963268", loc)
    }
    if trig := get(h, "/block?height=963268", freshInitData("TESTTOKEN")).Header().Get("HX-Trigger"); trig != `{"showtab":"blocks"}` {
        t.Errorf("HX-Trigger = %q; without it a search from Home leaves the Home tab showing", trig)
    }
}

// Whitespace around a pasted height must not defeat the lookup.
func TestSearchTrimsQuery(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{d: liveBlockInfo()})
    var w = get(h, "/search?q=%20963268%20", freshInitData("TESTTOKEN"))
    if w.Header().Get("Location") != "/block?height=963268" {
        t.Errorf("a padded height did not resolve: %d %q", w.Code, w.Header().Get("Location"))
    }
}

// Only block heights are understood so far. Anything else answers 204, which
// HTMX does not swap, so the page is left alone rather than being wiped.
func TestSearchIgnoresEmptyQuery(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{d: liveBlockInfo()})
    var w = get(h, "/search?q=", freshInitData("TESTTOKEN"))
    if w.Code != http.StatusNoContent {
        t.Errorf("an empty search = %d, want 204 so HTMX leaves the page alone", w.Code)
    }
    if w.Body.Len() != 0 {
        t.Error("an empty search returned a body; HTMX would swap it in")
    }
}

// The three kinds are told apart in the order info() uses: the 64-hex shape
// first, because a string of 64 digits is also a valid height, then a height,
// then an address as the catch-all.
func TestSearchClassifiesQuery(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var cases = []struct{ q, want string }{
        {liveTxid, "tx?id=" + liveTxid},
        {strings.Repeat("0", 64), "tx?id=" + strings.Repeat("0", 64)},
        {"963268", "block?height=963268"},
        {liveAddress, "address?a=" + liveAddress},
        {"not a block", "address?a=not+a+block"},
    }
    for _, c := range cases {
        var w = get(h, "/search?q="+url.QueryEscape(c.q), freshInitData("TESTTOKEN"))
        if w.Code != http.StatusSeeOther {
            t.Errorf("/search?q=%q = %d, want a redirect", c.q, w.Code)
        }
        if loc := w.Header().Get("Location"); loc != "/"+c.want {
            t.Errorf("/search?q=%q went to %q, want /%s", c.q, loc, c.want)
        }
    }
}

// A transaction opens on the Blocks tab, titled with the short txid and with
// Back to the block list.
func TestTxDetailsRender(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks(), t: liveTx()})
    var body = get(h, "/tx?id="+liveTxid, freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(body, "<h1>32e43e...870b16</h1>") {
        t.Errorf("missing the short-txid title: %s", body)
    }
    if !strings.Contains(body, `<div id="blocklist" class="blocklist det">`) {
        t.Error("a transaction belongs in the Blocks tab's container")
    }
    if rt := get(h, "/tx?id="+liveTxid, freshInitData("TESTTOKEN")).Header().Get("HX-Retarget"); rt != "#blocklist" {
        t.Errorf("HX-Retarget = %q, want #blocklist", rt)
    }
    for _, want := range []string{">Confirmations<", ">Amount<", ">Fee<", ">Size<",
        ">Inputs<", ">Outputs<", "412 (block #963268)", "9 990 000 sats"} {
        if !strings.Contains(body, want) {
            t.Errorf("transaction page is missing %q", want)
        }
    }
    var head = body[strings.Index(body, `class="head"`):strings.Index(body, `class="fields"`)]
    if !strings.Contains(head, `hx-get="blocks?page=0"`) {
        t.Errorf("Back should return to the block list: %s", head)
    }
    if strings.Index(head, "&lt; Back") > strings.Index(head, "<h1>") {
        t.Errorf("Back should come before the title: %s", head)
    }
}

// An address opens on the Addresses tab, titled with the short address and with
// Back to that tab's own content.
func TestAddressDetailsRender(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var w = get(h, "/address?a="+liveAddress, freshInitData("TESTTOKEN"))
    var body = w.Body.String()
    if !strings.Contains(body, "<h1>bc1qxy...dayd2g</h1>") {
        t.Errorf("missing the short-address title: %s", body)
    }
    if !strings.Contains(body, `<div id="addrpanel" class="blocklist det">`) {
        t.Error("an address belongs in the Addresses tab's container, not the block list")
    }
    if trig := w.Header().Get("HX-Trigger"); trig != `{"showtab":"addresses"}` {
        t.Errorf("HX-Trigger = %q; a searched address must move the reader to Addresses", trig)
    }
    // the one search field names #blocklist as its target, so the response has
    // to correct it or an address page replaces the block list
    if rt := w.Header().Get("HX-Retarget"); rt != "#addrpanel" {
        t.Errorf("HX-Retarget = %q; without it the address page lands in the Blocks tab", rt)
    }
    for _, want := range []string{">Type<", ">Balance<", ">Total received<", ">Total fees<",
        ">Activity period<", "segwit (bech32)", "0.09990000 BTC"} {
        if !strings.Contains(body, want) {
            t.Errorf("address page is missing %q", want)
        }
    }
    var head = body[strings.Index(body, `class="head"`):strings.Index(body, `class="fields"`)]
    if !strings.Contains(head, `hx-get="addresses"`) {
        t.Errorf("Back should return to the Addresses tab: %s", head)
    }
}

// Back from an address restores the tab's own content in the same slot.
func TestAddressesBackTarget(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var w = get(h, "/addresses", freshInitData("TESTTOKEN"))
    if w.Code != 200 {
        t.Fatalf("GET /addresses = %d, want 200", w.Code)
    }
    if !strings.Contains(w.Body.String(), `id="addrpanel"`) {
        t.Errorf("the fragment must carry the slot it replaces: %s", w.Body.String())
    }
    if !strings.Contains(w.Body.String(), "Addresses — coming soon") {
        t.Error("Back should restore the tab's placeholder")
    }
}

// Something that is not an address at all says so, rather than presenting an
// empty history as fact.
func TestAddressDetailsRejectsNonAddress(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: map[string]Info{
        "nonsense": {Title: "nonsense", Rows: []Field{{Label: "this does not look like a Bitcoin address"}}}}})
    var body = get(h, "/address?a=nonsense", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(body, "this does not look like a Bitcoin address") {
        t.Errorf("a non-address should say so: %s", body)
    }
    if strings.Contains(body, `class="fields"`) {
        t.Error("nothing to show, so there should be no field list")
    }
}

// Both new ids come from URLs a user can edit.
func TestDetailsRejectBadIds(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    for _, p := range []string{"/tx", "/tx?id=", "/tx?id=abc", "/tx?id=" + strings.Repeat("z", 64), "/address", "/address?a="} {
        if w := get(h, p, freshInitData("TESTTOKEN")); w.Code != 400 {
            t.Errorf("%s = %d, want 400", p, w.Code)
        }
    }
}

// Both are data endpoints, so both need a signature.
func TestBlockAndSearchNeedInitData(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks(), d: liveBlockInfo()})
    for _, p := range []string{"/block?height=963268", "/search?q=963268"} {
        if w := get(h, p, ""); w.Code != 401 {
            t.Errorf("unauthenticated %s = %d, want 401", p, w.Code)
        }
    }
}

// The search field must actually be wired, and send its value under a name the
// server reads.
func TestSearchFieldIsWired(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{}), "/", "").Body.String()
    for _, want := range []string{`name="q"`, `hx-get="search"`,
        `hx-trigger="keyup[key=='Enter']"`, `hx-target="#blocklist"`} {
        if !strings.Contains(body, want) {
            t.Errorf("the search field is missing %q", want)
        }
    }
    if !strings.Contains(body, `document.body.addEventListener("showtab"`) {
        t.Error("nothing listens for showtab, so a search would not switch tabs")
    }
}

// The Watches tab lists the caller's own watches, both kinds, shortened and
// linked to the same pages a search opens.
func TestWatchesListsBoth(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{w: liveWatches()})
    var body = get(h, "/watches", freshInitData("TESTTOKEN")).Body.String()
    for _, want := range []string{">Addresses<", ">Transactions<",
        ">bc1qxy...hx0wlh<", ">32e43e...870b16<", ">John<"} {
        if !strings.Contains(body, want) {
            t.Errorf("the watch list is missing %q: %s", want, body)
        }
    }
    // each row links to the details page for its kind, carrying the full id
    if !strings.Contains(body, `hx-get="address?a=`+liveAddress+`"`) {
        t.Error("a watched address must open the address page, with the full id")
    }
    if !strings.Contains(body, `hx-get="tx?id=`+liveTxid+`"`) {
        t.Error("a watched transaction must open the transaction page, with the full id")
    }
}

// The one thing that must never break: a watch list is per-user, and the caches
// are keyed by URL — identical for everyone. Two users must not see each other.
func TestWatchesAreNotSharedBetweenUsers(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{w: liveWatches()})
    var mine = signInitData("TESTTOKEN", map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Unix(), 10),
        "user":      `{"id":42,"first_name":"Pin"}`})
    var theirs = signInitData("TESTTOKEN", map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Unix(), 10),
        "user":      `{"id":99,"first_name":"Mallory"}`})
    var a = get(h, "/watches", mine).Body.String()
    var b = get(h, "/watches", theirs).Body.String()
    if !strings.Contains(a, "bc1qxy...hx0wlh") || strings.Contains(a, "1A1zP1...DivfNa") {
        t.Errorf("user 42 got the wrong list: %s", a)
    }
    if !strings.Contains(b, "1A1zP1...DivfNa") || strings.Contains(b, "bc1qxy...hx0wlh") {
        t.Errorf("user 99 was served user 42's watches — the list must never be cached: %s", b)
    }
    // and back again, in case the first response was cached under the URL
    if again := get(h, "/watches", mine).Body.String(); again != a {
        t.Error("user 42's second request differed; something is caching per-URL")
    }
}

// chatOf reads the id Telegram signs, which is what scopes the list.
func TestChatOfReadsSignedUser(t *testing.T) {
    if got := chatOf(freshInitData("TESTTOKEN")); got != 42 {
        t.Errorf("chatOf = %d, want 42", got)
    }
    for _, bad := range []string{"", "user=notjson", "auth_date=1"} {
        if got := chatOf(bad); got != 0 {
            t.Errorf("chatOf(%q) = %d, want 0", bad, got)
        }
    }
}

// Watching nothing is a different answer from the lookup failing.
func TestWatchesEmptyAndFailed(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{w: map[int64]Watches{42: {OK: true}}})
    if body := get(h, "/watches", freshInitData("TESTTOKEN")).Body.String(); !strings.Contains(body, "not watching anything yet") {
        t.Errorf("an empty list should say so: %s", body)
    }
    var broken = handler(t, "TESTTOKEN", fakeSource{w: map[int64]Watches{42: {OK: false}}})
    if body := get(broken, "/watches", freshInitData("TESTTOKEN")).Body.String(); !strings.Contains(body, "watches unavailable") {
        t.Errorf("a failed lookup should say so, not claim an empty list: %s", body)
    }
}

// The list is per-user, so it must need a signature and must never be rendered
// into the shell page, which is one cached copy served to every visitor.
func TestWatchesNeverInThePage(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees(), w: liveWatches()})
    if w := get(h, "/watches", ""); w.Code != 401 {
        t.Errorf("unauthenticated /watches = %d, want 401", w.Code)
    }
    var body = get(h, "/", "").Body.String()
    for _, leak := range []string{"bc1qxy...hx0wlh", "32e43e...870b16", "John", liveAddress} {
        if strings.Contains(body, leak) {
            t.Errorf("the shell page carries %q — it is cached and shared by every visitor", leak)
        }
    }
    if !strings.Contains(body, `id="watchpanel"`) {
        t.Error("the page needs the empty container for the fragment to replace")
    }
}

// The container in the page and the one the fragment returns must ask for the
// same thing, or the tab would stop refreshing after its first swap.
func TestWatchPanelWiringMatches(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{w: liveWatches()})
    var page = get(h, "/", "").Body.String()
    var frag = get(h, "/watches", freshInitData("TESTTOKEN")).Body.String()
    for _, want := range []string{`hx-get="watches"`, `hx-trigger="watchtab from:body"`, `hx-swap="outerHTML"`} {
        if !strings.Contains(page, want) {
            t.Errorf("the page's container is missing %q", want)
        }
        if !strings.Contains(frag, want) {
            t.Errorf("the fragment is missing %q, so the tab would stop refreshing", want)
        }
    }
    if !strings.Contains(page, `dispatchEvent(new Event("watchtab"))`) {
        t.Error("nothing fires watchtab, so opening the tab would never fetch")
    }
    if !strings.Contains(page, "Watches are only available inside Telegram") {
        t.Error("outside Telegram the tab must say so rather than failing silently")
    }
}

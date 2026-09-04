package app

import "context"
import "html"
import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "encoding/json"
import "fmt"
import "net"
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
    b []Block
    d map[int64]Info
    t map[string]Info
    a map[string]Info
    w map[int64]Watches
}

func (s fakeSource) Fees() Fees       { return s.f }
func (s fakeSource) Network() Network { return s.n }
func (s fakeSource) Market(lang string) Market { return s.m }
func (s fakeSource) Blocks(lang string, rng Range) Blocks { return window(s.b, rng) }

func (s fakeSource) BlockInfo(lang string, height int64) Info {
    if d, ok := s.d[height]; ok { return d }
    return Info{Title: "Block " + strconv.FormatInt(height, 10)}
}

func (s fakeSource) TxInfo(lang, txid string) Info {
    if d, ok := s.t[txid]; ok { return d }
    return Info{Title: txid[:6] + "..." + txid[58:]}
}

func (s fakeSource) MinerInfo(lang, name string) Info {
    if name != "AntPool" { return Info{Title: name} }
    return Info{OK: true, Title: name, Rows: []Field{
        {Label: "Blocks mined", Value: "22 blocks"},
        {Label: "Reward", Value: "69.14 BTC"},
        {Label: "Fees", Value: "0.39 BTC"},
        {Label: "Consumption", Value: "2 GW"},
    }}
}

// watched is the fake's per-chat watch set, so the tests can prove the button
// reflects the caller and not whoever asked first.
var watched = map[int64]map[string]bool{}

func (s fakeSource) Watching(chat int64, kind, id string) bool { return watched[chat][id] }

func (s fakeSource) SetWatch(chat int64, kind, id string, on bool) (bool, error) {
    if watched[chat] == nil { watched[chat] = map[string]bool{} }
    if on {
        watched[chat][id] = true
    } else {
        delete(watched[chat], id)
    }
    return on, nil
}

// aliases is what SetAlias recorded, so a test can prove the alias reached the
// Source and under which id.
var aliases = map[int64]map[string]string{}

func (s fakeSource) SetAlias(chat int64, kind, id, alias string) (bool, error) {
    if aliases[chat] == nil { aliases[chat] = map[string]string{} }
    aliases[chat][id] = alias
    return true, nil
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

func (s fakeSource) AddrInfo(lang, addr string) Info {
    if d, ok := s.a[addr]; ok { return d }
    return Info{Title: addr}
}

// liveTx and liveAddr mirror what main builds from txPairs and addrPairs.
func liveTx() map[string]Info {
    return map[string]Info{liveTxid: {OK: true, Title: "32e43e...870b16", Rows: []Field{
        {Label: "Confirmations", Value: "412 (block #963268)", Parts: []Part{
            {Text: "412 (block "}, {Text: "#963268", Id: "963268"}, {Text: ")"}}},
        {Label: "Amount", Value: "9 990 000 sats (≈ $6,614)"},
        {Label: "Fee", Value: "1 410 sats (10.0 sat/vB)"},
        {Label: "Size", Value: "223 B (141 vB)"},
        {Label: "Inputs", Value: "bc1qxy...dayd2g", Parts: []Part{
            {Text: "bc1qxy...dayd2g", Id: liveAddress}}},
        {Label: "Outputs", Value: "1A1zP1...DivfNa, bc1qxy...dayd2g", Parts: []Part{
            {Text: "1A1zP1...DivfNa", Id: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
            {Text: ", "},
            {Text: "bc1qxy...dayd2g", Id: liveAddress}}},
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

// fakeBatch and fakeMaxRows are what main's blocksPerPage and blocksMaxRows are
// to the real list: one batch, and the depth a restored list stops at.
const fakeBatch = 12
const fakeMaxRows = 24

// Two batches of blocks, newest first — the order the bucket's cursor hands them
// to main, so the fixture is a chain rather than a canned page.
func liveBlocks() []Block {
    var all []Block
    for i := 0; i < 24; i++ {
        var h = int64(963268 - i)
        var row = Block{Height: strconv.FormatInt(h, 10), Num: h,
            Size: "1.56 MB", Txs: "4 000 txs", Miner: "AntPool", MinerKnown: true}
        // the last row of each batch is unattributed, so every batch carries
        // both the linked and the plain form of the miner field
        if i%fakeBatch == 11 { row.Miner, row.MinerKnown = "Unknown", false }
        // a pool name with a space, so the link's URL encoding is exercised
        if i%fakeBatch == 5 { row.Miner = "SBI Crypto" }
        all = append(all, row)
    }
    return all
}

// window answers a Range out of that chain exactly as main's cursor walk does,
// so what the tests drive is the paging contract itself and not a fixture that
// happens to agree with it.
func window(all []Block, rng Range) Blocks {
    var out = Blocks{Top: rng.After}
    var limit = fakeBatch
    if rng.Down > 0 { limit = fakeMaxRows }
    var i = 0
    if rng.Before > 0 {
        for i < len(all) && all[i].Num >= rng.Before { i++ }
    }
    for ; i < len(all); i++ {
        if rng.After > 0 && all[i].Num <= rng.After { break }
        if len(out.Rows) == limit {
            out.More = true
            break
        }
        out.Rows = append(out.Rows, all[i])
        if rng.Down > 0 && all[i].Num <= rng.Down {
            out.More = i + 1 < len(all)
            break
        }
    }
    if len(out.Rows) > 0 { out.Top, out.Next = out.Rows[0].Num, out.Rows[len(out.Rows)-1].Num }
    out.OK = len(out.Rows) > 0
    return out
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
    invalidateAll()
    watched = map[int64]map[string]bool{}
    aliases = map[int64]map[string]string{}
    var srv = Start("127.0.0.1:0", token, src)
    t.Cleanup(func() { srv.Close() })
    return srv.Handler
}

// failingSource stands in for a store that cannot save, so the button's error
// path has something to render.
type failingSource struct{ fakeSource }

func (failingSource) SetWatch(chat int64, kind, id string, on bool) (bool, error) {
    return false, errFailed
}

var errFailed = fmt.Errorf("store unavailable")

func post(h http.Handler, path, initData string) *httptest.ResponseRecorder {
    var r = httptest.NewRequest("POST", path, nil)
    if initData != "" { r.Header.Set("X-Telegram-Init-Data", initData) }
    var w = httptest.NewRecorder()
    h.ServeHTTP(w, r)
    return w
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
    for _, want := range []string{"<h2>Network fees</h2>", ">Fastest<", ">~ 1 hour<", ">2+ hours<",
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
// fields, with the sentinel that loads the next batch below them.
func TestBlocksListRenders(t *testing.T) {
    var src = fakeSource{f: liveFees(), b: liveBlocks()}
    var body = get(handler(t, "TESTTOKEN", src), "/", "").Body.String()
    if n := strings.Count(body, `class="blk"`); n != 12 {
        t.Errorf("rendered %d block rows, want 12", n)
    }
    for _, want := range []string{"963268", "1.56 MB", "4 000 txs", "AntPool"} {
        if !strings.Contains(body, want) {
            t.Errorf("block list is missing %q", want)
        }
    }
    for _, gone := range []string{"Prev &lt;", "&gt; Next", `class="pager"`} {
        if strings.Contains(body, gone) {
            t.Errorf("the pager is gone; %q should not be rendered", gone)
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

// Scrolling to the sentinel below the rows appends the next batch, which
// continues below the lowest height on screen — a height, not a page number, so
// a block mined mid-scroll cannot shift the boundary and repeat a row.
func TestBlocksInfiniteScroll(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var data = freshInitData("TESTTOKEN")
    var first = get(h, "/blocks", data).Body.String()
    if !strings.Contains(first, "963268") || strings.Contains(first, "963256") {
        t.Error("the list should open on the newest twelve")
    }
    if !strings.Contains(first, `hx-get="moreblocks?before=963257"`) {
        t.Errorf("no sentinel continuing below the last row: %s", first)
    }
    if !strings.Contains(first, `hx-trigger="intersect once"`) {
        t.Error(`the sentinel must load when scrolled into view ("intersect once", not "revealed" — the rows are their own scroller)`)
    }
    var next = get(h, "/moreblocks?before=963257", data).Body.String()
    if !strings.Contains(next, "963256") || strings.Contains(next, "963268") {
        t.Errorf("the next batch should hold the twelve below 963257: %s", next)
    }
    // a fragment, not a container: it is swapped in where the sentinel was
    if strings.Contains(next, `id="blocklist"`) {
        t.Error("an appended batch must not carry the list container")
    }
    // the fixture ends there, so nothing more is offered
    if strings.Contains(next, "moreblocks?before=") {
        t.Error("the sentinel should be gone once the list is exhausted")
    }
}

// The heights come from a URL a user can edit, so nonsense must land on the
// newest blocks or be refused rather than erroring or panicking.
func TestBlocksBadHeightsAreSafe(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var data = freshInitData("TESTTOKEN")
    for _, q := range []string{"", "?down=", "?down=abc", "?down=-3", "?down=0"} {
        var w = get(h, "/blocks"+q, data)
        if w.Code != 200 || !strings.Contains(w.Body.String(), "963268") {
            t.Errorf("/blocks%s gave %d; want the newest blocks", q, w.Code)
        }
    }
    for _, p := range []string{"/moreblocks", "/moreblocks?before=abc", "/moreblocks?before=-1",
        "/newblocks", "/newblocks?after=abc", "/newblocks?after=0"} {
        if w := get(h, p, data); w.Code != 400 {
            t.Errorf("%s gave %d, want 400", p, w.Code)
        }
    }
}

// A new block is prepended above the rows already on screen rather than
// re-rendering the list, which is what keeps a reader who has scrolled where
// they are. The sentinel replaces itself, so it comes back with the new height.
func TestNewBlocksPrepend(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var data = freshInitData("TESTTOKEN")
    var list = get(h, "/blocks", data).Body.String()
    if !strings.Contains(list, `hx-get="newblocks?after=963268"`) {
        t.Errorf("no sentinel watching for blocks above the newest: %s", list)
    }
    if !strings.Contains(list, `hx-trigger="sse:blocks, every 10m"`) {
        t.Error("the sentinel must refresh on the event, with the slow poll as a fallback")
    }
    // a reader whose list topped out two blocks ago gets exactly those two
    var w = get(h, "/newblocks?after=963266", data)
    var got = w.Body.String()
    if n := strings.Count(got, `class="blk"`); n != 2 {
        t.Errorf("prepended %d rows, want the 2 new ones: %s", n, got)
    }
    if !strings.Contains(got, `hx-get="newblocks?after=963268"`) {
        t.Error("the replacement sentinel must ask about the new newest height")
    }
    // the sentinel comes first, or the next block would land below these two
    if strings.Index(got, "newblocks?after=") > strings.Index(got, `class="blk"`) {
        t.Errorf("the sentinel must stay above the rows it prepends: %s", got)
    }
    if strings.Contains(got, "moreblocks?before=") {
        t.Error("a prepend must not carry a second bottom sentinel")
    }
    if rt := w.Header().Get("HX-Retarget"); rt != "" {
        t.Errorf("a prepend retargeted to %q; it replaces the sentinel in place", rt)
    }
}

// Nothing new is a legitimate answer — the sentinel just goes back to waiting on
// the same height.
func TestNewBlocksWithNothingNew(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var got = get(h, "/newblocks?after=963268", freshInitData("TESTTOKEN")).Body.String()
    if strings.Contains(got, `class="blk"`) {
        t.Errorf("nothing was mined, so nothing should be prepended: %s", got)
    }
    if !strings.Contains(got, `hx-get="newblocks?after=963268"`) {
        t.Error("the sentinel must come back watching the same height")
    }
}

// More new blocks than one batch holds would leave a gap between them and the
// rows on screen, so the whole list is replaced instead.
func TestNewBlocksOverflowReplacesTheList(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var w = get(h, "/newblocks?after=963250", freshInitData("TESTTOKEN"))
    if rt := w.Header().Get("HX-Retarget"); rt != "#blocklist" {
        t.Errorf("HX-Retarget = %q, want #blocklist — a partial prepend would leave a hole", rt)
    }
    var got = w.Body.String()
    if !strings.Contains(got, `id="blocklist"`) {
        t.Errorf("the whole list should come back, not a fragment: %s", got)
    }
    if !strings.Contains(got, "963268") {
        t.Error("the replacement list should start at the newest block")
    }
}

// /blocks is data, so it needs a signature like the cards do.
func TestBlocksNeedsInitData(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    for _, p := range []string{"/blocks", "/moreblocks?before=963257", "/newblocks?after=963268"} {
        if w := get(h, p, ""); w.Code != 401 {
            t.Errorf("unauthenticated %s = %d, want 401", p, w.Code)
        }
    }
}

// An empty cache says so — and keeps watching, since there is no row for the
// sentinel to sit above until the first block is cached.
func TestBlocksEmptyCache(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    if !strings.Contains(body, "no blocks cached yet") {
        t.Error("with no cached blocks the tab should say so")
    }
    var empty = body[strings.Index(body, "no blocks cached yet")-260:]
    if !strings.Contains(empty[:strings.Index(empty, "no blocks cached yet")], `hx-trigger="sse:blocks, every 10m"`) {
        t.Errorf("an empty list must still reload when a block arrives: %s", empty[:260])
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
    for _, want := range []string{"<h2>Network fees</h2>", ">Fastest<", ">~ 1 hour<", ">2+ hours<",
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
    for subscribes() == 0 && time.Now().Before(deadline) {
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
    var before = subscribes()
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var r = httptest.NewRequest("GET", "/events", nil)
    var ctx, cancel = context.WithCancel(r.Context())
    r = r.WithContext(ctx)
    var done = make(chan struct{})
    go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
    var deadline = time.Now().Add(2 * time.Second)
    for subscribes() == before && time.Now().Before(deadline) {
        time.Sleep(5 * time.Millisecond)
    }
    if subscribes() != before+1 {
        t.Fatalf("subscriber not registered: %d, want %d", subscribes(), before+1)
    }
    cancel()
    <-done
    if subscribes() != before {
        t.Fatalf("subscriber left behind after disconnect: %d, want %d", subscribes(), before)
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
// container so the Blocks tab stays selected — and carrying its own height,
// which is the mark Back restores the list to.
func TestBlockHeightLinksToDetails(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks(), d: liveBlockInfo()})
    var list = get(h, "/blocks", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(list, `hx-get="block?height=963268&down=963268"`) {
        t.Error("the height does not link to its details page, carrying where the reader was")
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
    var body = html.UnescapeString(get(h, "/block?height=963268&down=963257", freshInitData("TESTTOKEN")).Body.String())
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
    if !strings.Contains(head, `hx-get="blocks?down=963257&to=blocks"`) {
        t.Errorf("Back returns to the wrong place: %s", head)
    }
    // and scrolls the row the reader tapped back into view, since a list with no
    // pages has nothing else to return them to
    if !strings.Contains(head, `hx-swap="outerHTML show:#blk963257:top"`) {
        t.Errorf("Back does not restore the reader's position: %s", head)
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
    if loc := w.Header().Get("Location"); loc != "/block?height=963268&from=home" {
        t.Errorf("Location = %q, want /block?height=963268&from=home", loc)
    }
    if trig := get(h, "/block?height=963268", freshInitData("TESTTOKEN")).Header().Get("HX-Trigger"); trig != `{"showtab":"blocks"}` {
        t.Errorf("HX-Trigger = %q; without it a search from Home leaves the Home tab showing", trig)
    }
}

// Whitespace around a pasted height must not defeat the lookup.
func TestSearchTrimsQuery(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{d: liveBlockInfo()})
    var w = get(h, "/search?q=%20963268%20", freshInitData("TESTTOKEN"))
    if w.Header().Get("Location") != "/block?height=963268&from=home" {
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
        {liveTxid, "tx?id=" + liveTxid + "&from=home"},
        {strings.Repeat("0", 64), "tx?id=" + strings.Repeat("0", 64) + "&from=home"},
        {"963268", "block?height=963268&from=home"},
        {liveAddress, "address?a=" + liveAddress + "&from=home"},
        {"not a block", "address?a=not+a+block&from=home"},
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
    // the confirmations line now carries a tappable block number, so it renders
    // in pieces — the text a reader sees is unchanged, the markup is not
    for _, want := range []string{">Confirmations<", ">Amount<", ">Fee<", ">Size<",
        ">Inputs<", ">Outputs<", "412 (block ", ">#963268<", "9 990 000 sats"} {
        if !strings.Contains(body, want) {
            t.Errorf("transaction page is missing %q", want)
        }
    }
    var head = body[strings.Index(body, `class="head"`):strings.Index(body, `class="fields"`)]
    // Back comes from a template *value*, so html/template escapes its "&" —
    // unlike the list's links, where the "&" is literal template text
    if !strings.Contains(head, `hx-get="blocks?to=blocks"`) {
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
    if !strings.Contains(head, `hx-get="addresses?to=addresses"`) {
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
    // from=watches is what sends Back to the watch list rather than to the
    // Addresses placeholder or the block list
    if !strings.Contains(body, `hx-get="address?a=`+liveAddress+`&from=watches"`) {
        t.Error("a watched address must open the address page, with the full id and its origin")
    }
    if !strings.Contains(body, `hx-get="tx?id=`+liveTxid+`&from=watches"`) {
        t.Error("a watched transaction must open the transaction page, with the full id and its origin")
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

// Back should put the reader where they started. A page opened from the search
// field returns to Home, one opened from the watch list returns to Watches, and
// one opened from the block list stays on Blocks.
func TestBackReturnsToOrigin(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks(), d: liveBlockInfo(),
        t: liveTx(), a: liveAddr()})
    var data = freshInitData("TESTTOKEN")
    var cases = []struct{ name, path, wantBack, wantTo string }{
        {"block from search", "/block?height=963268&from=home", "blocks?to=home", "home"},
        {"block from list", "/block?height=963268&down=963257", "blocks?down=963257&amp;to=blocks", "blocks"},
        {"tx from search", "/tx?id=" + liveTxid + "&from=home", "blocks?to=home", "home"},
        {"tx from watches", "/tx?id=" + liveTxid + "&from=watches", "blocks?to=watches", "watches"},
        {"address from search", "/address?a=" + liveAddress + "&from=home", "addresses?to=home", "home"},
        {"address from watches", "/address?a=" + liveAddress + "&from=watches", "addresses?to=watches", "watches"},
        {"miner from list", "/miner?name=AntPool&down=963260", "blocks?down=963260&amp;to=blocks", "blocks"},
    }
    for _, c := range cases {
        var body = get(h, c.path, data).Body.String()
        if !strings.Contains(body, `hx-get="`+c.wantBack+`"`) {
            var head = body[strings.Index(body, `class="head"`):]
            t.Errorf("%s: Back is not %q: %s", c.name, c.wantBack, head[:min(200, len(head))])
        }
    }
}

// Following Back must actually land on that tab, which is the restoring
// endpoint's job — it carries the reader on with HX-Trigger.
func TestBackEndpointsSwitchTab(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var data = freshInitData("TESTTOKEN")
    for _, c := range []struct{ path, want string }{
        {"/blocks?to=home", `{"showtab":"home"}`},
        {"/blocks?down=963257&to=watches", `{"showtab":"watches"}`},
        {"/addresses?to=watches", `{"showtab":"watches"}`},
        {"/addresses?to=home", `{"showtab":"home"}`},
        {"/addresses", `{"showtab":"addresses"}`},
    } {
        if got := get(h, c.path, data).Header().Get("HX-Trigger"); got != c.want {
            t.Errorf("%s: HX-Trigger = %q, want %q", c.path, got, c.want)
        }
    }
}

// The block list is also what an empty list reloads through, and the batches
// carry no tab of their own. None of that may move a reader off whatever tab
// they are on, so these stay silent unless Back explicitly asked for a switch.
func TestBlockListDoesNotHijackTheTab(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var data = freshInitData("TESTTOKEN")
    for _, p := range []string{"/blocks", "/blocks?down=963257",
        "/moreblocks?before=963257", "/newblocks?after=963266"} {
        if got := get(h, p, data).Header().Get("HX-Trigger"); got != "" {
            t.Errorf("%s set HX-Trigger %q; a refresh must not switch tabs", p, got)
        }
    }
}

// The origin arrives in a URL a user can edit and is interpolated into a JSON
// header, so only known panel names may pass.
func TestOriginIsValidated(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks(), d: liveBlockInfo()})
    var data = freshInitData("TESTTOKEN")
    var body = get(h, `/block?height=963268&from="},"x":{"`, data).Body.String()
    if !strings.Contains(body, `hx-get="blocks?to=blocks"`) {
        t.Error("an unknown origin should fall back to the page's own tab")
    }
    var trig = get(h, `/blocks?to="},"evil":{"`, data).Header().Get("HX-Trigger")
    if trig != "" {
        t.Errorf("an unknown tab reached the header: %q", trig)
    }
}

// The miner name in the block list opens its own page.
func TestMinerLinkAndPage(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var data = freshInitData("TESTTOKEN")
    var list = get(h, "/blocks", data).Body.String()
    if !strings.Contains(list, `hx-get="miner?name=AntPool&down=963268"`) {
        t.Error("the miner name does not link to its page")
    }
    // pool names have spaces ("SBI Crypto", "Foundry USA"), so the link has to
    // survive the URL — an unescaped space would truncate the name
    if !strings.Contains(list, `hx-get="miner?name=SBI&#43;Crypto&down=963263"`) {
        t.Errorf("a pool name with a space is not url-escaped in its link")
    }
    // an unattributed miner is still plain text: there is no pool to open
    if !strings.Contains(list, `<span class="mn">Unknown</span>`) {
        t.Error("Unknown must not become a link")
    }
    if strings.Contains(list, `miner?name=Unknown`) {
        t.Error("Unknown was linked")
    }
}

// The miner page: the name as the title, Back on its left, and the same figures
// /miners prints.
func TestMinerDetailsRender(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{b: liveBlocks()})
    var body = get(h, "/miner?name=AntPool", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(body, "<h1>AntPool</h1>") {
        t.Errorf("missing the miner name as the title: %s", body)
    }
    for _, want := range []string{">Blocks mined<", ">Reward<", ">Fees<", ">Consumption<",
        "22 blocks", "69.14 BTC", "0.39 BTC", "2 GW"} {
        if !strings.Contains(body, want) {
            t.Errorf("miner page is missing %q", want)
        }
    }
    if !strings.Contains(body, `<div id="blocklist" class="blocklist det">`) {
        t.Error("the miner page belongs in the Blocks tab's container")
    }
    var head = body[strings.Index(body, `class="head"`):strings.Index(body, `class="fields"`)]
    if strings.Index(head, "&lt; Back") > strings.Index(head, "<h1>") {
        t.Errorf("Back should come before the title: %s", head)
    }
}

// A pool with no statistics yet says so rather than showing zeros as fact.
func TestMinerDetailsUnknown(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var body = get(h, "/miner?name=NoSuchPool", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(body, "nothing found") {
        t.Errorf("an untracked pool should say so: %s", body)
    }
    if !strings.Contains(body, "<h1>NoSuchPool</h1>") {
        t.Error("the title should still name the pool that was asked for")
    }
}

// The name comes from a URL a user can edit, and pool names have spaces in them.
func TestMinerNameHandling(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var data = freshInitData("TESTTOKEN")
    for _, p := range []string{"/miner", "/miner?name="} {
        if w := get(h, p, data); w.Code != 400 {
            t.Errorf("%s = %d, want 400", p, w.Code)
        }
    }
    var body = get(h, "/miner?name=SBI+Crypto", data).Body.String()
    if !strings.Contains(body, "<h1>SBI Crypto</h1>") {
        t.Errorf("a pool name with a space did not survive the URL: %s", body)
    }
}

// The bell sits in the title row of the two pages that have something to watch,
// and loads its own state — the page around it is cached and shared, so the
// pushed/unpushed state cannot be rendered into it.
func TestWatchButtonOnDetailsPages(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{t: liveTx(), a: liveAddr(), b: liveBlocks(), d: liveBlockInfo()})
    var data = freshInitData("TESTTOKEN")
    for _, c := range []struct{ path, want string }{
        {"/address?a=" + liveAddress, `hx-get="watch?kind=address&id=` + liveAddress + `"`},
        {"/tx?id=" + liveTxid, `hx-get="watch?kind=tx&id=` + liveTxid + `"`},
    } {
        var body = get(h, c.path, data).Body.String()
        if !strings.Contains(body, c.want) {
            t.Errorf("%s does not load a watch button: %s", c.path, body[:min(400, len(body))])
        }
        if !strings.Contains(body, `hx-trigger="load"`) {
            t.Errorf("%s: the button must fetch its own state", c.path)
        }
        // the shared, cached page must not carry anyone's watch state
        if strings.Contains(body, `class="bell`) {
            t.Errorf("%s rendered the button's state into the cached page", c.path)
        }
    }
    // a block and a miner have nothing to watch
    for _, p := range []string{"/block?height=963268", "/miner?name=AntPool"} {
        if body := get(h, p, data).Body.String(); strings.Contains(body, "watch?kind=") {
            t.Errorf("%s should have no watch button", p)
        }
    }
}

// Pushed when watching, unpushed when not, and tapping flips it. The POST
// carries the state it wants rather than toggling, so a stale button cannot
// undo a watch the reader did not touch.
func TestWatchButtonTogglesAndReflectsState(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var data = freshInitData("TESTTOKEN")
    var url = "/watch?kind=address&id=" + liveAddress

    var off = get(h, url, data).Body.String()
    if strings.Contains(off, "bell on") {
        t.Errorf("an unwatched address should render unpushed: %s", off)
    }
    if !strings.Contains(off, "&on=1") {
        t.Errorf("the unpushed button should offer to start watching: %s", off)
    }

    var on = post(h, url+"&on=1", data).Body.String()
    if !strings.Contains(on, "bell on") {
        t.Errorf("after watching, the button should be pushed: %s", on)
    }
    if !strings.Contains(on, "&on=0") {
        t.Errorf("the pushed button should offer to stop watching: %s", on)
    }
    // it stays pushed on a fresh read, which is the whole point
    if again := get(h, url, data).Body.String(); !strings.Contains(again, "bell on") {
        t.Errorf("the watch did not stick: %s", again)
    }
    // and tapping again removes it
    var back = post(h, url+"&on=0", data).Body.String()
    if strings.Contains(back, "bell on") {
        t.Errorf("after unwatching, the button should be unpushed: %s", back)
    }
    if w, ok := watched[42][liveAddress]; ok && w {
        t.Error("the watch was not removed from the source")
    }
}

// Whether a reader watches something is per-user, so the button must never be
// cached: two users looking at the same page see their own state.
func TestWatchButtonIsPerUser(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var mine = freshInitData("TESTTOKEN")
    var theirs = signInitData("TESTTOKEN", map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Unix(), 10),
        "user":      `{"id":99,"first_name":"Mallory"}`})
    var url = "/watch?kind=address&id=" + liveAddress
    post(h, url+"&on=1", mine)
    if body := get(h, url, mine).Body.String(); !strings.Contains(body, "bell on") {
        t.Errorf("user 42 should see their own watch: %s", body)
    }
    if body := get(h, url, theirs).Body.String(); strings.Contains(body, "bell on") {
        t.Errorf("user 99 was served user 42's watch state: %s", body)
    }
}

// The button is data, so it needs a signature, and its parameters come from a
// URL a user can edit.
func TestWatchButtonGuards(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var data = freshInitData("TESTTOKEN")
    if w := get(h, "/watch?kind=address&id="+liveAddress, ""); w.Code != 401 {
        t.Errorf("unauthenticated /watch = %d, want 401", w.Code)
    }
    for _, p := range []string{"/watch", "/watch?kind=address", "/watch?kind=block&id=1",
        "/watch?kind=&id=x", "/watch?id=" + liveAddress} {
        if w := get(h, p, data); w.Code != 400 {
            t.Errorf("%s = %d, want 400", p, w.Code)
        }
    }
}

// Outside Telegram there is nobody to file a watch for, so the button is hidden
// — by a body class set once at load, since details pages arrive later by swap
// and any script inside them would not re-run.
func TestWatchButtonHiddenOutsideTelegram(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    if !strings.Contains(body, `classList.add("tg")`) {
        t.Error("nothing marks the page as running inside Telegram")
    }
    if !strings.Contains(body, ".det .head .bell { display: none;") {
        t.Error("the bell must be hidden by default")
    }
    if !strings.Contains(body, "body.tg .det .head .bell { display: flex; }") {
        t.Error("the bell must only appear once initData is present")
    }
}

// A failed set must not leave the button claiming a state the store does not
// have.
func TestWatchButtonReportsFailure(t *testing.T) {
    var h = handler(t, "TESTTOKEN", failingSource{})
    var body = post(h, "/watch?kind=address&id="+liveAddress+"&on=1", freshInitData("TESTTOKEN")).Body.String()
    if !strings.Contains(body, "bell err") {
        t.Errorf("a failed watch should show as failed: %s", body)
    }
    if strings.Contains(body, "bell on") {
        t.Errorf("a failed watch must not render as watching: %s", body)
    }
}

// Font sizes are relative so the page follows the font size the reader chose in
// their phone's settings; a px size ignores it, which is what made the text read
// too small on a high-density screen.
func TestFontSizesAreRelative(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    if n := strings.Count(body, "font-size: "); n == 0 {
        t.Fatal("no font sizes in the page at all")
    }
    // Every declaration must be relative — rem, or a min()/max() built on rem.
    // The root's own is the exception: it is the base the rest are fractions of,
    // so it cannot be relative to itself.
    for _, i := range indexesOf(body, "font-size: ") {
        var decl = body[i : i+min(60, len(body)-i)]
        var end = strings.Index(decl, ";")
        if end > 0 { decl = decl[:end] }
        if isRootRule(body[:i]) { continue }
        if !strings.Contains(decl, "rem") {
            t.Errorf("font size is not relative: %q", decl)
        }
    }
    if strings.Contains(body, "font: 17px") {
        t.Error("the body shorthand still carries a px size")
    }
}

// Every size being relative is what lets one rule move the whole page, which is
// how the text is made bigger on a phone: Telegram's webview does not pass the
// reader's own font setting down to the root.
func TestPhoneBaseFontSize(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    if !strings.Contains(body, "@media (max-width: 480px)") {
        t.Error("no phone-sized breakpoint, so the base size never changes")
    }
    var i = strings.Index(body, "@media (max-width: 480px)")
    var block = body[i : i+min(160, len(body)-i)]
    if !strings.Contains(block, "html.phone { font-size: 19px; }") {
        t.Errorf("the breakpoint does not raise the root: %q", block)
    }
    // Width alone matched Telegram Desktop too, which shows a Mini App in a
    // phone-width panel and came out oversized on a monitor.
    if !strings.Contains(body, `/^(android|android_x|ios)$/.test(tg.platform)`) {
        t.Error("nothing asks Telegram what platform it is, so a desktop gets the phone size")
    }
    if !strings.Contains(body, `classList.add("phone")`) {
        t.Error("the phone class is never set, so the breakpoint can never apply")
    }
    // It must be set before the body renders, or every size jumps once it lands.
    var script = strings.Index(body, `classList.add("phone")`)
    if script > strings.Index(body, "<body") {
        t.Error("the phone class is set after the body, which would show a visible jump")
    }
}

// Windows renders the panel larger than the other desktops, so it takes the
// opposite correction. Telegram cannot answer this one — Windows, Linux and
// macOS all report "tdesktop" — so the OS is read from the browser.
func TestWindowsBaseFontSize(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    var i = strings.Index(body, "@media (max-width: 480px)")
    if i < 0 { t.Fatal("no phone-sized breakpoint") }
    var block = body[i : i+min(500, len(body)-i)]
    if !strings.Contains(block, "html.windows { font-size: 15px; }") {
        t.Errorf("the breakpoint does not shrink the root on Windows: %q", block)
    }
    if !strings.Contains(body, `ua.platform === "Windows"`) {
        t.Error("nothing reads the OS from userAgentData")
    }
    if !strings.Contains(body, `/^Win/i.test(navigator.platform`) {
        t.Error("no fallback for a browser without userAgentData")
    }
    if !strings.Contains(body, `classList.add("windows")`) {
        t.Error("the windows class is never set, so the rule can never apply")
    }
    // Both rules have equal specificity, so a device matching each would be
    // decided by their order in the stylesheet rather than by anything real.
    if !strings.Contains(body, `else if (windows)`) {
        t.Error("phone and windows are not exclusive; a device could take both")
    }
    // Set before the body renders, like the phone class, or the size jumps.
    if strings.Index(body, `classList.add("windows")`) > strings.Index(body, "<body") {
        t.Error("the windows class is set after the body, which would show a visible jump")
    }
}

// Two sizes cannot simply scale, and both are guarded rather than left to break.
func TestFontSizeGuards(t *testing.T) {
    var body = get(handler(t, "TESTTOKEN", fakeSource{f: liveFees()}), "/", "").Body.String()
    // below 16px iOS zooms the whole page whenever the search field is focused,
    // which a smaller font setting would otherwise cause
    if !strings.Contains(body, "font-size: max(1.0625rem, 16px)") {
        t.Error("the search field has no 16px floor; a small font setting would make iOS zoom")
    }
    // six columns of percentages already measure their cells, so they yield to
    // the width once it runs out
    if !strings.Contains(body, "font-size: min(0.75rem, 3.34vw)") {
        t.Error("the market percentages are not width-capped; a large font setting overflows the row")
    }
}

// isRootRule reports whether a declaration sits in a rule on the root element,
// whose own font size is the base every rem is a fraction of and so is the one
// size that cannot itself be relative.
func isRootRule(before string) bool {
    var open = strings.LastIndex(before, "{")
    if open < 0 { return false }
    var selector = strings.TrimSpace(before[:open])
    if i := strings.LastIndexAny(selector, " \n"); i >= 0 { selector = selector[i+1:] }
    return selector == "html" || strings.HasPrefix(selector, "html.")
}

func indexesOf(s, sub string) []int {
    var out []int
    for i := 0; ; {
        var j = strings.Index(s[i:], sub)
        if j < 0 { return out }
        out = append(out, i+j)
        i += j + len(sub)
    }
}

// An open event stream must not hold the server open. Shutdown waits for
// connections to fall idle and a stream never does on its own, so before this it
// waited out the caller's whole timeout and then reported "context deadline
// exceeded" — which is what the bot's logs showed on every restart.
func TestShutdownDoesNotWaitForEventStreams(t *testing.T) {
    // a real listener and a real connection: the point is what Shutdown does
    // with a live streaming request, which a recorder cannot exercise
    var probe, err = net.Listen("tcp", "127.0.0.1:0")
    if err != nil { t.Fatalf("pick a port: %v", err) }
    var addr = probe.Addr().String()
    probe.Close()

    invalidateAll()
    var srv = Start(addr, "TESTTOKEN", fakeSource{f: liveFees()})
    t.Cleanup(func() { srv.Close() })

    var before = subscribes()
    var resp *http.Response
    var deadline = time.Now().Add(5 * time.Second)
    for time.Now().Before(deadline) {
        resp, err = http.Get("http://" + addr + "/events")
        if err == nil { break }
        time.Sleep(20 * time.Millisecond)
    }
    if err != nil { t.Fatalf("open the stream: %v", err) }
    defer resp.Body.Close()
    for subscribes() == before && time.Now().Before(deadline) {
        time.Sleep(10 * time.Millisecond)
    }
    if subscribes() != before+1 {
        t.Fatal("the stream never registered, so this would not prove anything")
    }

    // the bot allows 15s for every server together; a stream that ends when
    // asked takes milliseconds
    var ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    var start = time.Now()
    if err := srv.Shutdown(ctx); err != nil {
        t.Fatalf("shutdown after %s: %v", time.Since(start), err)
    }
    if took := time.Since(start); took > 2*time.Second {
        t.Errorf("shutdown took %s; the stream did not let go", took)
    }
    if n := subscribes(); n != before {
        t.Errorf("%d subscribers left after shutdown, want %d", n, before)
    }
}


// postForm is how the alias dialog's buttons reach the server: from inside a
// form, so what they act on arrives as form values rather than in the URL.
func postForm(h http.Handler, path, initData string, form url.Values) *httptest.ResponseRecorder {
    var r = httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
    r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
    if initData != "" { r.Header.Set("X-Telegram-Init-Data", initData) }
    var w = httptest.NewRecorder()
    h.ServeHTTP(w, r)
    return w
}

// Filing a watch is only the first half: the answer asks the page to open the
// alias dialog, which is what names it. Unwatching asks for nothing.
func TestWatchAsksForAnAlias(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var data = freshInitData("TESTTOKEN")
    var link = "/watch?kind=address&id=" + liveAddress
    var on = post(h, link+"&on=1", data).Header().Get("HX-Trigger")
    var events map[string]any
    if err := json.Unmarshal([]byte(on), &events); err != nil {
        t.Fatalf("HX-Trigger %q is not the JSON form: %v", on, err)
    }
    var ask, ok = events["askalias"].(map[string]any)
    if !ok {
        t.Fatalf("watching did not ask for an alias: %s", on)
    }
    if ask["kind"] != "address" || ask["id"] != liveAddress {
        t.Errorf("the dialog was asked about %v, want the address just watched", ask)
    }
    // and the watch list has changed, so it re-fetches itself
    if _, ok := events["watchtab"]; !ok {
        t.Errorf("the watch list was not told to refresh: %s", on)
    }
    var off = post(h, link+"&on=0", data).Header().Get("HX-Trigger")
    if strings.Contains(off, "askalias") {
        t.Errorf("unwatching asked for an alias: %s", off)
    }
    if !strings.Contains(off, "watchtab") {
        t.Errorf("unwatching did not refresh the list: %s", off)
    }
}

// The dialog names the watch the bell just filed.
func TestSetAlias(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var data = freshInitData("TESTTOKEN")
    var res = postForm(h, "/alias", data, url.Values{
        "kind": {"address"}, "id": {liveAddress}, "alias": {"John"}})
    if res.Code != http.StatusNoContent {
        t.Fatalf("status %d, want 204 — the dialog swaps nothing", res.Code)
    }
    if got := aliases[42][liveAddress]; got != "John" {
        t.Errorf("the alias reached the source as %q, want John", got)
    }
    if !strings.Contains(res.Header().Get("HX-Trigger"), "watchtab") {
        t.Errorf("the watch list was not told to refresh: %s", res.Header().Get("HX-Trigger"))
    }
}

// Save on an empty field is how a reader dismisses the dialog, so it must leave
// the watch alone rather than clearing the name it already has.
func TestEmptyAliasChangesNothing(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var data = freshInitData("TESTTOKEN")
    var res = postForm(h, "/alias", data, url.Values{
        "kind": {"address"}, "id": {liveAddress}, "alias": {"   "}})
    if res.Code != http.StatusNoContent {
        t.Fatalf("status %d, want 204", res.Code)
    }
    if _, set := aliases[42][liveAddress]; set {
        t.Errorf("an empty alias was written: %q", aliases[42][liveAddress])
    }
    if res.Header().Get("HX-Trigger") != "" {
        t.Errorf("nothing changed, so nothing should be refreshed: %s", res.Header().Get("HX-Trigger"))
    }
}

// A label is all an alias is, and this endpoint is reachable without the page,
// so an enormous one is cut rather than stored whole.
func TestLongAliasIsCut(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    postForm(h, "/alias", freshInitData("TESTTOKEN"), url.Values{
        "kind": {"address"}, "id": {liveAddress}, "alias": {strings.Repeat("é", 500)}})
    if got := len([]rune(aliases[42][liveAddress])); got != aliasMax {
        t.Errorf("stored %d runes, want it cut to %d", got, aliasMax)
    }
}

// An alias is per-user, like everything else on the Watches tab.
func TestAliasNeedsInitData(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var res = postForm(h, "/alias", "", url.Values{
        "kind": {"address"}, "id": {liveAddress}, "alias": {"John"}})
    if res.Code != http.StatusUnauthorized {
        t.Errorf("status %d, want 401 — an unsigned request must not rename anything", res.Code)
    }
    if len(aliases) != 0 {
        t.Errorf("an unsigned request renamed %v", aliases)
    }
}

// The dialog's Delete button posts from inside the form, so the id it removes
// arrives as a form value rather than in the URL.
func TestDeleteFromTheAliasDialog(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{a: liveAddr()})
    var data = freshInitData("TESTTOKEN")
    post(h, "/watch?kind=address&id="+liveAddress+"&on=1", data)
    if !watched[42][liveAddress] {
        t.Fatal("the fixture was not watching anything to delete")
    }
    var res = postForm(h, "/watch?on=0", data, url.Values{"kind": {"address"}, "id": {liveAddress}})
    if res.Code != http.StatusOK {
        t.Fatalf("status %d, want 200", res.Code)
    }
    if watched[42][liveAddress] {
        t.Error("the watch survived a delete from the dialog")
    }
    if !strings.Contains(res.Header().Get("HX-Trigger"), "watchtab") {
        t.Errorf("the list was not told to refresh: %s", res.Header().Get("HX-Trigger"))
    }
}

// Every row carries what the edit dialog needs, since the dialog itself is in
// the shared page and knows nothing until a row tells it.
func TestWatchRowsCarryAnEditIcon(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{w: liveWatches()})
    var body = get(h, "/watches", freshInitData("TESTTOKEN")).Body.String()
    for _, want := range []string{
        `class="edit" data-kind="address" data-id="` + liveAddress + `" data-alias="John"`,
        `class="edit" data-kind="tx" data-id="` + liveTxid + `"`,
    } {
        if !strings.Contains(body, want) {
            t.Errorf("no edit icon for %s in:\n%s", want, body)
        }
    }
}


// An id a details row mentions is tappable, and a tap is the same handoff a
// search makes — so an address opens on the Addresses tab and a block on Blocks,
// with no classification of its own.
func TestDetailsRowsLinkTheirIds(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{t: liveTx(), a: liveAddr(), d: liveBlockInfo()})
    var data = freshInitData("TESTTOKEN")
    var body = get(h, "/tx?id="+liveTxid, data).Body.String()
    for _, want := range []string{
        `<span class="lnk" hx-get="search?q=` + liveAddress + `&from=blocks"`,
        `<span class="lnk" hx-get="search?q=1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa&from=blocks"`,
        `<span class="lnk" hx-get="search?q=963268&from=blocks"`,
    } {
        if !strings.Contains(body, want) {
            t.Errorf("no link for %s in:\n%s", want, body)
        }
    }
    // the text around an id stays plain, and the whole line still reads the same
    for _, want := range []string{">412 (block ", ">#963268<", ">1A1zP1...DivfNa<", ">, <"} {
        if !strings.Contains(body, want) {
            t.Errorf("the line was not kept intact around its ids (%s):\n%s", want, body)
        }
    }
    // a row with no id in it renders as it always did
    if !strings.Contains(body, `<span class="val">9 990 000 sats (≈ $6,614)</span>`) {
        t.Errorf("a row with no ids should render plainly:\n%s", body)
    }
}

// A tapped id carries the page's own origin, so Back keeps returning to where
// the reader started rather than to whichever page they came through.
func TestLinkedIdsKeepTheOrigin(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{t: liveTx(), a: liveAddr()})
    var data = freshInitData("TESTTOKEN")
    var body = get(h, "/tx?id="+liveTxid+"&from=home", data).Body.String()
    if !strings.Contains(body, "&from=home") {
        t.Errorf("a page opened from Home should hand that on to its links:\n%s", body)
    }
    // and /search honours it, rather than sending everything back to Home
    var res = get(h, "/search?q="+liveAddress+"&from=watches", data)
    if got := res.Header().Get("Location"); !strings.Contains(got, "from=watches") {
        t.Errorf("search redirected to %q, dropping the origin", got)
    }
    // an origin that is not a panel falls back to Home, since it reaches a URL
    if res := get(h, "/search?q="+liveAddress+"&from=nonsense", data); !strings.Contains(res.Header().Get("Location"), "from=home") {
        t.Errorf("an unknown origin should fall back to Home: %q", res.Header().Get("Location"))
    }
}

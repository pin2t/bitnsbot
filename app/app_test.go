package app

import "context"
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
}

func (s fakeSource) Fees() Fees       { return s.f }
func (s fakeSource) Network() Network { return s.n }

func liveNetwork() Network {
    return Network{OK: true, Coins: "20.1 M", Cap: "21 M",
        Blocks: "963 166", Size: "869 GB", Nodes: "31 751", Txs: "1.4 B", Addresses: "1.5 B"}
}

// handler returns the routes Start wires. Start inlines the routing and always
// launches a listener, so tests take its Handler and drive it with a recorder
// rather than over a socket; the ephemeral listener is closed via t.Cleanup.
func handler(t *testing.T, token string, src Source) http.Handler {
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
        "X-Telegram-Init-Data", `data-panel="home"`, `data-panel="miners"`, `id="q"`} {
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
    if strings.Contains(body, "loading…") {
        t.Error(`page still ships a "loading…" placeholder; fees should be rendered server-side`)
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
    for _, want := range []string{"</style>", `id="q"`, `data-panel="miners"`, "</nav>", "</body>"} {
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

// The template block the page renders and the one the refresh returns must be
// the same block, or the card would change shape when it updates.
func TestInlineAndRefreshMatch(t *testing.T) {
    var h = handler(t, "TESTTOKEN", fakeSource{f: liveFees(), n: liveNetwork()})
    var page = get(h, "/", "").Body.String()
    for _, path := range []string{"/fees", "/network"} {
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
    if !strings.Contains(body, `hx-trigger="sse:fees"`) {
        t.Error(`the fees card must refresh on the sse:fees event`)
    }
    if strings.Contains(body, `hx-trigger="every 60s"`) {
        t.Error("the 60s poll should be gone: the server pushes now")
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

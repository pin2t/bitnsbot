package app

import "context"
import "net/http/httptest"
import "strings"
import "testing"
import "time"

const mpTx1 = "aa000000000000000000000000000000000000000000000000000000000000a1"
const mpTx2 = "bb000000000000000000000000000000000000000000000000000000000000b2"
const mpTx3 = "cc000000000000000000000000000000000000000000000000000000000000c3"

// liveMempool is the page with two arrivals, and what an open page at sequence
// 7 prepends: one more arrival, and one of the listed two mined since.
func liveMempool(after int64) Mempool {
    if after >= 0 {
        return Mempool{OK: true, Top: 9, Gone: []string{mpTx1},
            Txs: []MempoolTx{{Id: mpTx3, Short: "cc0000...0000c3", Amount: "7 000 sats", At: 1700000009, Ago: "just now"}}}
    }
    return Mempool{OK: true, Top: 7,
        Rows: []Field{{Label: "Size", Value: "12.5 MB"}, {Label: "Transactions", Value: "36 552"},
            {Label: "Flow rate", Value: "4.2 tx/sec"}, {Label: "Total flow", Value: "~1 234.5 BTC"}},
        Txs: []MempoolTx{
            {Id: mpTx2, Short: "bb0000...0000b2", Amount: "0.25 BTC", At: 1700000005, Ago: "just now"},
            {Id: mpTx1, Short: "aa0000...0000a1", Amount: "12 000 sats", At: 1700000001, Ago: "just now"},
        }}
}

func mempoolSource() fakeSource {
    return fakeSource{f: liveFees(), mp: map[int64]Mempool{-1: liveMempool(-1), 7: liveMempool(7)}}
}

// The mempool count on the fee card is what opens the page, in every language.
func TestFeesCardLinksToMempool(t *testing.T) {
    var h = handler(t, "TESTTOKEN", mempoolSource())
    for _, lang := range langs {
        var r = httptest.NewRequest("GET", "/fees", nil)
        r.Header.Set("X-Telegram-Init-Data", freshInitData("TESTTOKEN"))
        r.Header.Set("Accept-Language", lang)
        var w = httptest.NewRecorder()
        h.ServeHTTP(w, r)
        if !strings.Contains(w.Body.String(), `hx-get="mempool?from=home" hx-target="#blocklist" hx-swap="outerHTML">36 552</span>`) {
            t.Errorf("[%s] fee card does not link its mempool count: %s", lang, w.Body.String())
        }
    }
}

func TestMempoolPage(t *testing.T) {
    var h = handler(t, "TESTTOKEN", mempoolSource())
    var w = get(h, "/mempool?from=home", freshInitData("TESTTOKEN"))
    if w.Code != 200 {
        t.Fatalf("GET /mempool = %d, want 200", w.Code)
    }
    if got := w.Header().Get("HX-Retarget"); got != "#blocklist" {
        t.Errorf("HX-Retarget = %q, want #blocklist", got)
    }
    if got := w.Header().Get("HX-Trigger"); got != `{"showtab":"blocks"}` {
        t.Errorf("HX-Trigger = %q", got)
    }
    var body = w.Body.String()
    for _, want := range []string{
        `<h1>Mempool</h1>`,
        `hx-get="blocks?to=home"`,
        `<span class="lbl">Size</span><span class="val">12.5 MB</span>`,
        `<span class="lbl">Flow rate</span><span class="val">4.2 tx/sec</span>`,
        `<span class="lbl">Total flow</span><span class="val">~1 234.5 BTC</span>`,
        `hx-get="newmempool?after=7"`,
        `hx-trigger="sse:mempool, every 30s"`,
        `id="mp` + mpTx2 + `"`,
        `hx-get="tx?id=` + mpTx2 + `&from=mempool"`,
        `<span class="mpamt">0.25 BTC</span>`,
        `data-ts="1700000005"`,
        `data-lang="en"`,
    } {
        if !strings.Contains(body, want) {
            t.Errorf("mempool page is missing %q", want)
        }
    }
    if strings.Index(body, mpTx2) > strings.Index(body, mpTx1) {
        t.Errorf("mempool list is not newest first")
    }
}

// A prepend carries the new rows under a fresh sentinel, and deletes the rows
// of the transactions mined since.
func TestNewMempoolPrepends(t *testing.T) {
    var h = handler(t, "TESTTOKEN", mempoolSource())
    var w = get(h, "/newmempool?after=7", freshInitData("TESTTOKEN"))
    if w.Code != 200 {
        t.Fatalf("GET /newmempool = %d, want 200", w.Code)
    }
    var body = w.Body.String()
    for _, want := range []string{`hx-get="newmempool?after=9"`, `id="mp` + mpTx3 + `"`,
        `<div id="mp` + mpTx1 + `" hx-swap-oob="delete"></div>`} {
        if !strings.Contains(body, want) {
            t.Errorf("prepend is missing %q in %s", want, body)
        }
    }
    if strings.Contains(body, `<h1>`) || strings.Contains(body, mpTx2) {
        t.Errorf("prepend carries more than what is new: %s", body)
    }
    if w.Header().Get("HX-Retarget") != "" {
        t.Errorf("an ordinary prepend retargets")
    }
    for _, bad := range []string{"/newmempool", "/newmempool?after=-1", "/newmempool?after=x"} {
        if w := get(h, bad, freshInitData("TESTTOKEN")); w.Code != 400 {
            t.Errorf("GET %s = %d, want 400", bad, w.Code)
        }
    }
}

// More new arrivals than fit replace the list rather than leave a gap in it.
func TestNewMempoolOverflowReplacesTheList(t *testing.T) {
    var src = mempoolSource()
    var whole = liveMempool(-1)
    whole.More = true
    src.mp[3] = whole
    var w = get(handler(t, "TESTTOKEN", src), "/newmempool?after=3", freshInitData("TESTTOKEN"))
    if got := w.Header().Get("HX-Retarget"); got != "#mplist" {
        t.Errorf("HX-Retarget = %q, want #mplist", got)
    }
    if got := w.Header().Get("HX-Reswap"); got != "innerHTML" {
        t.Errorf("HX-Reswap = %q, want innerHTML", got)
    }
    if !strings.Contains(w.Body.String(), mpTx1) || !strings.Contains(w.Body.String(), mpTx2) {
        t.Errorf("the replacement is not the whole list: %s", w.Body.String())
    }
}

// The page is rendered afresh every time: a cached copy would show a list
// minutes old.
func TestMempoolIsNotCached(t *testing.T) {
    var src = newCounting()
    var h = handler(t, "TESTTOKEN", src)
    get(h, "/mempool", freshInitData("TESTTOKEN"))
    get(h, "/mempool", freshInitData("TESTTOKEN"))
    get(h, "/newmempool?after=7", freshInitData("TESTTOKEN"))
    get(h, "/newmempool?after=7", freshInitData("TESTTOKEN"))
    if n := src.count("mempool"); n != 4 {
        t.Errorf("Source.Mempool called %d times for four requests, want 4", n)
    }
}

// A transaction opened from the list goes Back to the list.
func TestTxFromMempoolGoesBackToIt(t *testing.T) {
    var w = get(handler(t, "TESTTOKEN", mempoolSource()), "/tx?id="+mpTx2+"&from=mempool", freshInitData("TESTTOKEN"))
    if !strings.Contains(w.Body.String(), `<button hx-get="mempool" hx-target="#blocklist" hx-swap="outerHTML">`) {
        t.Errorf("Back does not return to the mempool: %s", w.Body.String())
    }
}

// However often the list changes, the stream carries one mempool event per
// interval — the first at once, the rest folded into one when it is up.
func TestNotifyThrottlesMempool(t *testing.T) {
    var saved = throttled["mempool"]
    throttled["mempool"] = 200 * time.Millisecond
    t.Cleanup(func() { throttled["mempool"] = saved })
    throttleMu.Lock()
    throttleLast, throttlePending = map[string]time.Time{}, map[string]bool{}
    throttleMu.Unlock()
    var h = handler(t, "TESTTOKEN", fakeSource{})
    var r = httptest.NewRequest("GET", "/events", nil)
    var ctx, cancel = context.WithCancel(r.Context())
    r = r.WithContext(ctx)
    var w = httptest.NewRecorder()
    var done = make(chan struct{})
    go func() { h.ServeHTTP(w, r); close(done) }()
    var deadline = time.Now().Add(2 * time.Second)
    for subscribes() == 0 && time.Now().Before(deadline) {
        time.Sleep(5 * time.Millisecond)
    }
    for i := 0; i < 10; i++ {
        Notify("mempool")
        time.Sleep(10 * time.Millisecond)
    }
    time.Sleep(350 * time.Millisecond)
    cancel()
    <-done
    if n := strings.Count(w.Body.String(), "event: mempool\n"); n != 2 {
        t.Errorf("%d mempool events for ten changes inside one interval, want 2: %q", n, w.Body.String())
    }
}

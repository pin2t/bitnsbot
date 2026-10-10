package max

import "bufio"
import "crypto/hmac"
import "crypto/sha256"
import "encoding/hex"
import "encoding/json"
import "io"
import "net/http"
import "net/http/httptest"
import "net/url"
import "sort"
import "strconv"
import "strings"
import "sync"
import "testing"
import "time"
import "bitnsbot/app"

const testToken = "MAXTOKEN"

// sign builds launch data as MAX does: every field but hash, sorted by key and
// joined as key=value lines, signed under HMAC-SHA256 of the token keyed by
// "WebAppData". It is written apart from valid, so the two cannot share a
// mistake.
func sign(token string, fields map[string]string) string {
    var secret = hmac.New(sha256.New, []byte("WebAppData"))
    secret.Write([]byte(token))
    var keys []string
    for k := range fields { keys = append(keys, k) }
    sort.Strings(keys)
    var lines []string
    for _, k := range keys { lines = append(lines, k+"="+fields[k]) }
    var mac = hmac.New(sha256.New, secret.Sum(nil))
    mac.Write([]byte(strings.Join(lines, "\n")))
    var v = url.Values{}
    for k, val := range fields { v.Set(k, val) }
    v.Set("hash", hex.EncodeToString(mac.Sum(nil)))
    return v.Encode()
}

// launch is launch data opened from the dialog chat with the bot, as MAX
// documents it.
func launch(chat int64) string {
    return sign(testToken, map[string]string{
        "auth_date": strconv.FormatInt(time.Now().Unix(), 10),
        "chat":      `{"id":` + strconv.FormatInt(chat, 10) + `,"type":"DIALOG"}`,
        "ip":        "192.168.0.1",
        "query_id":  "4c0ab423-342b-4e45-aea4-2747dbc500cd",
        "user":      `{"id":67890,"first_name":"Max","last_name":"User","username":null,"language_code":"ru","photo_url":null}`,
    })
}

func secretOf(token string) []byte {
    var mac = hmac.New(sha256.New, []byte("WebAppData"))
    mac.Write([]byte(token))
    return mac.Sum(nil)
}

func TestValidLaunchData(t *testing.T) {
    var now = time.Now()
    var good = launch(12345)
    if !valid(secretOf(testToken), good, now) { t.Fatal("rejected launch data signed with the token") }
    if valid(secretOf("OTHER"), good, now) { t.Error("accepted launch data signed with another token") }
    if valid(secretOf(testToken), strings.Replace(good, "12345", "12346", 1), now) { t.Error("accepted a tampered chat") }
    if valid(secretOf(testToken), good, now.Add(48*time.Hour)) { t.Error("accepted launch data two days old") }
    if valid(secretOf(testToken), good+"&chat=x", now) { t.Error("accepted a parameter given twice") }
    var v, _ = url.ParseQuery(good)
    v.Del("hash")
    if valid(secretOf(testToken), v.Encode(), now) { t.Error("accepted launch data with no hash") }
    if valid(secretOf(testToken), "", now) { t.Error("accepted nothing") }
    var hash, _ = url.ParseQuery(good)
    if !valid(secretOf(testToken), strings.Replace(good, hash.Get("hash"), strings.ToUpper(hash.Get("hash")), 1), now) {
        t.Error("rejected a hash written in upper case")
    }
}

// Only a dialog names a chat a notification could be sent to.
func TestChatOf(t *testing.T) {
    if got := chatOf(launch(376690654)); got != 376690654 { t.Errorf("dialog chat = %d", got) }
    var group = sign(testToken, map[string]string{"auth_date": "1", "chat": `{"id":5,"type":"CHAT"}`})
    if got := chatOf(group); got != 0 { t.Errorf("group chat = %d, want 0", got) }
    var none = sign(testToken, map[string]string{"auth_date": "1", "user": `{"id":5}`})
    if got := chatOf(none); got != 0 { t.Errorf("no chat = %d, want 0", got) }
}

// recorder is a fake app.Options that counts its calls and remembers what each
// was asked, since an Options function has nothing translated to show.
type recorder struct {
    mu      sync.Mutex
    calls   map[string]int
    langs   map[string]string
    chats   []int64
    set     []string
    aliases []string
}

func (r *recorder) hit(name, lang string) {
    r.mu.Lock()
    defer r.mu.Unlock()
    r.calls[name]++
    if lang != "" { r.langs[name] = lang }
}

func (r *recorder) count(name string) int {
    r.mu.Lock()
    defer r.mu.Unlock()
    return r.calls[name]
}

func (r *recorder) options() app.Options {
    return app.Options{
        Fees:    func() app.Fees { r.hit("fees", ""); return app.Fees{OK: true, TxCount: "42"} },
        Network: func() app.Network { r.hit("network", ""); return app.Network{OK: true, Blocks: "963 268"} },
        Market:  func(lang string) app.Market { r.hit("market", lang); return app.Market{OK: true, Price: "$1"} },
        Blocks: func(lang string, rng app.Range) app.Blocks {
            r.hit("blocks", lang)
            return app.Blocks{OK: true, Top: rng.After + rng.Before, Rows: []app.Block{{Num: 7}}}
        },
        Addresses: func(lang string, rng app.AddrRange) app.Addrs {
            r.hit("addresses "+rng.Kind+" "+strconv.Itoa(rng.From), lang)
            return app.Addrs{OK: true, Kind: rng.Kind}
        },
        BlockInfo: func(lang string, height int64) app.Info {
            r.hit("block", lang)
            return app.Info{OK: true, Title: "Блок " + strconv.FormatInt(height, 10), Kind: "block"}
        },
        TxInfo: func(lang, txid string) app.Info {
            r.hit("tx", lang)
            if strings.HasPrefix(txid, "0000") { return app.Info{OK: true, Title: "Блок 1", Kind: "block"} }
            return app.Info{OK: true, Title: txid[:6]}
        },
        AddrInfo:  func(lang, a string) app.Info { r.hit("address", lang); return app.Info{OK: a != "nope", Title: a} },
        AddrTxs:   func(lang, a string, from int) app.Txs { r.hit("addrtxs", lang); return app.Txs{OK: true, Addr: a, Next: from + 1} },
        MinerInfo: func(lang, name string) app.Info { r.hit("miner", lang); return app.Info{OK: true, Title: name} },
        MinerChart: func(lang, name, data, period string) app.Chart {
            r.hit("chart "+data+" "+period, lang)
            return app.Chart{OK: true, Name: name, Data: data, Period: period}
        },
        Mempool: func(lang string, after int64) app.Mempool {
            r.hit("mempool "+strconv.FormatInt(after, 10), lang)
            return app.Mempool{OK: true, Top: after + 1}
        },
        Watches: func(chat int64) app.Watches {
            r.mu.Lock()
            r.chats = append(r.chats, chat)
            r.mu.Unlock()
            return app.Watches{OK: true, Addresses: []app.Watch{{Id: "bc1q" + strconv.FormatInt(chat, 10)}}}
        },
        Watching: func(chat int64, kind, id string) bool { return id == "bc1qon" },
        SetWatch: func(chat int64, kind, id string, on bool) (bool, error) {
            r.mu.Lock()
            r.set = append(r.set, strconv.FormatInt(chat, 10)+" "+kind+" "+id+" "+strconv.FormatBool(on))
            r.mu.Unlock()
            return on, nil
        },
        SetAlias: func(chat int64, kind, id, alias string) (bool, error) {
            r.mu.Lock()
            r.aliases = append(r.aliases, strconv.FormatInt(chat, 10)+" "+kind+" "+id+" "+alias)
            r.mu.Unlock()
            return true, nil
        },
    }
}

// serveApp starts the Mini App's routes against a fresh recorder, with an empty
// cache so no test is served another's answers.
func serveApp(t *testing.T) (*recorder, *httptest.Server, chan struct{}) {
    t.Helper()
    cacheMu.Lock()
    cache.Clear()
    cacheMu.Unlock()
    var r = &recorder{calls: map[string]int{}, langs: map[string]string{}}
    var closing = make(chan struct{})
    var srv = httptest.NewServer(routes(testToken, r.options(), closing))
    t.Cleanup(srv.Close)
    return r, srv, closing
}

// call makes one request with the given launch data and returns the status and
// body.
func call(t *testing.T, srv *httptest.Server, method, path, initData string, form url.Values) (int, string) {
    t.Helper()
    var body io.Reader
    if form != nil { body = strings.NewReader(form.Encode()) }
    var req, err = http.NewRequest(method, srv.URL+path, body)
    if err != nil { t.Fatal(err) }
    if form != nil { req.Header.Set("Content-Type", "application/x-www-form-urlencoded") }
    if initData != "" { req.Header.Set(initHeader, initData) }
    var resp, doErr = http.DefaultClient.Do(req)
    if doErr != nil { t.Fatal(doErr) }
    defer resp.Body.Close()
    var b, _ = io.ReadAll(resp.Body)
    return resp.StatusCode, string(b)
}

// The page carries no data, so it is served to anyone — the webview has to load
// it before it can read the launch data — while every endpoint behind it wants
// that data signed with this bot's token.
func TestPageIsPublicAndDataIsNot(t *testing.T) {
    var _, srv, _ = serveApp(t)
    var code, body = call(t, srv, "GET", "/", "", nil)
    if code != http.StatusOK || !strings.Contains(body, `"@maxhub/max-ui": "https://cdn.jsdelivr.net/npm/@maxhub/max-ui@0.6.0/dist/index.js"`) {
        t.Fatalf("page: %d", code)
    }
    if code, _ = call(t, srv, "GET", "/nothing", "", nil); code != http.StatusNotFound { t.Errorf("/nothing answered %d", code) }
    for _, path := range []string{"/api/fees", "/api/blocks", "/api/watches", "/api/tx?id=" + strings.Repeat("a", 64)} {
        if code, _ = call(t, srv, "GET", path, "", nil); code != http.StatusUnauthorized { t.Errorf("%s without launch data: %d", path, code) }
        var forged = sign("OTHER", map[string]string{"auth_date": strconv.FormatInt(time.Now().Unix(), 10)})
        if code, _ = call(t, srv, "GET", path, forged, nil); code != http.StatusUnauthorized { t.Errorf("%s with forged launch data: %d", path, code) }
    }
}

// Every endpoint answers the Options function's own value as JSON, and asks it
// in Russian — the one language the MAX Mini App speaks.
func TestAPIAnswersInRussian(t *testing.T) {
    var r, srv, _ = serveApp(t)
    var ld = launch(1)
    var code, body = call(t, srv, "GET", "/api/fees", ld, nil)
    var fees app.Fees
    if code != http.StatusOK || json.Unmarshal([]byte(body), &fees) != nil || !fees.OK || fees.TxCount != "42" { t.Fatalf("fees: %d %s", code, body) }
    for _, path := range []string{"/api/market", "/api/blocks?before=10", "/api/addresses?kind=active&from=15",
        "/api/block?height=5", "/api/address?a=bc1q", "/api/addrtxs?a=bc1q&from=3", "/api/miner?name=Foundry+USA",
        "/api/minerchart?name=Foundry+USA&data=consumption&period=year", "/api/mempool"} {
        if code, body = call(t, srv, "GET", path, ld, nil); code != http.StatusOK || !strings.HasPrefix(body, "{") { t.Errorf("%s: %d %s", path, code, body) }
    }
    for name, lang := range r.langs {
        if lang != "ru" { t.Errorf("%s was asked in %q", name, lang) }
    }
    for _, name := range []string{"addresses active 15", "chart consumption year", "chart blocks month", "mempool -1"} {
        if r.count(name) != 1 { t.Errorf("%s called %d times", name, r.count(name)) }
    }
    var miner app.Info
    call(t, srv, "GET", "/api/miner?name=Foundry+USA", ld, nil)
    _, body = call(t, srv, "GET", "/api/miner?name=Foundry+USA", ld, nil)
    if json.Unmarshal([]byte(body), &miner) != nil || miner.Chart == nil || miner.Chart.Name != "Foundry USA" { t.Errorf("miner page %s", body) }
}

// What a request names is checked before it reaches main: an edited URL gets a
// 400, or the page's own default for a selection it does not know.
func TestAPIRefusesNonsense(t *testing.T) {
    var r, srv, _ = serveApp(t)
    var ld = launch(1)
    for _, path := range []string{"/api/block?height=-1", "/api/block?height=x", "/api/tx?id=abc", "/api/tx?id=" + strings.Repeat("g", 64),
        "/api/address?a=", "/api/addrtxs?a=bc1q&from=0", "/api/miner?name=", "/api/minerchart?name="} {
        if code, _ := call(t, srv, "GET", path, ld, nil); code != http.StatusBadRequest { t.Errorf("%s answered %d", path, code) }
    }
    call(t, srv, "GET", "/api/addresses?kind=everyone", ld, nil)
    call(t, srv, "GET", "/api/minerchart?name=x&data=power&period=decade", ld, nil)
    if r.count("addresses rich 0") != 1 || r.count("chart blocks month") != 1 { t.Errorf("calls %v", r.calls) }
}

// The watch button belongs on what the page turned out to be: a transaction or
// an address, but not a block — even one asked for as a transaction, since a
// block hash has a txid's shape — and not a page with nothing on it.
func TestDetailsCarryTheirWatchKind(t *testing.T) {
    var _, srv, _ = serveApp(t)
    var ld = launch(1)
    var get = func(path string) app.Info {
        var _, body = call(t, srv, "GET", path, ld, nil)
        var info app.Info
        if err := json.Unmarshal([]byte(body), &info); err != nil { t.Fatalf("%s: %v", path, err) }
        return info
    }
    var txid = strings.Repeat("ab", 32)
    if i := get("/api/tx?id=" + txid); i.Kind != "tx" || i.Id != txid { t.Errorf("transaction: %q %q", i.Kind, i.Id) }
    if i := get("/api/tx?id=" + strings.Repeat("0", 64)); i.Kind != "" || i.Title != "Блок 1" { t.Errorf("block hash: %q %q", i.Kind, i.Title) }
    if i := get("/api/address?a=bc1qx"); i.Kind != "address" || i.Id != "bc1qx" { t.Errorf("address: %q %q", i.Kind, i.Id) }
    if i := get("/api/address?a=nope"); i.Kind != "" { t.Errorf("an address with nothing found has kind %q", i.Kind) }
    if i := get("/api/block?height=5"); i.Kind != "" { t.Errorf("block: %q", i.Kind) }
}

// An answer is served from memory until the event that moves it: a second
// request asks main nothing, the card's own event drops it, and a new block
// drops everything. What a new block put above the list, the mempool and the
// watch list are never kept.
func TestCacheIsClearedByEvents(t *testing.T) {
    var r, srv, _ = serveApp(t)
    var ld = launch(1)
    for i := 0; i < 2; i++ {
        call(t, srv, "GET", "/api/fees", ld, nil)
        call(t, srv, "GET", "/api/block?height=5", ld, nil)
        call(t, srv, "GET", "/api/blocks?after=9", ld, nil)
        call(t, srv, "GET", "/api/mempool?after=3", ld, nil)
        call(t, srv, "GET", "/api/watches", ld, nil)
    }
    if r.count("fees") != 1 || r.count("block") != 1 { t.Fatalf("cached calls %v", r.calls) }
    if r.count("blocks") != 2 || r.count("mempool 3") != 2 || len(r.chats) != 2 { t.Fatalf("uncached calls %v, watches %d", r.calls, len(r.chats)) }
    Notify("fees")
    call(t, srv, "GET", "/api/fees", ld, nil)
    call(t, srv, "GET", "/api/block?height=5", ld, nil)
    if r.count("fees") != 2 || r.count("block") != 1 { t.Fatalf("after a fees event %v", r.calls) }
    Notify("blocks")
    call(t, srv, "GET", "/api/fees", ld, nil)
    call(t, srv, "GET", "/api/block?height=5", ld, nil)
    if r.count("fees") != 3 || r.count("block") != 2 { t.Fatalf("after a block %v", r.calls) }
}

// The watch list, the button and the alias are the reader's own: they act on
// the dialog the app was opened from, and opened anywhere else there is no
// chat to act on.
func TestWatchesActOnTheDialog(t *testing.T) {
    var r, srv, _ = serveApp(t)
    var ld = launch(376690654)
    var code, body = call(t, srv, "GET", "/api/watches", ld, nil)
    if code != http.StatusOK || !strings.Contains(body, "bc1q376690654") { t.Fatalf("watches: %d %s", code, body) }
    if _, body = call(t, srv, "GET", "/api/watch?kind=address&id=bc1qon", ld, nil); body != `{"On":true,"Error":false}` { t.Errorf("watching: %s", body) }
    if _, body = call(t, srv, "GET", "/api/watch?kind=address&id=bc1qoff", ld, nil); body != `{"On":false,"Error":false}` { t.Errorf("not watching: %s", body) }
    if _, body = call(t, srv, "POST", "/api/watch?kind=tx&id=abc&on=1", ld, nil); body != `{"On":true,"Error":false}` { t.Errorf("set: %s", body) }
    call(t, srv, "POST", "/api/watch?kind=address&id=bc1qx&on=0", ld, nil)
    if strings.Join(r.set, "|") != "376690654 tx abc true|376690654 address bc1qx false" { t.Errorf("SetWatch got %q", r.set) }
    if code, _ = call(t, srv, "GET", "/api/watch?kind=block&id=5", ld, nil); code != http.StatusBadRequest { t.Errorf("a block was watchable: %d", code) }
    var group = sign(testToken, map[string]string{"auth_date": strconv.FormatInt(time.Now().Unix(), 10), "chat": `{"id":5,"type":"CHAT"}`})
    for _, path := range []string{"/api/watches", "/api/watch?kind=address&id=bc1qon"} {
        if code, _ = call(t, srv, "GET", path, group, nil); code != http.StatusForbidden { t.Errorf("%s from a group: %d", path, code) }
    }
    if code, _ = call(t, srv, "POST", "/api/alias", group, url.Values{"kind": {"address"}, "id": {"bc1qx"}, "alias": {"a"}}); code != http.StatusForbidden {
        t.Errorf("alias from a group: %d", code)
    }
}

// An empty alias leaves the watch alone — Save on an empty field is how the
// dialog is dismissed — and a long one is cut rather than refused.
func TestAlias(t *testing.T) {
    var r, srv, _ = serveApp(t)
    var ld = launch(7)
    if code, _ := call(t, srv, "GET", "/api/alias", ld, nil); code != http.StatusMethodNotAllowed { t.Errorf("GET answered %d", code) }
    if code, _ := call(t, srv, "POST", "/api/alias", ld, url.Values{"kind": {"address"}, "id": {"bc1qx"}, "alias": {"  "}}); code != http.StatusNoContent {
        t.Errorf("empty alias answered %d", code)
    }
    var long = strings.Repeat("я", 70)
    if code, _ := call(t, srv, "POST", "/api/alias", ld, url.Values{"kind": {"address"}, "id": {"bc1qx"}, "alias": {long}}); code != http.StatusNoContent {
        t.Errorf("alias answered %d", code)
    }
    if len(r.aliases) != 1 || r.aliases[0] != "7 address bc1qx "+strings.Repeat("я", aliasMax) { t.Errorf("SetAlias got %q", r.aliases) }
}

// The stream carries event names and nothing else, needs no signature, and ends
// when the server starts shutting down.
func TestEventStream(t *testing.T) {
    var _, srv, closing = serveApp(t)
    var resp, err = http.Get(srv.URL + "/events")
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" { t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type")) }
    for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
        subsMu.Lock()
        var n = len(subs)
        subsMu.Unlock()
        if n > 0 { break }
        if time.Now().After(deadline) { t.Fatal("the stream never subscribed") }
    }
    Notify("blocks")
    var lines = bufio.NewReader(resp.Body)
    var first, _ = lines.ReadString('\n')
    var second, _ = lines.ReadString('\n')
    if first != "event: blocks\n" || second != "data: 1\n" { t.Fatalf("got %q %q", first, second) }
    close(closing)
    var done = make(chan struct{})
    go func() { io.Copy(io.Discard, resp.Body); close(done) }()
    select {
    case <-done:
    case <-time.After(5 * time.Second):
        t.Fatal("the stream outlived the shutdown")
    }
}

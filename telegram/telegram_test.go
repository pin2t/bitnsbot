package telegram

import "bufio"
import "encoding/json"
import "io"
import "net"
import "net/http"
import "net/http/httptest"
import "strconv"
import "strings"
import "sync"
import "testing"
import "time"
import "bitnsbot/app"

// fakeAPI stands in for the Bot API server, recording each method called and
// its decoded body, and answering every call ok.
type fakeAPI struct {
    mu      sync.Mutex
    methods []string
    bodies  []map[string]any
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body map[string]any
        json.NewDecoder(r.Body).Decode(&body)
        f.mu.Lock()
        f.methods = append(f.methods, r.URL.Path)
        f.bodies = append(f.bodies, body)
        f.mu.Unlock()
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    t.Cleanup(srv.Close)
    return srv
}

// called records what the webhook handed to its handlers.
type called struct {
    mu   sync.Mutex
    what []string
    bodies []string
}

func (c *called) options(secret string) Options {
    var add = func(s string, body []byte) {
        c.mu.Lock()
        c.what = append(c.what, s)
        c.bodies = append(c.bodies, string(body))
        c.mu.Unlock()
    }
    return Options{
        Secret:   secret,
        Message:  func(platform string, chat int64, name, text, lang string, body []byte) { add(platform + " message " + strconv.FormatInt(chat, 10) + " " + name + " " + text + " " + lang, body) },
        Callback: func(platform string, chat int64, name, data, lang string, body []byte) { add(platform + " callback " + strconv.FormatInt(chat, 10) + " " + name + " " + data + " " + lang, body) },
    }
}

func postUpdate(t *testing.T, h http.HandlerFunc, secret, update string) int {
    t.Helper()
    var req = httptest.NewRequest(http.MethodPost, "/bot", strings.NewReader(update))
    if secret != "" { req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secret) }
    var rec = httptest.NewRecorder()
    h(rec, req)
    return rec.Code
}

// A message reaches Message with its chat, the sender named by username, else
// first name, else "unknown", and their client's language. A tapped button is
// answered before anything else and then reaches Callback; one with no message
// or no data is answered and goes no further. Each handler is given the update
// exactly as it was posted.
func TestWebhookRoutesUpdates(t *testing.T) {
    var f = &fakeAPI{}
    var c = &called{}
    if err := Start(Options{Token: "T", APIBaseURL: f.server(t).URL}); err != nil { t.Fatal(err) }
    var h = webhook(c.options(""))
    var updates = []string{
        `{"message":{"chat":{"id":42},"from":{"username":"alice","first_name":"Alice","language_code":"ru"},"text":"/start"}}`,
        `{"message":{"chat":{"id":7},"from":{"first_name":"Bob"},"text":"hi"}}`,
        `{"message":{"chat":{"id":9},"text":"anon"}}`,
        `{"callback_query":{"id":"q1","from":{"username":"carol","language_code":"es"},"message":{"chat":{"id":5}},"data":"100"}}`,
        `{"callback_query":{"id":"q2","data":"100"}}`,
        `{"update_id":1}`,
    }
    for _, u := range updates {
        if code := postUpdate(t, h, "", u); code != http.StatusOK { t.Fatalf("status %d for %s", code, u) }
    }
    var want = []string{"tg message 42 alice /start ru", "tg message 7 Bob hi ", "tg message 9 unknown anon ", "tg callback 5 carol 100 es"}
    if strings.Join(c.what, "|") != strings.Join(want, "|") { t.Fatalf("handled\n %q\nwant %q", c.what, want) }
    if strings.Join(c.bodies, "|") != strings.Join(updates[:4], "|") { t.Fatalf("bodies\n %q\nwant %q", c.bodies, updates[:4]) }
    if len(f.methods) != 2 || f.methods[0] != "/botT/answerCallbackQuery" || f.bodies[0]["callback_query_id"] != "q1" || f.bodies[1]["callback_query_id"] != "q2" {
        t.Fatalf("answered %v %v", f.methods, f.bodies)
    }
    if code := postUpdate(t, h, "", "not json"); code != http.StatusBadRequest { t.Fatalf("garbage answered %d", code) }
}

func TestWebhookChecksSecret(t *testing.T) {
    var c = &called{}
    var h = webhook(c.options("s3cret"))
    var update = `{"message":{"chat":{"id":1},"text":"/fees"}}`
    for _, tc := range []struct{ secret string; code, handled int }{{"", 401, 0}, {"wrong", 401, 0}, {"s3cret", 200, 1}} {
        if code := postUpdate(t, h, tc.secret, update); code != tc.code || len(c.what) != tc.handled {
            t.Fatalf("secret %q: status %d, handled %d", tc.secret, code, len(c.what))
        }
    }
    var rec = httptest.NewRecorder()
    h(rec, httptest.NewRequest(http.MethodGet, "/bot", nil))
    if rec.Code != http.StatusMethodNotAllowed { t.Fatalf("GET answered %d", rec.Code) }
}

// Send posts HTML with link previews off, and the ids as an inline keyboard two
// to a row, each labelled with the shortened id and carrying the full one as
// its callback_data. A message with no ids carries no keyboard.
func TestSendButtons(t *testing.T) {
    var f = &fakeAPI{}
    if err := Start(Options{Token: "T", APIBaseURL: f.server(t).URL}); err != nil { t.Fatal(err) }
    var txid = strings.Repeat("d", 64)
    if err := Send(t.Context(), 3, "<b>hi</b>", []string{txid, "100", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}); err != nil { t.Fatal(err) }
    if f.methods[0] != "/botT/sendMessage" || f.bodies[0]["parse_mode"] != "HTML" || f.bodies[0]["text"] != "<b>hi</b>" || f.bodies[0]["chat_id"] != float64(3) {
        t.Fatalf("sent %s %v", f.methods[0], f.bodies[0])
    }
    var raw, _ = json.Marshal(f.bodies[0]["reply_markup"])
    var want = `{"inline_keyboard":[[{"callback_data":"` + txid + `","text":"dddddd...dddddd"},{"callback_data":"100","text":"100"}],[{"callback_data":"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa","text":"1A1zP1...DivfNa"}]]}`
    if string(raw) != want { t.Fatalf("reply_markup\n got %s\nwant %s", raw, want) }
    if err := Send(t.Context(), 3, "plain", nil); err != nil { t.Fatal(err) }
    if _, ok := f.bodies[1]["reply_markup"]; ok { t.Fatalf("a message with no ids got a keyboard: %v", f.bodies[1]) }
}

// Start registers the webhook with its secret, publishes each language's menu
// in a fixed order, and serves updates on Listen at WebhookPath until
// Shutdown. Registering with no URL would remove the webhook, so it is refused.
func TestStartRegistersAndServes(t *testing.T) {
    var f = &fakeAPI{}
    var ln, err = net.Listen("tcp", "127.0.0.1:0")
    if err != nil { t.Fatal(err) }
    var addr = ln.Addr().String()
    ln.Close()
    var c = &called{}
    var opt = c.options("s3cret")
    opt.Token, opt.APIBaseURL, opt.Listen, opt.WebhookPath = "T", f.server(t).URL, addr, "/bot"
    opt.Register, opt.WebhookURL = true, "https://example.org/bot"
    opt.Menus = map[string][]Command{"ru": {{"start", "показать"}}, "": {{"start", "show"}}}
    if err := Start(opt); err != nil { t.Fatal(err) }
    defer Shutdown(t.Context())
    if strings.Join(f.methods, " ") != "/botT/setWebhook /botT/setMyCommands /botT/setMyCommands" { t.Fatalf("called %v", f.methods) }
    if f.bodies[0]["url"] != "https://example.org/bot" || f.bodies[0]["secret_token"] != "s3cret" { t.Fatalf("setWebhook %v", f.bodies[0]) }
    if _, ok := f.bodies[1]["language_code"]; ok || f.bodies[2]["language_code"] != "ru" { t.Fatalf("menus %v %v", f.bodies[1], f.bodies[2]) }
    var req, _ = http.NewRequest(http.MethodPost, "http://"+addr+"/bot", strings.NewReader(`{"message":{"chat":{"id":1},"text":"/fees"}}`))
    req.Header.Set("X-Telegram-Bot-Api-Secret-Token", "s3cret")
    var resp, perr = http.DefaultClient.Do(req)
    if perr != nil { t.Fatal(perr) }
    resp.Body.Close()
    if resp.StatusCode != http.StatusOK || strings.Join(c.what, "|") != "tg message 1 unknown /fees " { t.Fatalf("status %d, handled %q", resp.StatusCode, c.what) }
    if err := Shutdown(t.Context()); err != nil { t.Fatal(err) }
    if _, err := http.Post("http://"+addr+"/bot", "application/json", strings.NewReader(`{}`)); err == nil { t.Fatal("the webhook still answers after Shutdown") }
    if err := Start(Options{Token: "T", APIBaseURL: f.server(t).URL, Register: true}); err == nil { t.Fatal("registered a webhook with no URL") }
}

// Start serves the Mini App on AppListen from the data App supplies, and
// registers it for what app.Notify announces: an event reaches a page's open
// stream. Shutdown stops it along with the webhook.
func TestStartServesTheMiniApp(t *testing.T) {
    var ln, err = net.Listen("tcp", "127.0.0.1:0")
    if err != nil { t.Fatal(err) }
    var addr = ln.Addr().String()
    ln.Close()
    invalidateAll()
    if err := Start(Options{Token: "TESTTOKEN", AppListen: addr, App: fakeSource{f: liveFees()}.options()}); err != nil { t.Fatal(err) }
    defer Shutdown(t.Context())
    var resp *http.Response
    var deadline = time.Now().Add(2 * time.Second)
    for {
        resp, err = http.Get("http://" + addr + "/")
        if err == nil || time.Now().After(deadline) { break }
        time.Sleep(10 * time.Millisecond)
    }
    if err != nil { t.Fatal(err) }
    var page, _ = io.ReadAll(resp.Body)
    resp.Body.Close()
    if resp.StatusCode != http.StatusOK || !strings.HasSuffix(strings.TrimSpace(string(page)), "</html>") { t.Fatalf("/ answered %d", resp.StatusCode) }
    var before = subscribes()
    var stream, serr = http.Get("http://" + addr + "/events")
    if serr != nil { t.Fatal(serr) }
    defer stream.Body.Close()
    for subscribes() == before && time.Now().Before(deadline) { time.Sleep(5 * time.Millisecond) }
    var heard = make(chan bool, 1)
    go func() {
        var r = bufio.NewReader(stream.Body)
        for {
            var line, rerr = r.ReadString('\n')
            if rerr != nil {
                heard <- false
                return
            }
            if line == "event: fees\n" {
                heard <- true
                return
            }
        }
    }()
    app.Notify("fees")
    select {
    case ok := <-heard:
        if !ok { t.Fatal("the stream ended without the event") }
    case <-time.After(2 * time.Second):
        t.Fatal("app.Notify never reached the Mini App's stream")
    }
    if err := Shutdown(t.Context()); err != nil { t.Fatal(err) }
    if _, err := http.Get("http://" + addr + "/"); err == nil { t.Fatal("the Mini App still answers after Shutdown") }
}

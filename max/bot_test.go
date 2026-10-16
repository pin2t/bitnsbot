package max

import "encoding/json"
import "io"
import "net"
import "net/http"
import "net/http/httptest"
import "strconv"
import "strings"
import "sync"
import "testing"

// fakeAPI stands in for the MAX Bot API, recording each request's path,
// Authorization header and decoded body.
type fakeAPI struct {
    mu       sync.Mutex
    paths    []string
    auths    []string
    bodies   []map[string]any
    response string
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
    var srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var raw, _ = io.ReadAll(r.Body)
        var body map[string]any
        json.Unmarshal(raw, &body)
        f.mu.Lock()
        f.paths = append(f.paths, r.URL.RequestURI())
        f.auths = append(f.auths, r.Header.Get("Authorization"))
        f.bodies = append(f.bodies, body)
        f.mu.Unlock()
        if f.response != "" { io.WriteString(w, f.response) } else { io.WriteString(w, `{}`) }
    }))
    t.Cleanup(srv.Close)
    return srv
}

func TestSubscribe(t *testing.T) {
    var f = &fakeAPI{response: `{"success":true}`}
    if err := Start(Options{Token: "MAXTOKEN", APIBaseURL: f.server(t).URL}); err != nil { t.Fatal(err) }
    if err := subscribe(t.Context(), "https://example.org/webhook", "s3cret"); err != nil { t.Fatal(err) }
    if f.paths[0] != "/subscriptions" || f.auths[0] != "MAXTOKEN" { t.Fatalf("got %s with auth %q", f.paths[0], f.auths[0]) }
    if f.bodies[0]["url"] != "https://example.org/webhook" || f.bodies[0]["secret"] != "s3cret" { t.Fatalf("body %v", f.bodies[0]) }
    var types, _ = json.Marshal(f.bodies[0]["update_types"])
    if string(types) != `["message_created","message_callback","bot_started","bot_added"]` { t.Fatalf("update_types %s", types) }
    if err := subscribe(t.Context(), "https://example.org/webhook", ""); err != nil { t.Fatal(err) }
    if _, ok := f.bodies[1]["secret"]; ok { t.Fatalf("secret sent although none is set: %v", f.bodies[1]) }
}

func TestSubscribeRefused(t *testing.T) {
    var f = &fakeAPI{response: `{"success":false,"message":"bad url"}`}
    if err := Start(Options{Token: "MAXTOKEN", APIBaseURL: f.server(t).URL}); err != nil { t.Fatal(err) }
    var err = subscribe(t.Context(), "http://x", "")
    if err == nil || !strings.Contains(err.Error(), "bad url") { t.Fatalf("got %v", err) }
}

// The ids become one inline keyboard of callback buttons, two to a row, each
// labelled with the shortened id and carrying the full id as its payload, and a
// message with none carries no keyboard at all. Send goes to the API and with
// the token Start set.
func TestSendButtons(t *testing.T) {
    var f = &fakeAPI{}
    if err := Start(Options{Token: "MAXTOKEN", APIBaseURL: f.server(t).URL}); err != nil { t.Fatal(err) }
    var txid = strings.Repeat("d", 64)
    if err := Send(t.Context(), 3, "<b>hi</b>", []string{txid, "100", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}); err != nil { t.Fatal(err) }
    if f.paths[0] != "/messages?chat_id=3" || f.auths[0] != "MAXTOKEN" || f.bodies[0]["format"] != "html" || f.bodies[0]["text"] != "<b>hi</b>" {
        t.Fatalf("sent %s %q %v", f.paths[0], f.auths[0], f.bodies[0])
    }
    var raw, _ = json.Marshal(f.bodies[0]["attachments"])
    var want = `[{"payload":{"buttons":[[{"payload":"` + txid + `","text":"dddddd...dddddd","type":"callback"},{"payload":"100","text":"100","type":"callback"}],[{"payload":"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa","text":"1A1zP1...DivfNa","type":"callback"}]]},"type":"inline_keyboard"}]`
    if string(raw) != want { t.Fatalf("attachments\n got %s\nwant %s", raw, want) }
    if err := Send(t.Context(), 3, "plain", nil); err != nil { t.Fatal(err) }
    if _, ok := f.bodies[1]["attachments"]; ok { t.Fatalf("a message with no ids got a keyboard: %v", f.bodies[1]) }
}

// called records what the webhook handed to its handlers.
type called struct {
    mu   sync.Mutex
    what []string
}

func (c *called) handlers(secret string) Options {
    var add = func(s string) {
        c.mu.Lock()
        c.what = append(c.what, s)
        c.mu.Unlock()
    }
    return Options{
        Secret:   secret,
        Message:  func(platform string, chat int64, name, text, lang string) { add(platform + " message " + strconv.FormatInt(chat, 10) + " " + name + " " + text + " " + lang) },
        Callback: func(platform string, chat int64, name, payload, lang string) { add(platform + " callback " + strconv.FormatInt(chat, 10) + " " + name + " " + payload + " " + lang) },
        Started:  func(platform string, chat int64, name, lang string) { add(platform + " started " + strconv.FormatInt(chat, 10) + " " + name + " " + lang) },
    }
}

func post(t *testing.T, h http.HandlerFunc, secret, update string) int {
    t.Helper()
    var req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(update))
    if secret != "" { req.Header.Set("X-Max-Bot-Api-Secret", secret) }
    var rec = httptest.NewRecorder()
    h(rec, req)
    return rec.Code
}

// Each update reaches the handler for its type, with the chat a reply belongs in
// and the locale reduced to a language. A message from a bot reaches none, so
// the bot never answers itself, and an update of any other type is acknowledged
// and ignored.
func TestWebhookRoutesUpdates(t *testing.T) {
    var c = &called{}
    var h = webhook(c.handlers(""))
    var updates = []string{
        `{"message":{"recipient":{"chat_type":"dialog","chat_id":376690654,"user_id":514924285},"timestamp":1791551633577,"body":{"mid":"mid.1","seq":1,"text":"/start"},"sender":{"user_id":146983203,"first_name":"Илья","is_bot":false,"name":"Илья"}},"timestamp":1791551633577,"user_locale":"ru","update_type":"message_created"}`,
        `{"message":{"recipient":{"chat_id":9},"body":{"text":"/fees"},"sender":{"user_id":1,"is_bot":true}},"update_type":"message_created"}`,
        `{"update_type":"message_callback","user_locale":"es-ES","callback":{"callback_id":"cb1","payload":"100","user":{"user_id":9}},"message":{"recipient":{"chat_id":77,"chat_type":"dialog"},"body":{"text":"x"}}}`,
        `{"update_type":"bot_started","chat_id":5,"user":{"user_id":6},"user_locale":"en_US"}`,
        `{"update_type":"bot_added","chat_id":8}`,
        `{"update_type":"message_removed","chat_id":8}`,
    }
    for _, u := range updates {
        if code := post(t, h, "", u); code != http.StatusOK { t.Fatalf("status %d for %s", code, u) }
    }
    var want = []string{"max message 376690654 Илья /start ru", "max callback 77 unknown 100 es", "max started 5 unknown en", "max started 8 unknown "}
    if strings.Join(c.what, "|") != strings.Join(want, "|") { t.Fatalf("handled\n %q\nwant %q", c.what, want) }
    if code := post(t, h, "", "not json"); code != http.StatusBadRequest { t.Fatalf("garbage answered %d", code) }
}

func TestWebhookChecksSecret(t *testing.T) {
    var c = &called{}
    var h = webhook(c.handlers("s3cret"))
    var update = `{"update_type":"bot_added","chat_id":1,"user_locale":"en"}`
    for _, tc := range []struct{ secret string; code, handled int }{{"", 401, 0}, {"wrong", 401, 0}, {"s3cret", 200, 1}} {
        if code := post(t, h, tc.secret, update); code != tc.code || len(c.what) != tc.handled {
            t.Fatalf("secret %q: status %d, handled %d", tc.secret, code, len(c.what))
        }
    }
    var rec = httptest.NewRecorder()
    h(rec, httptest.NewRequest(http.MethodGet, "/webhook", nil))
    if rec.Code != http.StatusMethodNotAllowed { t.Fatalf("GET answered %d", rec.Code) }
}

func TestLangOf(t *testing.T) {
    for in, want := range map[string]string{"ru": "ru", "ru-RU": "ru", "es_ES": "es", "EN": "en", "": ""} {
        if got := langOf(in); got != want { t.Errorf("langOf(%q) = %q, want %q", in, got, want) }
    }
}

// Start serves the webhook on Listen at the path of WebhookURL, checking the
// secret, subscribes that URL with the same secret, and Shutdown stops the
// server.
func TestStartServesAndSubscribes(t *testing.T) {
    var f = &fakeAPI{response: `{"success":true}`}
    var ln, err = net.Listen("tcp", "127.0.0.1:0")
    if err != nil { t.Fatal(err) }
    var addr = ln.Addr().String()
    ln.Close()
    var c = &called{}
    var opt = c.handlers("s3cret")
    opt.Token, opt.APIBaseURL, opt.Listen, opt.WebhookURL = "MAXTOKEN", f.server(t).URL, addr, "https://example.org/max/hook"
    if err := Start(opt); err != nil { t.Fatal(err) }
    defer Shutdown(t.Context())
    if len(f.paths) != 1 || f.paths[0] != "/subscriptions" || f.bodies[0]["secret"] != "s3cret" { t.Fatalf("subscribed %v %v", f.paths, f.bodies) }
    var req, _ = http.NewRequest(http.MethodPost, "http://"+addr+"/max/hook", strings.NewReader(`{"update_type":"bot_started","chat_id":5,"user_locale":"ru"}`))
    req.Header.Set("X-Max-Bot-Api-Secret", "s3cret")
    var resp, perr = http.DefaultClient.Do(req)
    if perr != nil { t.Fatal(perr) }
    resp.Body.Close()
    if resp.StatusCode != http.StatusOK || strings.Join(c.what, "|") != "max started 5 unknown ru" { t.Fatalf("status %d, handled %q", resp.StatusCode, c.what) }
    if err := Shutdown(t.Context()); err != nil { t.Fatal(err) }
    if _, err := http.Post("http://"+addr+"/max/hook", "application/json", strings.NewReader(`{}`)); err == nil { t.Fatal("the webhook still answers after Shutdown") }
}

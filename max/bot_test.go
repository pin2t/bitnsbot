package max

import "encoding/json"
import "io"
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
    var b = New("MAXTOKEN", f.server(t).URL)
    if err := b.Subscribe(t.Context(), "https://example.org/webhook", "s3cret"); err != nil { t.Fatal(err) }
    if f.paths[0] != "/subscriptions" || f.auths[0] != "MAXTOKEN" { t.Fatalf("got %s with auth %q", f.paths[0], f.auths[0]) }
    if f.bodies[0]["url"] != "https://example.org/webhook" || f.bodies[0]["secret"] != "s3cret" { t.Fatalf("body %v", f.bodies[0]) }
    var types, _ = json.Marshal(f.bodies[0]["update_types"])
    if string(types) != `["message_created","message_callback","bot_started","bot_added"]` { t.Fatalf("update_types %s", types) }
    if err := b.Subscribe(t.Context(), "https://example.org/webhook", ""); err != nil { t.Fatal(err) }
    if _, ok := f.bodies[1]["secret"]; ok { t.Fatalf("secret sent although none is set: %v", f.bodies[1]) }
}

func TestSubscribeRefused(t *testing.T) {
    var f = &fakeAPI{response: `{"success":false,"message":"bad url"}`}
    var err = New("MAXTOKEN", f.server(t).URL).Subscribe(t.Context(), "http://x", "")
    if err == nil || !strings.Contains(err.Error(), "bad url") { t.Fatalf("got %v", err) }
}

// The rows become one inline keyboard of callback buttons, in their rows, and a
// message with none carries no keyboard at all.
func TestSendButtons(t *testing.T) {
    var f = &fakeAPI{}
    var b = New("MAXTOKEN", f.server(t).URL)
    var rows = [][]Button{{{"a...b", "aaab"}, {"100", "100"}}, {{"c", "c"}}}
    if err := b.Send(t.Context(), 3, "<b>hi</b>", rows); err != nil { t.Fatal(err) }
    if f.paths[0] != "/messages?chat_id=3" || f.bodies[0]["format"] != "html" || f.bodies[0]["text"] != "<b>hi</b>" {
        t.Fatalf("sent %s %v", f.paths[0], f.bodies[0])
    }
    var raw, _ = json.Marshal(f.bodies[0]["attachments"])
    var want = `[{"payload":{"buttons":[[{"payload":"aaab","text":"a...b","type":"callback"},{"payload":"100","text":"100","type":"callback"}],[{"payload":"c","text":"c","type":"callback"}]]},"type":"inline_keyboard"}]`
    if string(raw) != want { t.Fatalf("attachments\n got %s\nwant %s", raw, want) }
    if err := b.Send(t.Context(), 3, "plain", nil); err != nil { t.Fatal(err) }
    if _, ok := f.bodies[1]["attachments"]; ok { t.Fatalf("a message with no buttons got a keyboard: %v", f.bodies[1]) }
}

// called records what the webhook handed to its handlers.
type called struct {
    mu   sync.Mutex
    what []string
}

func (c *called) handlers() Handlers {
    var add = func(s string) {
        c.mu.Lock()
        c.what = append(c.what, s)
        c.mu.Unlock()
    }
    return Handlers{
        Message:  func(chat int64, text, lang string) { add("message " + strconv.FormatInt(chat, 10) + " " + text + " " + lang) },
        Callback: func(chat int64, payload, lang string) { add("callback " + strconv.FormatInt(chat, 10) + " " + payload + " " + lang) },
        Started:  func(chat int64, lang string) { add("started " + strconv.FormatInt(chat, 10) + " " + lang) },
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
    var h = Webhook("", c.handlers())
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
    var want = []string{"message 376690654 /start ru", "callback 77 100 es", "started 5 en", "started 8 "}
    if strings.Join(c.what, "|") != strings.Join(want, "|") { t.Fatalf("handled\n %q\nwant %q", c.what, want) }
    if code := post(t, h, "", "not json"); code != http.StatusBadRequest { t.Fatalf("garbage answered %d", code) }
}

func TestWebhookChecksSecret(t *testing.T) {
    var c = &called{}
    var h = Webhook("s3cret", c.handlers())
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

func TestLang(t *testing.T) {
    for in, want := range map[string]string{"ru": "ru", "ru-RU": "ru", "es_ES": "es", "EN": "en", "": ""} {
        if got := Lang(in); got != want { t.Errorf("Lang(%q) = %q, want %q", in, got, want) }
    }
}

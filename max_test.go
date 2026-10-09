package main

import "encoding/json"
import "io"
import "net/http"
import "net/http/httptest"
import "strings"
import "sync"
import "testing"

// fakeMax stands in for the MAX Bot API, recording each request's path,
// Authorization header and decoded body.
type fakeMax struct {
    mu       sync.Mutex
    paths    []string
    auths    []string
    bodies   []map[string]any
    response string
}

func (f *fakeMax) server(t *testing.T) *httptest.Server {
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

func setMaxSecret(t *testing.T, secret string) {
    var old = *maxSecret
    *maxSecret = secret
    t.Cleanup(func() { *maxSecret = old })
}

func TestMaxSubscribe(t *testing.T) {
    var f = &fakeMax{response: `{"success":true}`}
    var mb = newMaxBot("MAXTOKEN", f.server(t).URL)
    if err := mb.subscribe(t.Context(), "https://example.org/webhook", "s3cret"); err != nil { t.Fatal(err) }
    if f.paths[0] != "/subscriptions" || f.auths[0] != "MAXTOKEN" { t.Fatalf("got %s with auth %q", f.paths[0], f.auths[0]) }
    if f.bodies[0]["url"] != "https://example.org/webhook" || f.bodies[0]["secret"] != "s3cret" { t.Fatalf("body %v", f.bodies[0]) }
    if err := mb.subscribe(t.Context(), "https://example.org/webhook", ""); err != nil { t.Fatal(err) }
    if _, ok := f.bodies[1]["secret"]; ok { t.Fatalf("secret sent although none is set: %v", f.bodies[1]) }
}

func TestMaxSubscribeRefused(t *testing.T) {
    var f = &fakeMax{response: `{"success":false,"message":"bad url"}`}
    var err = newMaxBot("MAXTOKEN", f.server(t).URL).subscribe(t.Context(), "http://x", "")
    if err == nil || !strings.Contains(err.Error(), "bad url") { t.Fatalf("got %v", err) }
}

func TestMaxBotStartedSendsStart(t *testing.T) {
    setMaxSecret(t, "")
    var f = &fakeMax{}
    var h = maxWebhookHandler(newMaxBot("MAXTOKEN", f.server(t).URL))
    var update = `{"update_type":"bot_started","timestamp":1791551633577,"chat_id":376690654,"user":{"user_id":146983203,"name":"Илья"},"user_locale":"ru"}`
    var rec = httptest.NewRecorder()
    h(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(update)))
    if rec.Code != http.StatusOK { t.Fatalf("status %d", rec.Code) }
    if len(f.paths) != 1 || f.paths[0] != "/messages?chat_id=376690654" { t.Fatalf("requests %v", f.paths) }
    if f.bodies[0]["text"] != startText("ru") || f.bodies[0]["format"] != "html" { t.Fatalf("body %v", f.bodies[0]) }
    if startText("ru") == startText("") { t.Fatal("start text is not translated") }
}

func TestMaxMessageCreatedIsOnlyLogged(t *testing.T) {
    setMaxSecret(t, "")
    var f = &fakeMax{}
    var h = maxWebhookHandler(newMaxBot("MAXTOKEN", f.server(t).URL))
    var update = `{"message":{"recipient":{"chat_type":"dialog","chat_id":376690654,"user_id":514924285},"timestamp":1791551633577,"body":{"mid":"mid.000000001673d7de01a120cc78a96a52","seq":117411127858129490,"text":"/start"},"sender":{"user_id":146983203,"first_name":"Илья","is_bot":false,"last_name":"","last_activity_time":1791551634000,"name":"Илья"}},"timestamp":1791551633577,"user_locale":"ru","update_type":"message_created"}`
    var rec = httptest.NewRecorder()
    h(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(update)))
    if rec.Code != http.StatusOK || len(f.paths) != 0 { t.Fatalf("status %d, requests %v", rec.Code, f.paths) }
    var u maxUpdate
    if err := json.Unmarshal([]byte(update), &u); err != nil { t.Fatal(err) }
    if u.Message.Body.Text != "/start" || u.Message.Recipient.ChatID != 376690654 || u.Message.Sender.UserID != 146983203 { t.Fatalf("decoded %+v", u.Message) }
}

func TestMaxWebhookChecksSecret(t *testing.T) {
    setMaxSecret(t, "s3cret")
    var f = &fakeMax{}
    var h = maxWebhookHandler(newMaxBot("MAXTOKEN", f.server(t).URL))
    var update = `{"update_type":"bot_added","chat_id":1,"user_locale":"en"}`
    for _, c := range []struct{ secret string; code, sent int }{{"", 401, 0}, {"wrong", 401, 0}, {"s3cret", 200, 1}} {
        var req = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(update))
        if c.secret != "" { req.Header.Set("X-Max-Bot-Api-Secret", c.secret) }
        var rec = httptest.NewRecorder()
        h(rec, req)
        if rec.Code != c.code || len(f.paths) != c.sent { t.Fatalf("secret %q: status %d, requests %d", c.secret, rec.Code, len(f.paths)) }
    }
}

func TestMaxLang(t *testing.T) {
    for in, want := range map[string]string{"ru": "ru", "ru-RU": "ru", "es_ES": "es", "EN": "en", "": ""} {
        if got := maxLang(in); got != want { t.Errorf("maxLang(%q) = %q, want %q", in, got, want) }
    }
}

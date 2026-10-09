package main

import "encoding/json"
import "io"
import "net/http"
import "net/http/httptest"
import "path/filepath"
import "strings"
import "sync"
import "testing"
import "bitnsbot/core/coretest"
import "bitnsbot/txwatches"
import "bitnsbot/watches"

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

// maxExample is a real message_created update, as MAX sent it.
const maxExample = `{"message":{"recipient":{"chat_type":"dialog","chat_id":376690654,"user_id":514924285},"timestamp":1791551633577,"body":{"mid":"mid.000000001673d7de01a120cc78a96a52","seq":117411127858129490,"text":"/start"},"sender":{"user_id":146983203,"first_name":"Илья","is_bot":false,"last_name":"","last_activity_time":1791551634000,"name":"Илья"}},"timestamp":1791551633577,"user_locale":"ru","update_type":"message_created"}`

// maxMessageUpdate is a message_created from chat with text, in English.
func maxMessageUpdate(chat int64, text string) string {
    var body, _ = json.Marshal(map[string]any{"update_type": "message_created", "user_locale": "en",
        "message": map[string]any{"recipient": map[string]any{"chat_id": chat, "chat_type": "dialog"},
            "sender": map[string]any{"user_id": chat, "is_bot": false}, "body": map[string]any{"text": text}}})
    return string(body)
}

func postMax(t *testing.T, h http.HandlerFunc, update string) {
    t.Helper()
    var rec = httptest.NewRecorder()
    h(rec, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(update)))
    if rec.Code != http.StatusOK { t.Fatalf("status %d for %s", rec.Code, update) }
}

// lastText is the text of the last message the fake MAX was asked to send.
func (f *fakeMax) lastText(t *testing.T) string {
    t.Helper()
    f.mu.Lock()
    defer f.mu.Unlock()
    if len(f.bodies) == 0 { t.Fatal("nothing was sent") }
    var text, _ = f.bodies[len(f.bodies)-1]["text"].(string)
    return text
}

// A message is a command, run through the same dispatch Telegram's are, and
// answered in the update's user_locale — the example MAX sent is /start in
// Russian. A message from a bot is not answered, so the bot never talks to
// itself.
func TestMaxMessageCreatedRunsCommands(t *testing.T) {
    setMaxSecret(t, "")
    var f = &fakeMax{}
    var h = maxWebhookHandler(newMaxBot("MAXTOKEN", f.server(t).URL))
    postMax(t, h, maxExample)
    if len(f.paths) != 1 || f.paths[0] != "/messages?chat_id=376690654" { t.Fatalf("requests %v", f.paths) }
    if f.lastText(t) != startText("ru") { t.Fatalf("sent %q", f.lastText(t)) }
    postMax(t, h, strings.Replace(maxExample, `"is_bot":false`, `"is_bot":true`, 1))
    if len(f.paths) != 1 { t.Fatalf("answered a bot: %v", f.paths) }
    var u maxUpdate
    if err := json.Unmarshal([]byte(maxExample), &u); err != nil { t.Fatal(err) }
    if u.Message.Body.Text != "/start" || u.Message.Recipient.ChatID != 376690654 || u.Message.Sender.UserID != 146983203 { t.Fatalf("decoded %+v", u.Message) }
}

// A watch made on MAX is filed under "max", and the platform is what scopes
// every watch command: Telegram's chat 5 neither lists nor removes MAX's chat 5's
// watch, and the other way round. The bare /watch's pending argument is MAX's
// own too.
func TestMaxWatchesAreScopedToThePlatform(t *testing.T) {
    setMaxSecret(t, "")
    var tgSent []string
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct{ Text string `json:"text"` }
        json.NewDecoder(r.Body).Decode(&body)
        tgSent = append(tgSent, body.Text)
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    var b = newBot("TESTTOKEN", tg.URL)
    if err := openDB(filepath.Join(t.TempDir(), "watches.db")); err != nil { t.Fatal(err) }
    defer closeDB()
    stopNotify()
    defer stopNotify()
    var f = &fakeMax{}
    var h = maxWebhookHandler(newMaxBot("MAXTOKEN", f.server(t).URL))
    var addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
    var txid = strings.Repeat("c", 64)
    postMax(t, h, maxMessageUpdate(5, "/watch "+addr+" Genesis"))
    if !strings.Contains(f.lastText(t), "Watching "+addr+" (Genesis)") { t.Fatalf("reply %q", f.lastText(t)) }
    postMax(t, h, maxMessageUpdate(5, "/watch"))
    if !pendingWatchChats[chatKey{"max", 5}] || pendingWatchChats[chatKey{"tg", 5}] { t.Fatal("pending is not MAX's") }
    postMax(t, h, maxMessageUpdate(5, txid))
    var list, err = watches.List()
    if err != nil { t.Fatal(err) }
    if len(list) != 1 || list[0].Platform != "max" || list[0].Chat != 5 { t.Fatalf("stored %+v", list) }
    if e := txwatches.For("max", 5); len(e) != 1 || e[0].Txid != txid { t.Fatalf("tx watches %+v", e) }
    update(b, Update{Message: &Message{Chat: Chat{ID: 5}, Text: "/watches"}})
    if tgSent[len(tgSent)-1] != "You're not watching anything yet" { t.Fatalf("Telegram listed %q", tgSent[len(tgSent)-1]) }
    update(b, Update{Message: &Message{Chat: Chat{ID: 5}, Text: "/unwatch " + addr}})
    if !strings.Contains(tgSent[len(tgSent)-1], "not watching") { t.Fatalf("Telegram unwatched: %q", tgSent[len(tgSent)-1]) }
    postMax(t, h, maxMessageUpdate(5, "/watches"))
    if !strings.Contains(f.lastText(t), addr) || !strings.Contains(f.lastText(t), txid) { t.Fatalf("MAX listed %q", f.lastText(t)) }
    postMax(t, h, maxMessageUpdate(5, "/unwatch "+addr))
    if !strings.Contains(f.lastText(t), "Stopped watching") { t.Fatalf("MAX unwatched: %q", f.lastText(t)) }
    if list, _ = watches.List(); len(list) != 0 { t.Fatalf("left %+v", list) }
}

// A tapped id button comes back as a message_callback and runs /info on its
// payload, in the chat the button's message was in.
func TestMaxCallbackRunsInfo(t *testing.T) {
    setMaxSecret(t, "")
    var f = &fakeMax{}
    var h = maxWebhookHandler(newMaxBot("MAXTOKEN", f.server(t).URL))
    postMax(t, h, `{"update_type":"message_callback","user_locale":"en","callback":{"callback_id":"cb1","payload":"100","user":{"user_id":9}},"message":{"recipient":{"chat_id":77,"chat_type":"dialog"},"body":{"text":"x"}}}`)
    if len(f.paths) != 1 || f.paths[0] != "/messages?chat_id=77" { t.Fatalf("requests %v", f.paths) }
    if f.lastText(t) != "Bitcoin node connection is not configured" { t.Fatalf("sent %q", f.lastText(t)) }
}

// Ids become an inline keyboard of callback buttons, two to a row like
// Telegram's, each carrying the full id as its payload.
func TestMaxButtons(t *testing.T) {
    var f = &fakeMax{}
    var mb = newMaxBot("MAXTOKEN", f.server(t).URL)
    var txid = strings.Repeat("d", 64)
    if err := mb.sendWithButtons(t.Context(), 3, "hi", []string{txid, "100", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"}); err != nil { t.Fatal(err) }
    var raw, _ = json.Marshal(f.bodies[0]["attachments"])
    var want = `[{"payload":{"buttons":[[{"payload":"` + txid + `","text":"` + short(txid) + `","type":"callback"},{"payload":"100","text":"100","type":"callback"}],[{"payload":"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa","text":"1A1zP1...DivfNa","type":"callback"}]]},"type":"inline_keyboard"}]`
    if string(raw) != want { t.Fatalf("attachments\n got %s\nwant %s", raw, want) }
    if err := mb.sendWithButtons(t.Context(), 3, "plain", nil); err != nil { t.Fatal(err) }
    if _, ok := f.bodies[1]["attachments"]; ok { t.Fatalf("a message with no ids got a keyboard: %v", f.bodies[1]) }
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

// A confirmation goes out on the platform its watch was made on: the MAX
// chat's on MAX, the Telegram chat's on Telegram, though both are chat 7.
func TestMaxConfirmationGoesToItsPlatform(t *testing.T) {
    var tgMu sync.Mutex
    var tgSent []string
    var tg = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        var body struct{ Text string `json:"text"` }
        json.NewDecoder(r.Body).Decode(&body)
        tgMu.Lock()
        tgSent = append(tgSent, body.Text)
        tgMu.Unlock()
        json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
    }))
    defer tg.Close()
    var maxTx = strings.Repeat("e", 64)
    var tgTx = strings.Repeat("f", 64)
    coretest.Start(t, func(method string, params []any) (any, error) {
        if method == "getblock" { return map[string]any{"height": 300, "tx": []string{maxTx, tgTx}}, nil }
        return nil, nil
    })
    stopNotify()
    defer stopNotify()
    var f = &fakeMax{}
    txwatches.Add(maxTx, "max", 7, "on max")
    txwatches.Add(tgTx, "tg", 7, "on telegram")
    processConfirms("hash300", newBot("TESTTOKEN", tg.URL), newMaxBot("MAXTOKEN", f.server(t).URL))
    if len(f.bodies) != 1 || !strings.Contains(f.lastText(t), "(on max)") { t.Fatalf("MAX got %v", f.bodies) }
    tgMu.Lock()
    defer tgMu.Unlock()
    if len(tgSent) != 1 || !strings.Contains(tgSent[0], "(on telegram)") { t.Fatalf("Telegram got %q", tgSent) }
}

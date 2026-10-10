// Package max is everything the bot does on the MAX messenger: the Bot API
// client, the webhook that receives updates, and the MAX Mini App — the same
// four tabs as the Telegram one, drawn with MAX UI and React instead of HTMX.
//
// Running a command is package main's business, so the webhook hands each
// update to the Handlers main passes, and the Mini App reads its chain data
// through the same app.Options functions the Telegram Mini App does.
package max

import "bytes"
import "context"
import "crypto/subtle"
import "encoding/json"
import "fmt"
import "io"
import "net/http"
import "net/url"
import "strings"
import "time"
import "bitnsbot/logging"

// BaseURL is the MAX Bot API. The docs name platform-api2 in place of the older
// platform-api; a package var so tests point it at an httptest server.
var BaseURL = "https://platform-api2.max.ru"

// updateTypes are the updates the bot subscribes to: a message, a tapped
// button, and the two events it answers with the start message.
var updateTypes = []string{"message_created", "message_callback", "bot_started", "bot_added"}

// Bot is the MAX Bot API client. The token travels in the Authorization header
// — MAX no longer accepts it as a query parameter — so, unlike the Telegram
// client, no URL it builds carries a secret.
type Bot struct {
    token      string
    baseURL    string
    httpClient *http.Client
}

// Update is MAX's Update object. Which fields are set depends on update_type: a
// message_created carries Message, a message_callback carries Callback and the
// Message its button was on, a bot_started or bot_added carries ChatID and User.
// UserLocale is the user's client language, e.g. "ru".
type Update struct {
    UpdateType string    `json:"update_type"`
    Timestamp  int64     `json:"timestamp"`
    ChatID     int64     `json:"chat_id"`
    User       *User     `json:"user"`
    Message    *Message  `json:"message"`
    Callback   *Callback `json:"callback"`
    UserLocale string    `json:"user_locale"`
    Payload    string    `json:"payload"`
    IsChannel  bool      `json:"is_channel"`
}

// Callback is a tapped callback button; Payload is what the button carried —
// here, the full id to look up.
type Callback struct {
    CallbackID string `json:"callback_id"`
    Payload    string `json:"payload"`
    User       *User  `json:"user"`
}

type User struct {
    UserID           int64  `json:"user_id"`
    FirstName        string `json:"first_name"`
    LastName         string `json:"last_name"`
    Name             string `json:"name"`
    Username         string `json:"username"`
    IsBot            bool   `json:"is_bot"`
    LastActivityTime int64  `json:"last_activity_time"`
}

type Message struct {
    Sender    *User     `json:"sender"`
    Recipient Recipient `json:"recipient"`
    Timestamp int64     `json:"timestamp"`
    Body      Body      `json:"body"`
}

type Recipient struct {
    ChatID   int64  `json:"chat_id"`
    ChatType string `json:"chat_type"`
    UserID   int64  `json:"user_id"`
}

type Body struct {
    Mid  string `json:"mid"`
    Seq  int64  `json:"seq"`
    Text string `json:"text"`
}

// Button is one callback button of a message's inline keyboard. Payload comes
// back in the message_callback a tap produces.
type Button struct {
    Text    string
    Payload string
}

func New(token, baseURL string) *Bot {
    return &Bot{
        token:      token,
        baseURL:    strings.TrimRight(baseURL, "/"),
        httpClient: &http.Client{Timeout: 15 * time.Second},
    }
}

// call sends one request to the MAX Bot API and decodes the reply into out.
// The subscription body carries the secret, so it is never logged.
func (b *Bot) call(ctx context.Context, method, path string, payload, out any) error {
    var buf, err = json.Marshal(payload)
    if err != nil { return err }
    if path == "/subscriptions" {
        logging.Net("max → %s %s (body omitted: contains secret)", method, path)
    } else {
        logging.Net("max → %s %s %s", method, path, buf)
    }
    var req, reqErr = http.NewRequestWithContext(ctx, method, b.baseURL+path, bytes.NewReader(buf))
    if reqErr != nil { return reqErr }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", b.token)
    var resp, doErr = b.httpClient.Do(req)
    if doErr != nil { return doErr }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil { return readErr }
    logging.Net("max ← %s %s %d %s", method, path, resp.StatusCode, body)
    if resp.StatusCode != http.StatusOK { return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, body) }
    if out == nil { return nil }
    return json.Unmarshal(body, out)
}

// Subscribe registers the webhook with POST /subscriptions. MAX answers 200
// either way and reports a refusal in the body, so success is read from there.
func (b *Bot) Subscribe(ctx context.Context, webhookURL, secret string) error {
    var payload = map[string]any{"url": webhookURL, "update_types": updateTypes}
    if secret != "" { payload["secret"] = secret }
    var res struct {
        Success bool   `json:"success"`
        Message string `json:"message"`
    }
    if err := b.call(ctx, http.MethodPost, "/subscriptions", payload, &res); err != nil { return err }
    if !res.Success { return fmt.Errorf("subscription refused: %s", res.Message) }
    return nil
}

// Send posts an HTML-formatted message to a chat. The text is the Telegram one:
// MAX's html format understands the same <b>, <a>, <code> and <pre>. The rows
// become an inline keyboard of callback buttons; a message with none carries
// no keyboard at all.
func (b *Bot) Send(ctx context.Context, chat int64, text string, rows [][]Button) error {
    var path = "/messages?" + url.Values{"chat_id": {fmt.Sprint(chat)}}.Encode()
    var payload = map[string]any{"text": text, "format": "html", "disable_link_preview": true}
    var keyboard [][]map[string]string
    for _, row := range rows {
        var buttons []map[string]string
        for _, btn := range row {
            buttons = append(buttons, map[string]string{"type": "callback", "text": btn.Text, "payload": btn.Payload})
        }
        keyboard = append(keyboard, buttons)
    }
    if len(keyboard) > 0 {
        payload["attachments"] = []any{map[string]any{"type": "inline_keyboard", "payload": map[string]any{"buttons": keyboard}}}
    }
    return b.call(ctx, http.MethodPost, path, payload, nil)
}

// Lang reduces a MAX user_locale ("ru", "ru-RU", "ru_RU") to the language code
// the translation tables are keyed by.
func Lang(locale string) string {
    var lang, _, _ = strings.Cut(strings.ToLower(locale), "-")
    lang, _, _ = strings.Cut(lang, "_")
    return lang
}

// Handlers are what the webhook does with an update. Running a command is
// package main's business, so main supplies them. lang is the update's
// user_locale reduced by Lang, empty when MAX sent none.
type Handlers struct {
    // Message is a message somebody typed: a command, or the argument a bare
    // command asked for.
    Message func(chat int64, text, lang string)
    // Callback is a tapped id button, payload the full id it carried.
    Callback func(chat int64, payload, lang string)
    // Started is the bot being started or added to a chat.
    Started func(chat int64, lang string)
}

// Webhook receives MAX updates. When secret is set every request must carry it
// in X-Max-Bot-Api-Secret, compared in constant time, since that is all that
// stops an arbitrary POST being taken for MAX. The update is logged at INFO and
// handed to h. A message from a bot is dropped, so the bot never answers
// itself. Every update is answered 200 once decoded, whatever its handler did:
// MAX unsubscribes a bot whose webhook keeps failing.
func Webhook(secret string, h Handlers) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
            return
        }
        if secret != "" {
            var got = r.Header.Get("X-Max-Bot-Api-Secret")
            if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
                http.Error(w, "unauthorized", http.StatusUnauthorized)
                return
            }
        }
        defer r.Body.Close()
        var body, readErr = io.ReadAll(r.Body)
        if readErr != nil {
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        logging.Info("MAX update: %s", body)
        var u Update
        if err := json.Unmarshal(body, &u); err != nil {
            logging.Err("decode MAX update: %v", err)
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        var lang = Lang(u.UserLocale)
        switch u.UpdateType {
        case "message_created":
            if u.Message == nil || (u.Message.Sender != nil && u.Message.Sender.IsBot) { break }
            h.Message(u.Message.Recipient.ChatID, u.Message.Body.Text, lang)
        case "message_callback":
            if u.Message == nil || u.Callback == nil || u.Callback.Payload == "" { break }
            h.Callback(u.Message.Recipient.ChatID, u.Callback.Payload, lang)
        case "bot_started", "bot_added":
            h.Started(u.ChatID, lang)
        }
        w.WriteHeader(http.StatusOK)
    }
}

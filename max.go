package main

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

// maxBaseURL is the MAX Bot API. The docs name platform-api2 in place of the
// older platform-api; a package var so tests point it at an httptest server.
var maxBaseURL = "https://platform-api2.max.ru"

// maxUpdateTypes are the updates the bot subscribes to: a message, a tapped
// button, and the two events it answers with the start message.
var maxUpdateTypes = []string{"message_created", "message_callback", "bot_started", "bot_added"}

// maxBot is the MAX Bot API client. The token travels in the Authorization
// header — MAX no longer accepts it as a query parameter — so, unlike the
// Telegram client, no URL it builds carries a secret.
type maxBot struct {
    token      string
    baseURL    string
    httpClient *http.Client
}

// maxUpdate is MAX's Update object. Which fields are set depends on
// update_type: a message_created carries Message, a message_callback carries
// Callback and the Message its button was on, a bot_started or bot_added
// carries ChatID and User. UserLocale is the user's client language, e.g. "ru".
type maxUpdate struct {
    UpdateType string       `json:"update_type"`
    Timestamp  int64        `json:"timestamp"`
    ChatID     int64        `json:"chat_id"`
    User       *maxUser     `json:"user"`
    Message    *maxMessage  `json:"message"`
    Callback   *maxCallback `json:"callback"`
    UserLocale string      `json:"user_locale"`
    Payload    string      `json:"payload"`
    IsChannel  bool        `json:"is_channel"`
}

// maxCallback is a tapped callback button; Payload is what the button carried —
// here, the full id to look up.
type maxCallback struct {
    CallbackID string   `json:"callback_id"`
    Payload    string   `json:"payload"`
    User       *maxUser `json:"user"`
}

type maxUser struct {
    UserID           int64  `json:"user_id"`
    FirstName        string `json:"first_name"`
    LastName         string `json:"last_name"`
    Name             string `json:"name"`
    Username         string `json:"username"`
    IsBot            bool   `json:"is_bot"`
    LastActivityTime int64  `json:"last_activity_time"`
}

type maxMessage struct {
    Sender    *maxUser       `json:"sender"`
    Recipient maxRecipient   `json:"recipient"`
    Timestamp int64          `json:"timestamp"`
    Body      maxMessageBody `json:"body"`
}

type maxRecipient struct {
    ChatID   int64  `json:"chat_id"`
    ChatType string `json:"chat_type"`
    UserID   int64  `json:"user_id"`
}

type maxMessageBody struct {
    Mid  string `json:"mid"`
    Seq  int64  `json:"seq"`
    Text string `json:"text"`
}

func newMaxBot(token, baseURL string) *maxBot {
    return &maxBot{
        token:      token,
        baseURL:    strings.TrimRight(baseURL, "/"),
        httpClient: &http.Client{Timeout: 15 * time.Second},
    }
}

// call sends one request to the MAX Bot API and decodes the reply into out.
// The subscription body carries the secret, so it is never logged.
func (b *maxBot) call(ctx context.Context, method, path string, payload, out any) error {
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

// subscribe registers the webhook with POST /subscriptions. MAX answers 200
// either way and reports a refusal in the body, so success is read from there.
func (b *maxBot) subscribe(ctx context.Context, webhookURL, secret string) error {
    var payload = map[string]any{"url": webhookURL, "update_types": maxUpdateTypes}
    if secret != "" { payload["secret"] = secret }
    var res struct {
        Success bool   `json:"success"`
        Message string `json:"message"`
    }
    if err := b.call(ctx, http.MethodPost, "/subscriptions", payload, &res); err != nil { return err }
    if !res.Success { return fmt.Errorf("subscription refused: %s", res.Message) }
    return nil
}

func (b *maxBot) platform() string { return "max" }

// sendWithButtons posts an HTML-formatted message to a chat. The text is the
// Telegram one: MAX's html format understands the same <b>, <a>, <code> and
// <pre>. The ids become an inline keyboard of callback buttons, laid out as
// Telegram's are (buttonRows), each carrying the full id as its payload so a tap
// comes back as a message_callback that runs /info on it.
func (b *maxBot) sendWithButtons(ctx context.Context, chat int64, text string, ids []string) error {
    var path = "/messages?" + url.Values{"chat_id": {fmt.Sprint(chat)}}.Encode()
    var payload = map[string]any{"text": text, "format": "html", "disable_link_preview": true}
    var rows [][]map[string]string
    for _, row := range buttonRows(ids) {
        var buttons []map[string]string
        for _, btn := range row {
            buttons = append(buttons, map[string]string{"type": "callback", "text": btn["text"], "payload": btn["callback_data"]})
        }
        rows = append(rows, buttons)
    }
    if len(rows) > 0 {
        payload["attachments"] = []any{map[string]any{"type": "inline_keyboard", "payload": map[string]any{"buttons": rows}}}
    }
    return b.call(ctx, http.MethodPost, path, payload, nil)
}

// maxLang reduces a MAX user_locale ("ru", "ru-RU", "ru_RU") to the language
// code the translation tables are keyed by.
func maxLang(locale string) string {
    var lang, _, _ = strings.Cut(strings.ToLower(locale), "-")
    lang, _, _ = strings.Cut(lang, "_")
    return lang
}

// maxWebhookHandler receives MAX updates. When -max-secret is set every request
// must carry it in X-Max-Bot-Api-Secret, compared in constant time, since that
// is all that stops an arbitrary POST being taken for MAX. The update is logged
// at INFO, and the chat's language set from its user_locale, which every reply
// is translated through. A message_created is a command or a pending argument,
// run through the same dispatch Telegram's messages are, so every command works
// here too; a message from a bot is ignored, so the bot never answers itself. A
// message_callback is a tapped id button, and runs /info on it. A bot_started or
// bot_added is answered with the start message. A failed reply is logged and
// still answered 200: MAX unsubscribes a bot whose webhook keeps failing.
func maxWebhookHandler(mb *maxBot) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
            return
        }
        if *maxSecret != "" {
            var got = r.Header.Get("X-Max-Bot-Api-Secret")
            if subtle.ConstantTimeCompare([]byte(got), []byte(*maxSecret)) != 1 {
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
        var u maxUpdate
        if err := json.Unmarshal(body, &u); err != nil {
            logging.Err("decode MAX update: %v", err)
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        switch u.UpdateType {
        case "message_created":
            if u.Message == nil || (u.Message.Sender != nil && u.Message.Sender.IsBot) { break }
            var chat = u.Message.Recipient.ChatID
            if u.UserLocale != "" { SetChatLanguage(chat, maxLang(u.UserLocale)) }
            dispatch(mb, chat, u.Message.Body.Text)
        case "message_callback":
            if u.Message == nil || u.Callback == nil || u.Callback.Payload == "" { break }
            var chat = u.Message.Recipient.ChatID
            if u.UserLocale != "" { SetChatLanguage(chat, maxLang(u.UserLocale)) }
            logging.Info("MAX callback %q from chat %d", short(u.Callback.Payload), chat)
            info(mb, chat, u.Callback.Payload)
        case "bot_started", "bot_added":
            if u.UserLocale != "" { SetChatLanguage(u.ChatID, maxLang(u.UserLocale)) }
            start(mb, u.ChatID)
        }
        w.WriteHeader(http.StatusOK)
    }
}

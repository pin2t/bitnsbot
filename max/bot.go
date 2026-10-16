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

// updateTypes are the updates the bot subscribes to: a message, a tapped
// button, and the two events it answers with the start message.
var updateTypes = []string{"message_created", "message_callback", "bot_started", "bot_added"}

// client is the MAX Bot API client. The token travels in the Authorization
// header — MAX no longer accepts it as a query parameter — so, unlike the
// Telegram client, no URL it builds carries a secret.
type client struct {
    token      string
    baseURL    string
    httpClient *http.Client
}

// update is MAX's Update object. Which fields are set depends on update_type: a
// message_created carries Message, a message_callback carries Callback and the
// Message its button was on, a bot_started or bot_added carries ChatID and User.
// UserLocale is the user's client language, e.g. "ru".
type update struct {
    UpdateType string    `json:"update_type"`
    Timestamp  int64     `json:"timestamp"`
    ChatID     int64     `json:"chat_id"`
    User       *user     `json:"user"`
    Message    *message  `json:"message"`
    Callback   *callback `json:"callback"`
    UserLocale string    `json:"user_locale"`
    Payload    string    `json:"payload"`
    IsChannel  bool      `json:"is_channel"`
}

// callback is a tapped callback button; Payload is what the button carried —
// here, the full id to look up.
type callback struct {
    CallbackID string `json:"callback_id"`
    Payload    string `json:"payload"`
    User       *user  `json:"user"`
}

type user struct {
    UserID           int64  `json:"user_id"`
    FirstName        string `json:"first_name"`
    LastName         string `json:"last_name"`
    Name             string `json:"name"`
    Username         string `json:"username"`
    IsBot            bool   `json:"is_bot"`
    LastActivityTime int64  `json:"last_activity_time"`
}

// name is what the user is called: their display name, else their first name,
// else their username, and "unknown" for no user at all — MAX may send an
// update with none.
func (u *user) name() string {
    if u == nil { return "unknown" }
    for _, n := range []string{u.Name, u.FirstName, u.Username} {
        if n != "" { return n }
    }
    return "unknown"
}

type message struct {
    Sender    *user     `json:"sender"`
    Recipient recipient `json:"recipient"`
    Timestamp int64     `json:"timestamp"`
    Body      body      `json:"body"`
}

type recipient struct {
    ChatID   int64  `json:"chat_id"`
    ChatType string `json:"chat_type"`
    UserID   int64  `json:"user_id"`
}

type body struct {
    Mid  string `json:"mid"`
    Seq  int64  `json:"seq"`
    Text string `json:"text"`
}

func newClient(token, baseURL string) *client {
    return &client{
        token:      token,
        baseURL:    strings.TrimRight(baseURL, "/"),
        httpClient: &http.Client{Timeout: 15 * time.Second},
    }
}

// call sends one request to the MAX Bot API and decodes the reply into out.
// The subscription body carries the secret, so it is never logged.
func (c *client) call(ctx context.Context, method, path string, payload, out any) error {
    var buf, err = json.Marshal(payload)
    if err != nil { return err }
    if path == "/subscriptions" {
        logging.Net("max → %s %s (body omitted: contains secret)", method, path)
    } else {
        logging.Net("max → %s %s %s", method, path, buf)
    }
    var req, reqErr = http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(buf))
    if reqErr != nil { return reqErr }
    req.Header.Set("Content-Type", "application/json")
    req.Header.Set("Authorization", c.token)
    var resp, doErr = c.httpClient.Do(req)
    if doErr != nil { return doErr }
    defer resp.Body.Close()
    var raw, readErr = io.ReadAll(resp.Body)
    if readErr != nil { return readErr }
    logging.Net("max ← %s %s %d %s", method, path, resp.StatusCode, raw)
    if resp.StatusCode != http.StatusOK { return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, raw) }
    if out == nil { return nil }
    return json.Unmarshal(raw, out)
}

// subscribe registers the webhook with POST /subscriptions. MAX answers 200
// either way and reports a refusal in the body, so success is read from there.
func (c *client) subscribe(ctx context.Context, webhookURL, secret string) error {
    var payload = map[string]any{"url": webhookURL, "update_types": updateTypes}
    if secret != "" { payload["secret"] = secret }
    var res struct {
        Success bool   `json:"success"`
        Message string `json:"message"`
    }
    if err := c.call(ctx, http.MethodPost, "/subscriptions", payload, &res); err != nil { return err }
    if !res.Success { return fmt.Errorf("subscription refused: %s", res.Message) }
    return nil
}

// send posts a message with its ids as an inline keyboard of callback buttons,
// two to a row; see Send.
func (c *client) send(ctx context.Context, chat int64, text string, ids []string) error {
    var path = "/messages?" + url.Values{"chat_id": {fmt.Sprint(chat)}}.Encode()
    var payload = map[string]any{"text": text, "format": "html", "disable_link_preview": true}
    var keyboard [][]map[string]string
    for i, id := range ids {
        var btn = map[string]string{"type": "callback", "text": short(id), "payload": id}
        if i%2 == 0 {
            keyboard = append(keyboard, []map[string]string{btn})
        } else {
            keyboard[len(keyboard)-1] = append(keyboard[len(keyboard)-1], btn)
        }
    }
    if len(keyboard) > 0 {
        payload["attachments"] = []any{map[string]any{"type": "inline_keyboard", "payload": map[string]any{"buttons": keyboard}}}
    }
    return c.call(ctx, http.MethodPost, path, payload, nil)
}

// short is an id as the bot's messages print it: the first and last six
// characters, for anything longer than fifteen.
func short(s string) string {
    if len(s) <= 15 { return s }
    return s[:6] + "..." + s[len(s)-6:]
}

// langOf reduces a MAX user_locale ("ru", "ru-RU", "ru_RU") to the language
// code the translation tables are keyed by.
func langOf(locale string) string {
    var lang, _, _ = strings.Cut(strings.ToLower(locale), "-")
    lang, _, _ = strings.Cut(lang, "_")
    return lang
}

// webhook receives MAX updates. When a secret is set every request must carry
// it in X-Max-Bot-Api-Secret, compared in constant time, since that is all that
// stops an arbitrary POST being taken for MAX. The update is handed to the
// handler for its type in opt. A message from a bot is dropped, so the bot
// never answers itself. Every update is answered 200 once decoded, whatever its
// handler did: MAX unsubscribes a bot whose webhook keeps failing.
func webhook(opt Options) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
            return
        }
        if opt.Secret != "" {
            var got = r.Header.Get("X-Max-Bot-Api-Secret")
            if subtle.ConstantTimeCompare([]byte(got), []byte(opt.Secret)) != 1 {
                http.Error(w, "unauthorized", http.StatusUnauthorized)
                return
            }
        }
        defer r.Body.Close()
        var raw, readErr = io.ReadAll(r.Body)
        if readErr != nil {
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        var u update
        if err := json.Unmarshal(raw, &u); err != nil {
            logging.Err("decode MAX update: %v", err)
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        var lang = langOf(u.UserLocale)
        switch u.UpdateType {
        case "message_created":
            if u.Message == nil || (u.Message.Sender != nil && u.Message.Sender.IsBot) || opt.Message == nil { break }
            opt.Message(Platform, u.Message.Recipient.ChatID, u.Message.Sender.name(), u.Message.Body.Text, lang)
        case "message_callback":
            if u.Message == nil || u.Callback == nil || u.Callback.Payload == "" || opt.Callback == nil { break }
            opt.Callback(Platform, u.Message.Recipient.ChatID, u.Callback.User.name(), u.Callback.Payload, lang)
        case "bot_started", "bot_added":
            if opt.Started == nil { break }
            opt.Started(Platform, u.ChatID, u.User.name(), lang)
        }
        w.WriteHeader(http.StatusOK)
    }
}

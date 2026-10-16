// Package telegram is everything the bot does on Telegram: the Bot API client,
// the webhook that receives updates, and the command menu.
//
// Start is the one way in. Running a command is package main's business, so
// the webhook hands each update to the handlers main passes in Options, and
// main replies through Send. The package holds the process's one Bot API
// connection in package variables, the way core holds its one node.
//
// The bot runs behind a self-hosted telegram-bot-api server on localhost, which
// owns the HTTPS conversation with Telegram, so the webhook server here is
// plain HTTP.
package telegram

import "bytes"
import "context"
import "crypto/subtle"
import "encoding/json"
import "errors"
import "fmt"
import "io"
import "net"
import "net/http"
import "sort"
import "strings"
import "sync"
import "time"
import "bitnsbot/logging"

// Platform is what a watch made on Telegram is filed under, and what keeps a
// Telegram chat apart from a MAX chat that happens to share its number.
const Platform = "tg"

// Command is one entry of the menu Telegram shows beside the input field.
type Command struct {
    Command     string `json:"command"`
    Description string `json:"description"`
}

// Options is what Start needs. The handlers take the platform first, so main
// hands Telegram and MAX the very same functions; name is who sent the update,
// for the log, and lang their client's language_code, empty when Telegram sent
// none.
type Options struct {
    // Token authenticates every Bot API call. It travels in the request URL,
    // so no URL is ever logged.
    Token string
    // APIBaseURL is the Bot API server called.
    APIBaseURL string
    // Listen is the address the webhook server binds to; empty starts none.
    Listen string
    // WebhookPath is the path the Bot API server POSTs updates to.
    WebhookPath string
    // WebhookURL is the URL registered with setWebhook.
    WebhookURL string
    // Secret, when set, is registered with the webhook and then checked
    // against X-Telegram-Bot-Api-Secret-Token on every update — the only
    // thing stopping an arbitrary POST being taken for Telegram.
    Secret string
    // Register calls setWebhook and publishes Menus. setMyCommands is global
    // to the bot token, so a second instance that must not touch the live
    // bot's Telegram-side state leaves both alone.
    Register bool
    // Menus is the command menu per language_code. Telegram falls back to the
    // list registered without one, so "" is the default.
    Menus map[string][]Command
    // Message is a message somebody typed: a command, or the argument a bare
    // command asked for.
    Message func(platform string, chat int64, name, text, lang string)
    // Callback is a tapped inline-keyboard button, data the full id it carried.
    Callback func(platform string, chat int64, name, data, lang string)
}

// mu guards token, baseURL and server: Start sets them while a notifier left
// over from an earlier start, in the tests, may still be sending.
var mu sync.Mutex

// token authenticates every Bot API call; empty until Start.
var token string

// baseURL is the Bot API server called.
var baseURL string

// server is the webhook server Start started, nil without one.
var server *http.Server

var httpClient = &http.Client{Timeout: 15 * time.Second}

// Start sets the token and the server every call goes to, registers the
// webhook and the menu when asked to, and serves the webhook on Listen. A menu that fails to
// register is logged and skipped — the bot works fine without one — and every
// other failure is returned. An empty WebhookURL would remove the webhook
// rather than set one, so registering without one is refused. The menu goes up
// one list per language, in a fixed order.
func Start(opt Options) error {
    mu.Lock()
    token, baseURL = opt.Token, strings.TrimRight(opt.APIBaseURL, "/")
    mu.Unlock()
    if opt.Register {
        if opt.WebhookURL == "" { return errors.New("registering the webhook needs its URL") }
        var payload = map[string]any{"url": opt.WebhookURL}
        if opt.Secret != "" { payload["secret_token"] = opt.Secret }
        var ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
        var _, err = call(ctx, "setWebhook", payload)
        cancel()
        if err != nil { return fmt.Errorf("set webhook: %w", err) }
        logging.Status("webhook registered at %s", opt.WebhookURL)
        var langs []string
        for lang := range opt.Menus { langs = append(langs, lang) }
        sort.Strings(langs)
        for _, lang := range langs {
            var menu = map[string]any{"commands": opt.Menus[lang]}
            if lang != "" { menu["language_code"] = lang }
            var mctx, mcancel = context.WithTimeout(context.Background(), 15*time.Second)
            var _, merr = call(mctx, "setMyCommands", menu)
            mcancel()
            if merr != nil {
                logging.Err("register command menu (language %q): %v", lang, merr)
                continue
            }
            logging.Status("command menu registered: %d commands, language %q", len(opt.Menus[lang]), lang)
        }
    }
    if opt.Listen == "" { return nil }
    var ln, err = net.Listen("tcp", opt.Listen)
    if err != nil { return err }
    var path = opt.WebhookPath
    if path == "" { path = "/" }
    var mux = http.NewServeMux()
    mux.HandleFunc(path, webhook(opt))
    var srv = &http.Server{Handler: mux}
    mu.Lock()
    server = srv
    mu.Unlock()
    go func() {
        logging.Status("listening on %s", opt.Listen)
        if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
            logging.Fatal("error listening: %v", err)
        }
    }()
    return nil
}

// Shutdown drains the webhook server, waiting for the handlers in flight, so a
// caller can close what those handlers use once it returns. Without a server
// it does nothing.
func Shutdown(ctx context.Context) error {
    mu.Lock()
    var srv = server
    server = nil
    mu.Unlock()
    if srv == nil { return nil }
    return srv.Shutdown(ctx)
}

// Send posts an HTML message to a chat, with one inline-keyboard button per id,
// two to a row, each labelled with the shortened id as the text shows it and
// carrying the full id as its callback_data.
//
// Buttons rather than links in the text, because Telegram does not parse
// entities inside <pre> — and the aligned field blocks every reply uses are
// <pre>. A button sits outside the text entirely, so the alignment survives.
// Telegram caps callback_data at 64 bytes, which a txid hits exactly, so the id
// travels bare: no type prefix, and a tap simply runs /info on it, which
// classifies it anyway. Choosing which ids get a button is the caller's.
func Send(ctx context.Context, chat int64, text string, ids []string) error {
    var payload = map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML",
        "link_preview_options": map[string]any{"is_disabled": true}}
    var rows [][]map[string]string
    for i, id := range ids {
        var btn = map[string]string{"text": short(id), "callback_data": id}
        if i%2 == 0 {
            rows = append(rows, []map[string]string{btn})
        } else {
            rows[len(rows)-1] = append(rows[len(rows)-1], btn)
        }
    }
    if len(rows) > 0 { payload["reply_markup"] = map[string]any{"inline_keyboard": rows} }
    var _, err = call(ctx, "sendMessage", payload)
    return err
}

// short is an id as the bot's messages print it: the first and last six
// characters, for anything longer than fifteen.
func short(s string) string {
    if len(s) <= 15 { return s }
    return s[:6] + "..." + s[len(s)-6:]
}

// call sends one Bot API request, and fails before Start has set a token. The
// token is in the URL, so the URL is never logged, and the setWebhook body
// carries the secret, so it is left out too.
func call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
    mu.Lock()
    var url = fmt.Sprintf("%s/bot%s/%s", baseURL, token, method)
    var started = token != ""
    mu.Unlock()
    if !started { return nil, errors.New("telegram is not started") }
    var buf, err = json.Marshal(payload)
    if err != nil { return nil, err }
    if method == "setWebhook" {
        logging.Net("telegram → %s (body omitted: contains secret_token)", method)
    } else {
        logging.Net("telegram → %s %s", method, buf)
    }
    var req, reqErr = http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
    if reqErr != nil { return nil, reqErr }
    req.Header.Set("Content-Type", "application/json")
    var resp, doErr = httpClient.Do(req)
    if doErr != nil { return nil, doErr }
    defer resp.Body.Close()
    var body, readErr = io.ReadAll(resp.Body)
    if readErr != nil { return nil, readErr }
    logging.Net("telegram ← %s %s", method, body)
    var apiResp struct {
        OK          bool            `json:"ok"`
        Description string          `json:"description"`
        ErrorCode   int             `json:"error_code"`
        Result      json.RawMessage `json:"result"`
    }
    if err := json.Unmarshal(body, &apiResp); err != nil {
        return nil, fmt.Errorf("decode response: %w", err)
    }
    if !apiResp.OK {
        return nil, fmt.Errorf("telegram api error %d: %s", apiResp.ErrorCode, apiResp.Description)
    }
    return apiResp.Result, nil
}

type update struct {
    UpdateID      int64          `json:"update_id"`
    Message       *message       `json:"message"`
    CallbackQuery *callbackQuery `json:"callback_query"`
}

// callbackQuery arrives when a user taps an inline-keyboard button. Data carries
// whatever was put in the button's callback_data — here, the full id to look up.
type callbackQuery struct {
    ID      string   `json:"id"`
    From    *user    `json:"from"`
    Message *message `json:"message"`
    Data    string   `json:"data"`
}

type message struct {
    MessageID int64  `json:"message_id"`
    From      *user  `json:"from"`
    Chat      chat   `json:"chat"`
    Date      int64  `json:"date"`
    Text      string `json:"text"`
}

type user struct {
    ID           int64  `json:"id"`
    IsBot        bool   `json:"is_bot"`
    FirstName    string `json:"first_name"`
    Username     string `json:"username"`
    LanguageCode string `json:"language_code"`
}

// name is what the user is called in the log: their username, else their first
// name, else "unknown" — which is also what an update with no user is.
func (u *user) name() string {
    if u == nil { return "unknown" }
    if u.Username != "" { return u.Username }
    if u.FirstName != "" { return u.FirstName }
    return "unknown"
}

// lang is the user's client language, empty for an update with no user.
func (u *user) lang() string {
    if u == nil { return "" }
    return u.LanguageCode
}

type chat struct {
    ID   int64  `json:"id"`
    Type string `json:"type"`
}

// webhook receives Telegram updates. When a secret is set, every request must
// carry it, compared in constant time. A tapped button is acknowledged before
// anything else — Telegram leaves it spinning until the query is answered — and
// then handed to Callback; a message is handed to Message.
func webhook(opt Options) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
            return
        }
        if opt.Secret != "" {
            var got = r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
            if subtle.ConstantTimeCompare([]byte(got), []byte(opt.Secret)) != 1 {
                http.Error(w, "unauthorized", http.StatusUnauthorized)
                return
            }
        }
        defer r.Body.Close()
        var u update
        if err := json.NewDecoder(r.Body).Decode(&u); err != nil {
            logging.Err("decode update: %v", err)
            http.Error(w, "bad request", http.StatusBadRequest)
            return
        }
        if q := u.CallbackQuery; q != nil {
            var ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
            if _, err := call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": q.ID}); err != nil {
                logging.Warn("answer callback: %v", err)
            }
            cancel()
            if q.Message != nil && q.Data != "" && opt.Callback != nil {
                opt.Callback(Platform, q.Message.Chat.ID, q.From.name(), q.Data, q.From.lang())
            }
        } else if m := u.Message; m != nil && opt.Message != nil {
            opt.Message(Platform, m.Chat.ID, m.From.name(), m.Text, m.From.lang())
        }
        w.WriteHeader(http.StatusOK)
    }
}

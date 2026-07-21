package main

import "bytes"
import "context"
import "encoding/json"
import "fmt"
import "io"
import "net/http"
import "strings"
import "time"
import "bitnsbot/logging"

type bot struct {
    token      string
    baseURL    string
    httpClient *http.Client
}

func newBot(token, baseURL string) *bot {
    return &bot{
        token:      token,
        baseURL:    strings.TrimRight(baseURL, "/"),
        httpClient: &http.Client{Timeout: 15 * time.Second},
    }
}

type Update struct {
    UpdateID      int64          `json:"update_id"`
    Message       *Message       `json:"message"`
    CallbackQuery *CallbackQuery `json:"callback_query"`
}

// CallbackQuery arrives when a user taps an inline-keyboard button. Data carries
// whatever was put in the button's callback_data — here, the full id to look up.
type CallbackQuery struct {
    ID      string   `json:"id"`
    From    *User    `json:"from"`
    Message *Message `json:"message"`
    Data    string   `json:"data"`
}

type Message struct {
    MessageID int64  `json:"message_id"`
    From      *User  `json:"from"`
    Chat      Chat   `json:"chat"`
    Date      int64  `json:"date"`
    Text      string `json:"text"`
}

type User struct {
    ID        int64  `json:"id"`
    IsBot     bool   `json:"is_bot"`
    FirstName string `json:"first_name"`
    Username  string `json:"username"`
}

type Chat struct {
    ID   int64  `json:"id"`
    Type string `json:"type"`
}

func (b *bot) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
    var buf, err = json.Marshal(payload)
    if err != nil { return nil, err }
    if method == "setWebhook" {
        logging.Net("telegram → %s (body omitted: contains secret_token)", method)
    } else {
        logging.Net("telegram → %s %s", method, buf)
    }
    // the token is in the URL, so it is deliberately never logged
    var url = fmt.Sprintf("%s/bot%s/%s", b.baseURL, b.token, method)
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
    if err != nil { return nil, err }
    req.Header.Set("Content-Type", "application/json")
    resp, err := b.httpClient.Do(req)
    if err != nil { return nil, err }
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

func (b *bot) send(ctx context.Context, chatID int64, text string) error {
    return b.sendWithButtons(ctx, chatID, text, nil)
}

// sendWithButtons sends a message with one inline-keyboard button per id, each
// labelled with the shortened id as it appears in the text and carrying the full
// id as its callback_data.
//
// Buttons rather than links in the text, because Telegram does not parse
// entities inside <pre> — and the aligned field blocks every reply uses are
// <pre>. A button sits outside the text entirely, so the alignment survives.
// Telegram caps callback_data at 64 bytes, which a txid hits exactly, so the id
// travels bare: no type prefix, and the handler simply passes it to /info, which
// classifies it anyway.
func (b *bot) sendWithButtons(ctx context.Context, chatID int64, text string, ids []string) error {
    var payload = map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML"}
    if rows := buttonRows(ids); len(rows) > 0 {
        payload["reply_markup"] = map[string]any{"inline_keyboard": rows}
    }
    var _, err = b.call(ctx, "sendMessage", payload)
    return err
}

// maxButtons bounds how many buttons one message carries, so an /info on a
// transaction with many addresses doesn't bury the message under a keyboard.
const maxButtons = 8

// buttonRows turns ids into rows of two buttons, dropping duplicates, empties,
// the "(non-standard)" placeholder outputs use, and anything too long for
// callback_data.
func buttonRows(ids []string) [][]map[string]string {
    var seen = map[string]bool{}
    var buttons []map[string]string
    for _, id := range ids {
        if id == "" || id == "(non-standard)" || seen[id] || len(id) > 64 { continue }
        seen[id] = true
        buttons = append(buttons, map[string]string{"text": short(id), "callback_data": id})
        if len(buttons) == maxButtons { break }
    }
    var rows [][]map[string]string
    for i := 0; i < len(buttons); i += 2 {
        var end = i + 2
        if end > len(buttons) { end = len(buttons) }
        rows = append(rows, buttons[i:end])
    }
    return rows
}

// answerCallback acknowledges a tapped button. Telegram shows a loading state on
// the button until this is called, so it must happen even when the lookup fails.
func (b *bot) answerCallback(ctx context.Context, id string) error {
    var _, err = b.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id})
    return err
}

func (b *bot) setWebhook(ctx context.Context, url, token string) error {
    var payload = map[string]any{"url": url}
    if token != "" { payload["secret_token"] = token }
    var _, err = b.call(ctx, "setWebhook", payload)
    return err
}

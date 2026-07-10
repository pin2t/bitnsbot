package main

import "bytes"
import "context"
import "encoding/json"
import "fmt"
import "io"
import "net/http"
import "strings"
import "time"

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
    UpdateID int64    `json:"update_id"`
    Message  *Message `json:"message"`
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
        logNet("telegram → %s (body omitted: contains secret_token)", method)
    } else {
        logNet("telegram → %s %s", method, buf)
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
    logNet("telegram ← %s %s", method, body)
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
    var _, err = b.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML"})
    return err
}

func (b *bot) setWebhook(ctx context.Context, url, token string) error {
    var payload = map[string]any{"url": url}
    if token != "" { payload["secret_token"] = token }
    var _, err = b.call(ctx, "setWebhook", payload)
    return err
}

package main

import "bytes"
import "context"
import "encoding/json"
import "fmt"
import "net/http"
import "strings"
import "time"

// Bot is a minimal Telegram Bot API client built on net/http and encoding/json.
// It talks to whatever Bot API server is at baseURL, which lets it be pointed
// at a local telegram-bot-api instance instead of https://api.telegram.org.
type Bot struct {
	token      string
	baseURL    string
	httpClient *http.Client
}

func NewBot(token, baseURL string) *Bot {
	return &Bot{
		token:      token,
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Update represents an incoming update delivered to the webhook.
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

// apiResponse mirrors the envelope every Telegram Bot API call returns.
type apiResponse struct {
	OK          bool            `json:"ok"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Result      json.RawMessage `json:"result"`
}

func (b *Bot) call(ctx context.Context, method string, payload any) (json.RawMessage, error) {
	var buf, err = json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var url = fmt.Sprintf("%s/bot%s/%s", b.baseURL, b.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if !apiResp.OK {
		return nil, fmt.Errorf("telegram api error %d: %s", apiResp.ErrorCode, apiResp.Description)
	}
	return apiResp.Result, nil
}

// SendMessage sends a text message to the given chat.
func (b *Bot) SendMessage(ctx context.Context, chatID int64, text string) error {
	var _, err = b.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
	return err
}

// SetWebhookOptions configures the setWebhook call.
type SetWebhookOptions struct {
	URL         string
	SecretToken string // optional: echoed back in the X-Telegram-Bot-Api-Secret-Token header
}

// SetWebhook registers the URL the Bot API server will POST updates to.
func (b *Bot) SetWebhook(ctx context.Context, opts SetWebhookOptions) error {
	var payload = map[string]any{"url": opts.URL}
	if opts.SecretToken != "" {
		payload["secret_token"] = opts.SecretToken
	}
	var _, err = b.call(ctx, "setWebhook", payload)
	return err
}

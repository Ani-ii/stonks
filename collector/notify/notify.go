// Package notify sends messages. Telegram is the real one; Printer writes to a stream so the whole
// pipeline can be tried before a bot token exists.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type Telegram struct {
	Token   string
	ChatID  string
	BaseURL string // default https://api.telegram.org
	HTTP    *http.Client
}

func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{Token: token, ChatID: chatID, BaseURL: "https://api.telegram.org", HTTP: &http.Client{Timeout: 20 * time.Second}}
}

var chatIDPattern = regexp.MustCompile(`^(-?\d+|@\w+)$`)

// Check catches the two most common setup mistakes before any message is sent.
func (t *Telegram) Check() error {
	if !strings.Contains(t.Token, ":") {
		return errors.New("TELEGRAM_BOT_TOKEN does not look like a bot token (expected digits, a colon, then letters and digits)")
	}
	if !chatIDPattern.MatchString(t.ChatID) {
		return errors.New("TELEGRAM_CHAT_ID must be the numeric chat id, not a username: send /start to your bot, open https://api.telegram.org/bot<TOKEN>/getUpdates and copy chat.id")
	}
	return nil
}

// RateLimitError means Telegram answered 429.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return fmt.Sprintf("telegram rate limited (retry after %v)", e.RetryAfter) }

func (t *Telegram) redact(s string) string { return strings.ReplaceAll(s, t.Token, "***") }

// Send posts a plain-text message and returns its message id. The bot token is part of the URL, so any
// error text is scrubbed of it before it can reach a log.
func (t *Telegram) Send(ctx context.Context, text string) (int64, error) {
	if r := []rune(text); len(r) > 4000 {
		text = string(r[:4000]) + "..."
	}
	body, _ := json.Marshal(map[string]any{"chat_id": t.ChatID, "text": text, "disable_web_page_preview": true})
	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/bot%s/sendMessage", strings.TrimRight(t.BaseURL, "/"), t.Token), bytes.NewReader(body))
	if err != nil {
		return 0, errors.New(t.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return 0, errors.New(t.redact(err.Error()))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	_ = json.Unmarshal(raw, &r)
	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, &RateLimitError{RetryAfter: time.Duration(r.Parameters.RetryAfter) * time.Second}
	}
	if !r.OK {
		return 0, fmt.Errorf("telegram http %d: %s", resp.StatusCode, t.redact(r.Description))
	}
	return r.Result.MessageID, nil
}

// Printer writes messages to a stream instead of sending them.
type Printer struct{ W io.Writer }

func (p Printer) Send(_ context.Context, text string) (int64, error) {
	_, err := fmt.Fprintf(p.W, "---- would send ----\n%s\n--------------------\n", text)
	return 0, err
}

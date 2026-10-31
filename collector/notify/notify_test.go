package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSend(t *testing.T) {
	var path string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"ok":true,"result":{"message_id":77}}`))
	}))
	defer srv.Close()
	tg := NewTelegram("123:SECRET", "555")
	tg.BaseURL = srv.URL
	id, err := tg.Send(context.Background(), "hello")
	if err != nil || id != 77 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if path != "/bot123:SECRET/sendMessage" || got["chat_id"] != "555" || got["text"] != "hello" {
		t.Errorf("request wrong: path=%q body=%v", path, got)
	}
}

func TestLongTextIsTruncated(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer srv.Close()
	tg := NewTelegram("1:x", "1")
	tg.BaseURL = srv.URL
	tg.Send(context.Background(), strings.Repeat("é", 6000))
	if n := len([]rune(got["text"].(string))); n > 4010 {
		t.Fatalf("text not truncated: %d runes", n)
	}
}

func TestRateLimitAndAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("x") == "" && strings.Contains(r.URL.Path, "429") {
			w.WriteHeader(429)
			w.Write([]byte(`{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":9}}`))
			return
		}
		w.WriteHeader(400)
		w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()
	tg := NewTelegram("429:tok", "1")
	tg.BaseURL = srv.URL
	_, err := tg.Send(context.Background(), "x")
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 9*time.Second {
		t.Fatalf("want RateLimitError{9s}, got %v", err)
	}
	tg2 := NewTelegram("1:tok", "1")
	tg2.BaseURL = srv.URL
	if _, err := tg2.Send(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("API error not surfaced: %v", err)
	}
}

func TestTokenNeverAppearsInErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening: the HTTP client error would normally contain the full URL
	tg := NewTelegram("999:VERYSECRET", "1")
	tg.BaseURL = url
	_, err := tg.Send(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "VERYSECRET") {
		t.Fatalf("error must exist and must not contain the token: %v", err)
	}
}

func TestCheck(t *testing.T) {
	if err := NewTelegram("123:abc", "-1001234").Check(); err != nil {
		t.Errorf("valid group id rejected: %v", err)
	}
	if err := NewTelegram("123:abc", "4242").Check(); err != nil {
		t.Errorf("valid id rejected: %v", err)
	}
	if err := NewTelegram("123:abc", "SomeUserName").Check(); err == nil || !strings.Contains(err.Error(), "numeric") {
		t.Errorf("a username must be rejected with advice: %v", err)
	}
	if err := NewTelegram("nocolon", "4242").Check(); err == nil {
		t.Error("token without a colon must be rejected")
	}
}

func TestPrinter(t *testing.T) {
	var b bytes.Buffer
	if _, err := (Printer{W: &b}).Send(context.Background(), "msg"); err != nil || !strings.Contains(b.String(), "msg") {
		t.Fatalf("printer: %q %v", b.String(), err)
	}
}

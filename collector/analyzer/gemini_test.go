package analyzer

import (
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

const okBody = `{"candidates":[{"content":{"parts":[{"text":"{\"direction\":\"up\",\"confidence\":0.7,\"materiality\":\"high\",\"reason\":\"Large order win.\"}"}]},"finishReason":"STOP"}],
"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":40,"thoughtsTokenCount":10}}`

func newTestGemini(url string) *Gemini {
	g := NewGemini("test-key", "gemini-test")
	g.BaseURL = url
	g.MinInterval = 0
	return g
}

func TestGeminiRequestAndParse(t *testing.T) {
	var gotKeyHeader, gotPath, gotQuery string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKeyHeader, gotPath, gotQuery = r.Header.Get("x-goog-api-key"), r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Write([]byte(okBody))
	}))
	defer srv.Close()
	sig, err := newTestGemini(srv.URL).Analyze(context.Background(), sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	if sig.Direction != "up" || sig.Confidence != 0.7 || sig.Materiality != "high" || sig.Model != "gemini:gemini-test" {
		t.Errorf("bad signal: %+v", sig)
	}
	if sig.InputTokens != 300 || sig.OutputTokens != 50 {
		t.Errorf("token counts: in=%d out=%d (want 300, 50 including thought tokens)", sig.InputTokens, sig.OutputTokens)
	}
	if gotKeyHeader != "test-key" || gotQuery != "" {
		t.Errorf("key must be sent in the header only: header=%q query=%q", gotKeyHeader, gotQuery)
	}
	if gotPath != "/v1beta/models/gemini-test:generateContent" {
		t.Errorf("path %q", gotPath)
	}
	gc, _ := gotBody["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" || gc["responseSchema"] == nil {
		t.Errorf("generationConfig lacks JSON schema: %v", gc)
	}
	if gotBody["systemInstruction"] == nil {
		t.Error("systemInstruction missing")
	}
	if !strings.Contains(string(mustJSON(gotBody["contents"])), "Transworld Shipping Lines Ltd") {
		t.Error("filing text not in request")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestGeminiRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	_, err := newTestGemini(srv.URL).Analyze(context.Background(), sampleInput())
	var rl *ErrRateLimited
	if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second {
		t.Fatalf("want ErrRateLimited{7s}, got %v", err)
	}
}

func TestGeminiBadAnswers(t *testing.T) {
	for name, body := range map[string]string{
		"not json":  `{"candidates":[{"content":{"parts":[{"text":"sure, up!"}]}}]}`,
		"bad enum":  `{"candidates":[{"content":{"parts":[{"text":"{\"direction\":\"moon\",\"confidence\":0.5,\"materiality\":\"low\",\"reason\":\"r\"}"}]}}]}`,
		"no cands":  `{"candidates":[]}`,
		"garbage":   `<html>`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		if _, err := newTestGemini(srv.URL).Analyze(context.Background(), sampleInput()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		srv.Close()
	}
}

func TestGeminiHTTPErrorDoesNotLeakKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"message":"permission denied"}}`))
	}))
	defer srv.Close()
	_, err := newTestGemini(srv.URL).Analyze(context.Background(), sampleInput())
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "test-key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGeminiMinInterval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(okBody)) }))
	defer srv.Close()
	g := newTestGemini(srv.URL)
	g.MinInterval = 80 * time.Millisecond
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := g.Analyze(context.Background(), sampleInput()); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("3 calls took %v, want at least 160ms of spacing", d)
	}
}

func TestGeminiServerErrorsAndDroppedConnectionsAreTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	_, err := newTestGemini(srv.URL).Analyze(context.Background(), sampleInput())
	srv.Close()
	var tr *ErrTransient
	if !errors.As(err, &tr) {
		t.Fatalf("503 should be transient, got %v", err)
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer srv.Close()
	if _, err := newTestGemini(srv.URL).Analyze(context.Background(), sampleInput()); !errors.As(err, &tr) {
		t.Fatalf("dropped connection should be transient, got %v", err)
	}
	// A 403 or a bad answer is permanent.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv2.Close()
	if _, err := newTestGemini(srv2.URL).Analyze(context.Background(), sampleInput()); errors.As(err, &tr) {
		t.Fatalf("403 must not be transient: %v", err)
	}
}

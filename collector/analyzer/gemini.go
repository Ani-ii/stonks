package analyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrRateLimited means the provider answered 429; the caller should wait RetryAfter (if known) and retry later.
type ErrRateLimited struct{ RetryAfter time.Duration }

func (e *ErrRateLimited) Error() string {
	return fmt.Sprintf("rate limited (retry after %v)", e.RetryAfter)
}

// Gemini calls the Gemini Developer API (generateContent) with a JSON response schema.
// The API key goes in a header, never in the URL, so it cannot end up in logs.
type Gemini struct {
	APIKey      string
	Model       string        // pin an exact model id, not a "-latest" alias, so results stay comparable
	BaseURL     string        // default https://generativelanguage.googleapis.com
	HTTP        *http.Client
	MinInterval time.Duration // minimum gap between requests (the free tier allows roughly 15 per minute)
	MaxChars    int

	mu   sync.Mutex
	last time.Time
}

func NewGemini(apiKey, model string) *Gemini {
	return &Gemini{
		APIKey: apiKey, Model: model,
		BaseURL:     "https://generativelanguage.googleapis.com",
		HTTP:        &http.Client{Timeout: 60 * time.Second},
		MinInterval: 5 * time.Second,
		MaxChars:    DefaultMaxChars,
	}
}

func (g *Gemini) Name() string { return "gemini:" + g.Model }

func (g *Gemini) wait(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := time.Until(g.last.Add(g.MinInterval)); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	g.last = time.Now()
	return nil
}

var responseSchema = map[string]any{
	"type": "OBJECT",
	"properties": map[string]any{
		"direction":   map[string]any{"type": "STRING", "enum": []string{"up", "down", "neutral"}},
		"confidence":  map[string]any{"type": "NUMBER"},
		"materiality": map[string]any{"type": "STRING", "enum": []string{"high", "medium", "low"}},
		"reason":      map[string]any{"type": "STRING"},
	},
	"required": []string{"direction", "confidence", "materiality", "reason"},
}

func (g *Gemini) Analyze(ctx context.Context, in Input) (Signal, error) {
	if err := g.wait(ctx); err != nil {
		return Signal{}, err
	}
	reqBody, err := json.Marshal(map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]any{{"text": SystemPrompt}}},
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": BuildPrompt(in, g.MaxChars)}},
		}},
		"generationConfig": map[string]any{
			"temperature":      0,
			"responseMimeType": "application/json",
			"responseSchema":   responseSchema,
			"maxOutputTokens":  1024,
		},
	})
	if err != nil {
		return Signal{}, err
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", strings.TrimRight(g.BaseURL, "/"), g.Model)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return Signal{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.APIKey)
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return Signal{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Signal{}, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return Signal{}, &ErrRateLimited{RetryAfter: time.Duration(secs) * time.Second}
	}
	if resp.StatusCode != http.StatusOK {
		s := strings.Join(strings.Fields(string(body)), " ")
		if len(s) > 200 {
			s = s[:200]
		}
		return Signal{}, fmt.Errorf("gemini http %d: %s", resp.StatusCode, s)
	}
	return parseGemini(body, g.Name())
}

func parseGemini(body []byte, model string) (Signal, error) {
	var r struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Usage struct {
			Prompt     int `json:"promptTokenCount"`
			Candidates int `json:"candidatesTokenCount"`
			Thoughts   int `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return Signal{}, fmt.Errorf("gemini: unreadable response: %w", err)
	}
	if len(r.Candidates) == 0 {
		return Signal{}, errors.New("gemini: no candidates in response (possibly blocked)")
	}
	var text strings.Builder
	for _, p := range r.Candidates[0].Content.Parts {
		if !p.Thought {
			text.WriteString(p.Text)
		}
	}
	var raw struct {
		Direction   string  `json:"direction"`
		Confidence  float64 `json:"confidence"`
		Materiality string  `json:"materiality"`
		Reason      string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(text.String()), &raw); err != nil {
		return Signal{}, fmt.Errorf("gemini: answer is not the requested JSON (finish=%s): %w", r.Candidates[0].FinishReason, err)
	}
	return Validate(Signal{
		Direction: raw.Direction, Confidence: raw.Confidence, Materiality: raw.Materiality, Reason: raw.Reason,
		Model: model, InputTokens: r.Usage.Prompt, OutputTokens: r.Usage.Candidates + r.Usage.Thoughts,
	})
}

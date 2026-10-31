// Package runner is the M3 pipeline: for every new filing it records the filter's decision, analyses the
// ANALYZE-class ones with the model, stores the signal, and optionally sends an alert. It runs as its own
// program, so a slow model or a Telegram problem can never stall the collector.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"stonks/analyzer"
	"stonks/announce"
	"stonks/attach"
	"stonks/filter"
	"stonks/match"
	"stonks/store"
)

type Store interface {
	Untriaged(ctx context.Context, sinceMs int64, limit int) ([]store.Row, error)
	SetTriage(ctx context.Context, id int64, decision, reason string, now time.Time) error
	PendingAnalysis(ctx context.Context, model string, sinceMs int64, limit int) ([]store.Row, error)
	RecentSignals(ctx context.Context, sinceMs int64) ([]store.SignalRef, error)
	SaveSignal(ctx context.Context, rec store.SignalRecord) (int64, error)
	CountSignalsSince(ctx context.Context, sinceMs int64, model string) (int, error)
	MarkNotified(ctx context.Context, signalID int64, now time.Time) error
}

type Notifier interface {
	Send(ctx context.Context, text string) (int64, error)
}

type Config struct {
	Since           time.Duration // only filings published within this window are considered (default 6h)
	Batch           int           // filings analysed per pass (default 20)
	DailyCap        int           // model calls per rolling 24 h; 0 means no cap (default 450 of the free tier's 500)
	EventWindow     time.Duration // same-company window across exchanges (default 15 min)
	DocWindow       time.Duration // how far back to look for identical documents or events (default 72 h)
	MinMateriality  string        // alerts need at least this materiality: "high" or "medium" (default medium)
	MinConfidence   float64       // alerts need at least this confidence (default 0.5)
	MaxTextAttempts int           // attachment download attempts before falling back to metadata only (default 3)
}

func (c Config) withDefaults() Config {
	if c.Since <= 0 {
		c.Since = 6 * time.Hour
	}
	if c.Batch <= 0 {
		c.Batch = 20
	}
	if c.EventWindow <= 0 {
		c.EventWindow = 15 * time.Minute
	}
	if c.DocWindow <= 0 {
		c.DocWindow = 72 * time.Hour
	}
	if c.MinMateriality == "" {
		c.MinMateriality = "medium"
	}
	if c.MinConfidence <= 0 {
		c.MinConfidence = 0.5
	}
	if c.MaxTextAttempts <= 0 {
		c.MaxTextAttempts = 3
	}
	return c
}

type Runner struct {
	S    Store
	A    analyzer.Analyzer
	Text func(ctx context.Context, a announce.Announcement) (string, error) // nil = metadata only
	Link func(a announce.Announcement) string                              // attachment link for alerts, may be nil
	N    Notifier                                                          // nil = no alerts
	Cfg  Config
	Now  func() time.Time
	Log  *log.Logger

	attempts  map[int64]int
	capLogged bool
}

func New(s Store, a analyzer.Analyzer, cfg Config, l *log.Logger) *Runner {
	return &Runner{S: s, A: a, Cfg: cfg.withDefaults(), Now: time.Now, Log: l, attempts: map[int64]int{}}
}

// Run repeats Step until ctx is cancelled, waiting at least interval between passes.
func (r *Runner) Run(ctx context.Context, interval time.Duration) {
	for {
		pause := r.Step(ctx)
		wait := interval
		if pause > wait {
			wait = pause
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Step runs one pass: triage new filings, then analyse up to Batch pending ones. It returns how long the
// caller should wait before the next pass when the model or a store asked for a pause (0 = no request).
func (r *Runner) Step(ctx context.Context) time.Duration {
	cfg := r.Cfg
	now := r.Now()
	since := now.Add(-cfg.Since).UnixMilli()

	rows, err := r.S.Untriaged(ctx, since, 500)
	if err != nil {
		r.Log.Printf("ERROR reading untriaged filings: %v", err)
		return 30 * time.Second
	}
	for _, row := range rows {
		d := filter.Classify(row.A)
		if err := r.S.SetTriage(ctx, row.ID, d.Decision.String(), d.Reason, now); err != nil {
			r.Log.Printf("ERROR saving triage: %v", err)
			return 30 * time.Second
		}
	}

	pending, err := r.S.PendingAnalysis(ctx, r.A.Name(), since, cfg.Batch)
	if err != nil {
		r.Log.Printf("ERROR reading pending filings: %v", err)
		return 30 * time.Second
	}
	for _, row := range pending {
		if ctx.Err() != nil {
			return 0
		}
		if stop, pause := r.process(ctx, row); stop {
			return pause
		}
	}
	return 0
}

// process handles one filing. stop=true asks the caller to end the pass and wait for pause.
func (r *Runner) process(ctx context.Context, row store.Row) (stop bool, pause time.Duration) {
	cfg := r.Cfg
	now := r.Now()

	if cfg.DailyCap > 0 {
		n, err := r.S.CountSignalsSince(ctx, now.Add(-24*time.Hour).UnixMilli(), r.A.Name())
		if err != nil {
			r.Log.Printf("ERROR counting signals: %v", err)
			return true, 30 * time.Second
		}
		if n >= cfg.DailyCap {
			if !r.capLogged {
				r.Log.Printf("WARN daily cap reached (%d model calls in 24h); pausing analysis", n)
				r.capLogged = true
			}
			return true, 5 * time.Minute
		}
		r.capLogged = false
	}

	recent, err := r.S.RecentSignals(ctx, now.Add(-cfg.DocWindow).UnixMilli())
	if err != nil {
		r.Log.Printf("ERROR reading recent signals: %v", err)
		return true, 30 * time.Second
	}
	for _, ref := range recent {
		if ref.AnnouncementID != row.ID && (match.SameEvent(ref.A, row.A, cfg.EventWindow) || match.SameAttachment(ref.A, row.A)) {
			r.duplicate(ctx, row, fmt.Sprintf("same event as #%d", ref.AnnouncementID))
			return false, 0
		}
	}

	text := ""
	if r.Text != nil {
		t, err := r.Text(ctx, row.A)
		switch {
		case err == nil:
			text = t
		case errors.Is(err, attach.ErrNoAttachment), errors.Is(err, attach.ErrNotPDF), errors.Is(err, attach.ErrNoText), errors.Is(err, attach.ErrTooLarge):
			// permanent: analyse from the metadata alone
		default:
			r.attempts[row.ID]++
			if r.attempts[row.ID] < cfg.MaxTextAttempts {
				r.Log.Printf("WARN attachment for #%d not available yet (%v); will retry", row.ID, err)
				return false, 0
			}
			r.Log.Printf("WARN giving up on the attachment for #%d (%v); analysing metadata only", row.ID, err)
		}
	}
	hash := ""
	if text != "" {
		hash = attach.TextHash(text)
		for _, ref := range recent {
			if ref.AnnouncementID != row.ID && ref.TextHash == hash {
				r.duplicate(ctx, row, fmt.Sprintf("same document as #%d", ref.AnnouncementID))
				return false, 0
			}
		}
	}

	sig, err := r.A.Analyze(ctx, analyzer.Input{Filing: row.A, Text: text})
	var rl *analyzer.ErrRateLimited
	var tr *analyzer.ErrTransient
	switch {
	case ctx.Err() != nil:
		return true, 0
	case errors.As(err, &rl):
		wait := rl.RetryAfter
		if wait <= 0 {
			wait = time.Minute
		}
		r.Log.Printf("WARN model rate limit; pausing %v", wait)
		return true, wait
	case errors.As(err, &tr):
		r.Log.Printf("WARN model temporarily unavailable (%v); pausing 30s", err)
		return true, 30 * time.Second
	case err != nil:
		r.Log.Printf("ERROR analysing #%d (%s): %v", row.ID, row.A.Company, err)
		if e := r.S.SetTriage(ctx, row.ID, "ERROR", truncate(err.Error(), 200), now); e != nil {
			r.Log.Printf("ERROR saving triage: %v", e)
		}
		return false, 0
	}

	created := r.Now()
	id, err := r.S.SaveSignal(ctx, store.SignalRecord{AnnouncementID: row.ID, Sig: sig, TextHash: hash, TextChars: len(text), Created: created})
	if err != nil {
		r.Log.Printf("ERROR saving signal: %v", err)
		return true, 30 * time.Second
	}
	r.Log.Printf("SIGNAL %s %s | %s | %s %.2f %s (text %d chars)", row.A.Source, row.A.Company,
		strings.TrimSpace(row.A.Category+" / "+row.A.Subcategory), strings.ToUpper(sig.Direction), sig.Confidence, sig.Materiality, len(text))

	if r.N != nil && id != 0 && r.shouldAlert(sig) {
		if _, err := r.N.Send(ctx, r.FormatMessage(row, sig)); err != nil {
			r.Log.Printf("ERROR sending alert for #%d: %v", row.ID, err)
		} else if err := r.S.MarkNotified(ctx, id, r.Now()); err != nil {
			r.Log.Printf("ERROR marking alert as sent: %v", err)
		}
	}
	return false, 0
}

func (r *Runner) duplicate(ctx context.Context, row store.Row, why string) {
	if err := r.S.SetTriage(ctx, row.ID, "DUPLICATE", why, r.Now()); err != nil {
		r.Log.Printf("ERROR saving triage: %v", err)
		return
	}
	r.Log.Printf("DUPLICATE %s %s (%s)", row.A.Source, row.A.Company, why)
}

var materialityRank = map[string]int{"low": 0, "medium": 1, "high": 2}

// shouldAlert: neutral signals are stored but never alerted; the rest need enough materiality and confidence.
func (r *Runner) shouldAlert(s analyzer.Signal) bool {
	return s.Direction != "neutral" &&
		materialityRank[s.Materiality] >= materialityRank[r.Cfg.MinMateriality] &&
		s.Confidence >= r.Cfg.MinConfidence
}

// FormatMessage builds the alert text. Times are IST, as published by the exchanges.
func (r *Runner) FormatMessage(row store.Row, s analyzer.Signal) string {
	a := row.A
	head := map[string]string{"up": "▲ UP", "down": "▼ DOWN", "neutral": "● NEUTRAL"}[s.Direction]
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s (%s)\n", head, a.Company, a.Source)
	if c := strings.Trim(strings.TrimSpace(a.Category+" / "+a.Subcategory), "/ "); c != "" {
		fmt.Fprintf(&b, "%s\n", c)
	}
	fmt.Fprintf(&b, "Published %s IST", a.DisseminatedAt.In(announce.IST).Format("15:04:05"))
	if !row.Backlog && !row.FirstSeen.IsZero() {
		fmt.Fprintf(&b, ", detected %ds later", int(row.FirstSeen.Sub(a.DisseminatedAt).Seconds()))
	}
	fmt.Fprintf(&b, "\nConfidence %.2f · materiality %s · %s\n%s\n", s.Confidence, s.Materiality, s.Model, s.Reason)
	if r.Link != nil {
		if u := r.Link(a); u != "" {
			fmt.Fprintf(&b, "%s\n", u)
		}
	}
	b.WriteString("Experimental signal, not investment advice.")
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

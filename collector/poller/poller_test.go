package poller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"stonks/announce"
)

type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]announce.Announcement
	backlog  map[string]bool
	firstSee map[string]time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]announce.Announcement{}, backlog: map[string]bool{}, firstSee: map[string]time.Time{}}
}
func (s *fakeStore) Seen(_ context.Context, src string, ids []string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := map[string]bool{}
	for _, id := range ids {
		if _, ok := s.rows[src+id]; ok {
			m[id] = true
		}
	}
	return m, nil
}
func (s *fakeStore) Insert(_ context.Context, rows []announce.Announcement, t time.Time, bl bool) ([]announce.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []announce.Announcement
	for _, r := range rows {
		k := r.Source + r.SourceID
		if _, ok := s.rows[k]; ok {
			continue
		}
		s.rows[k], s.backlog[k], s.firstSee[k] = r, bl, t
		out = append(out, r)
	}
	return out, nil
}

type fakeFetcher struct {
	mu     sync.Mutex
	byDay  map[string][]announce.Announcement // optional per-day BSE rows (key YYYYMMDD)
	nse    []announce.Announcement
	nseErr error
	pages  map[int][]announce.Announcement // BSE pages
	total  int
	calls  []int
}

func (f *fakeFetcher) NSELatest(context.Context) ([]announce.Announcement, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nse, f.nseErr
}
func (f *fakeFetcher) BSEPage(_ context.Context, from, _ time.Time, page int) ([]announce.Announcement, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rows, ok := f.byDay[from.In(announce.IST).Format("20060102")]; ok {
		return rows, 1, nil
	}
	if from.In(announce.IST).Format("20060102") == "20261025" { // "today" in setup()
		f.calls = append(f.calls, page)
	}
	return f.pages[page], f.total, nil
}

func ann(src, id string, disseminated time.Time) announce.Announcement {
	return announce.Announcement{Source: src, SourceID: id, Symbol: "SYM" + id, Category: "Cat", Headline: "h" + id, DisseminatedAt: disseminated}
}

func setup() (*Poller, *fakeFetcher, *fakeStore, *bytes.Buffer, *time.Time) {
	f, s, buf := &fakeFetcher{pages: map[int][]announce.Announcement{}, total: 1}, newFakeStore(), &bytes.Buffer{}
	now := time.Date(2026, 10, 25, 10, 0, 0, 0, announce.IST)
	p := New(f, s, 5*time.Second, log.New(buf, "", 0))
	p.Now = func() time.Time { return now }
	return p, f, s, buf, &now
}

func TestBacklogThenDedupeAndLag(t *testing.T) {
	p, f, s, buf, now := setup()
	f.nse = []announce.Announcement{ann("NSE", "1", now.Add(-time.Hour))}
	p.Once(context.Background())
	if !s.backlog["NSE1"] || strings.Contains(buf.String(), "NEW NSE") {
		t.Fatalf("first cycle must be stored as backlog without lag logging:\n%s", buf)
	}
	*now = now.Add(5 * time.Second)
	f.nse = []announce.Announcement{ann("NSE", "2", now.Add(-3*time.Second)), ann("NSE", "1", now.Add(-time.Hour))}
	p.Once(context.Background())
	if len(s.rows) != 2 || s.backlog["NSE2"] {
		t.Fatalf("want 2 rows, row 2 not backlog; got %d", len(s.rows))
	}
	if !strings.Contains(buf.String(), "lag=3.0s") {
		t.Fatalf("lag not logged:\n%s", buf)
	}
	p.Once(context.Background()) // same data again: nothing new
	if got := strings.Count(buf.String(), "NEW NSE"); got != 1 {
		t.Fatalf("duplicate logged, NEW count=%d", got)
	}
	if !strings.Contains(p.Stats(), "NSE new=1") {
		t.Fatalf("stats: %s", p.Stats())
	}
}

func TestBSEPagingStopsAtKnownRows(t *testing.T) {
	p, f, s, _, now := setup()
	mk := func(ids ...string) []announce.Announcement {
		var r []announce.Announcement
		for _, id := range ids {
			r = append(r, ann("BSE", id, now.Add(-time.Second)))
		}
		return r
	}
	f.total = 5
	f.pages = map[int][]announce.Announcement{1: mk("a", "b"), 2: mk("c", "d"), 3: mk("e", "f")}
	p.Once(context.Background()) // backlog cycle reads page 1 only
	if len(f.calls) != 1 || len(s.rows) != 2 {
		t.Fatalf("backlog calls=%v rows=%d", f.calls, len(s.rows))
	}
	f.calls = nil
	f.pages = map[int][]announce.Announcement{1: mk("n1", "n2"), 2: mk("n3", "b"), 3: mk("x")}
	p.Once(context.Background()) // page 1 all new -> page 2 has known "b" -> stop
	if fmt.Sprint(f.calls) != "[1 2]" || len(s.rows) != 5 {
		t.Fatalf("calls=%v rows=%d", f.calls, len(s.rows))
	}
}

func TestErrorIsolationAndBackoff(t *testing.T) {
	p, f, s, buf, now := setup()
	f.nseErr = errors.New("http 403: Access Denied")
	f.pages = map[int][]announce.Announcement{1: {ann("BSE", "1", now.Add(-time.Minute))}}
	p.Once(context.Background())
	if len(s.rows) != 1 || !strings.Contains(buf.String(), "ERROR NSE") {
		t.Fatalf("BSE must still be ingested when NSE fails:\n%s", buf)
	}
	f.nseErr = nil
	f.nse = []announce.Announcement{ann("NSE", "9", now.Add(-time.Second))}
	*now = now.Add(2 * time.Second) // inside the 5s backoff: NSE skipped
	p.Once(context.Background())
	if _, ok := s.rows["NSE9"]; ok {
		t.Fatal("NSE polled during backoff")
	}
	*now = now.Add(10 * time.Second)
	p.Once(context.Background())
	if _, ok := s.rows["NSE9"]; !ok {
		t.Fatal("NSE not polled after backoff expired")
	}
}

func TestNSEGapWarning(t *testing.T) {
	p, f, _, buf, now := setup()
	p.Once(context.Background()) // backlog cycle with empty feed
	var rows []announce.Announcement
	for i := 0; i < 20; i++ {
		rows = append(rows, ann("NSE", fmt.Sprint(100+i), now.Add(-time.Second)))
	}
	f.nse = rows
	*now = now.Add(5 * time.Second)
	p.Once(context.Background())
	if !strings.Contains(buf.String(), "WARN NSE possible gap") {
		t.Fatalf("expected gap warning:\n%s", buf)
	}
}

func TestBSEZeroRowsWarnsAndStatsShowFeedRows(t *testing.T) {
	p, f, _, buf, now := setup()
	f.nse = []announce.Announcement{ann("NSE", "1", now.Add(-time.Hour))}
	p.Once(context.Background()) // BSE page 1 is empty
	if !strings.Contains(buf.String(), "WARN BSE returned 0 rows") {
		t.Fatalf("expected zero-row warning:\n%s", buf)
	}
	if s := p.Stats(); !strings.Contains(s, "NSE new=0 errors=0 feed_rows=1") || !strings.Contains(s, "BSE new=0 errors=0 feed_rows=0") {
		t.Fatalf("stats: %s", s)
	}
}

func TestYesterdayBackfillOnStartupAndAfterMidnight(t *testing.T) {
	p, f, s, buf, now := setup()
	f.byDay = map[string][]announce.Announcement{
		"20261024": {ann("BSE", "old1", now.Add(-20*time.Hour))}, // dated yesterday, found on startup
	}
	p.Once(context.Background()) // backlog cycle also reads yesterday
	if _, ok := s.rows["BSEold1"]; !ok || !s.backlog["BSEold1"] {
		t.Fatal("yesterday's filing should be stored as backlog on startup")
	}
	// Just after midnight a late filing dated yesterday must still be picked up (Q-9).
	*now = time.Date(2026, 10, 26, 0, 5, 0, 0, announce.IST)
	f.byDay = map[string][]announce.Announcement{
		"20261025": {ann("BSE", "late", now.Add(-2*time.Minute))},
	}
	p.Once(context.Background())
	if _, ok := s.rows["BSElate"]; !ok || !strings.Contains(buf.String(), "NEW BSE") {
		t.Fatalf("late filing not ingested after midnight:\n%s", buf)
	}
}

func TestNoZeroRowWarningInEarlyHours(t *testing.T) {
	p, _, _, buf, now := setup()
	*now = time.Date(2026, 10, 26, 1, 0, 0, 0, announce.IST)
	p.Once(context.Background())
	if strings.Contains(buf.String(), "WARN BSE returned 0 rows") {
		t.Fatalf("no warning expected at 01:00 IST:\n%s", buf)
	}
}

func TestRunPollsBothSourcesAndStops(t *testing.T) {
	p, f, s, _, now := setup()
	p.Interval = time.Millisecond
	f.nse = []announce.Announcement{ann("NSE", "1", now.Add(-time.Hour))}
	f.pages = map[int][]announce.Announcement{1: {ann("BSE", "1", now.Add(-time.Hour))}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.rows)
		s.mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run did not poll both sources")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}

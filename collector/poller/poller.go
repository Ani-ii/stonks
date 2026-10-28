// Package poller runs the polling loops for both exchanges (D-022) and logs how late we see each filing (D-009).
// Each exchange has its own loop, so a slow or failing source never delays the other (D-042).
package poller

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"stonks/announce"
)

type Fetcher interface {
	NSELatest(ctx context.Context) ([]announce.Announcement, error)
	BSEPage(ctx context.Context, from, to time.Time, page int) ([]announce.Announcement, int, error)
}

type Store interface {
	Seen(ctx context.Context, source string, ids []string) (map[string]bool, error)
	Insert(ctx context.Context, rows []announce.Announcement, firstSeen time.Time, backlog bool) ([]announce.Announcement, error)
}

type Poller struct {
	F           Fetcher
	S           Store
	Interval    time.Duration
	MaxBSEPages int           // page cap per cycle (default 3)
	BSEDays     int           // widen today's BSE range this many days back (0 = today only; D-033)
	NSECap      int           // NSE feed row cap, used for gap warnings (default 20)
	MaxBackoff  time.Duration // default 5 minutes
	Now         func() time.Time
	Log         *log.Logger

	mu       sync.Mutex // guards everything below
	cycle    int        // used by Once only
	cycles   map[string]int
	backoff  map[string]time.Duration
	nextTry  map[string]time.Time
	lags     map[string][]time.Duration
	newRows  map[string]int
	errs     map[string]int
	lastRows map[string]int // rows returned by the feed in the latest cycle (page 1 of today for BSE)
}

func New(f Fetcher, s Store, interval time.Duration, l *log.Logger) *Poller {
	return &Poller{F: f, S: s, Interval: interval, MaxBSEPages: 3, NSECap: 20, MaxBackoff: 5 * time.Minute,
		Now: time.Now, Log: l,
		cycles: map[string]int{}, backoff: map[string]time.Duration{}, nextTry: map[string]time.Time{},
		lags: map[string][]time.Duration{}, newRows: map[string]int{}, errs: map[string]int{}, lastRows: map[string]int{}}
}

// Run polls both exchanges, each in its own loop, until ctx is cancelled, and prints stats about once a minute.
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, src := range []string{"NSE", "BSE"} {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			first := true
			for {
				p.runOne(ctx, src, first) // the first cycle of each source is "backlog"
				first = false
				select {
				case <-ctx.Done():
					return
				case <-time.After(p.Interval):
				}
			}
		}(src)
	}
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			p.Log.Println("stopping:", p.Stats())
			return
		case <-tick.C:
			p.Log.Println(p.Stats())
		}
	}
}

// Once runs one sequential cycle for both sources (used by tests). The first call is the backlog cycle.
func (p *Poller) Once(ctx context.Context) {
	p.mu.Lock()
	backlog := p.cycle == 0
	p.cycle++
	p.mu.Unlock()
	p.runOne(ctx, "NSE", backlog)
	p.runOne(ctx, "BSE", backlog)
}

// runOne polls one source once. Backlog rows are stored but flagged, because their first-seen time says
// nothing about how fast we detect new filings (D-028).
func (p *Poller) runOne(ctx context.Context, src string, backlog bool) {
	p.mu.Lock()
	p.cycles[src]++
	skip := p.Now().Before(p.nextTry[src])
	p.mu.Unlock()
	if skip {
		return
	}
	var err error
	if src == "NSE" {
		err = p.pollNSE(ctx, backlog)
	} else {
		err = p.pollBSE(ctx, backlog)
	}
	if err == nil {
		p.mu.Lock()
		p.backoff[src] = 0
		p.mu.Unlock()
		return
	}
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	p.errs[src]++
	b := p.backoff[src] * 2
	if b < p.Interval {
		b = p.Interval
	}
	if b > p.MaxBackoff {
		b = p.MaxBackoff
	}
	p.backoff[src] = b
	p.nextTry[src] = p.Now().Add(b)
	p.mu.Unlock()
	p.Log.Printf("ERROR %s: %v (backing off %v)", src, err, b)
}

func (p *Poller) setLastRows(src string, n int) {
	p.mu.Lock()
	p.lastRows[src] = n
	p.mu.Unlock()
}

func (p *Poller) pollNSE(ctx context.Context, backlog bool) error {
	rows, err := p.F.NSELatest(ctx)
	if err != nil {
		return err
	}
	p.setLastRows("NSE", len(rows))
	fresh, err := p.ingest(ctx, "NSE", rows, backlog)
	if err != nil {
		return err
	}
	if !backlog && p.NSECap > 0 && len(rows) >= p.NSECap && fresh == len(rows) {
		p.Log.Printf("WARN NSE possible gap: feed returned %d rows and all were new; older announcements may have been missed", len(rows))
	}
	return nil
}

func (p *Poller) pollBSE(ctx context.Context, backlog bool) error {
	now := p.Now().In(announce.IST)
	if err := p.bseRange(ctx, now.AddDate(0, 0, -p.BSEDays), now, backlog, true); err != nil {
		return err
	}
	// Filings dated yesterday: on startup and in the first minutes after midnight (Q-9). Single-day range.
	if backlog || (now.Hour() == 0 && now.Minute() < 15) {
		y := now.AddDate(0, 0, -1)
		return p.bseRange(ctx, y, y, backlog, false)
	}
	return nil
}

// bseRange reads newest-first pages for the date range, continuing only while every row on a page is new (D-029).
func (p *Poller) bseRange(ctx context.Context, from, to time.Time, backlog, isToday bool) error {
	pageCap := p.MaxBSEPages
	if backlog {
		pageCap = 1
	}
	for page := 1; page <= pageCap; page++ {
		rows, total, err := p.F.BSEPage(ctx, from, to, page)
		if err != nil {
			return err
		}
		if page == 1 && isToday {
			p.setLastRows("BSE", len(rows))
			// An empty feed right after midnight is normal, so only warn from 09:00 IST (Q-9).
			if len(rows) == 0 && backlog && to.Hour() >= 9 {
				p.Log.Printf("WARN BSE returned 0 rows for %s to %s; if this persists on a weekday, the date range or endpoint is wrong",
					from.Format("2006-01-02"), to.Format("2006-01-02"))
			}
		}
		if len(rows) == 0 {
			return nil
		}
		fresh, err := p.ingest(ctx, "BSE", rows, backlog)
		if err != nil {
			return err
		}
		if fresh < len(rows) || page >= total {
			return nil // reached rows we already have, or the last page
		}
		if page == pageCap && !backlog {
			p.Log.Printf("WARN BSE possible gap: page cap %d reached with all rows new (total pages %d)", pageCap, total)
		}
	}
	return nil
}

// ingest stores unseen rows and logs them. It returns how many rows were new.
func (p *Poller) ingest(ctx context.Context, src string, rows []announce.Announcement, backlog bool) (int, error) {
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.SourceID
	}
	seen, err := p.S.Seen(ctx, src, ids)
	if err != nil {
		return 0, err
	}
	var unseen []announce.Announcement
	for _, r := range rows {
		if !seen[r.SourceID] {
			unseen = append(unseen, r)
		}
	}
	if len(unseen) == 0 {
		return 0, nil
	}
	now := p.Now()
	inserted, err := p.S.Insert(ctx, unseen, now, backlog)
	if err != nil {
		return 0, err
	}
	if backlog {
		p.Log.Printf("backlog %s: stored %d existing announcements (not counted in latency stats)", src, len(inserted))
		return len(unseen), nil
	}
	for _, a := range inserted {
		lag := now.Sub(a.DisseminatedAt)
		p.mu.Lock()
		p.lags[src] = append(p.lags[src], lag)
		p.newRows[src]++
		p.mu.Unlock()
		name := a.Symbol
		if name == "" {
			name = a.Company
		}
		h := a.Headline
		if len(h) > 70 {
			h = h[:70] + "..."
		}
		p.Log.Printf("NEW %s %-14s %s | %s | exch=%s lag=%.1fs", src, name, strings.TrimSpace(a.Category+" "+a.Subcategory), h,
			a.DisseminatedAt.In(announce.IST).Format("15:04:05.000"), lag.Seconds())
	}
	return len(unseen), nil
}

// Stats summarizes counters and first-seen lag (seen time minus exchange time) per source.
func (p *Poller) Stats() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	parts := []string{fmt.Sprintf("cycles=%d", p.cycles["NSE"])}
	for _, src := range []string{"NSE", "BSE"} {
		l := append([]time.Duration(nil), p.lags[src]...)
		sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
		s := fmt.Sprintf("%s new=%d errors=%d feed_rows=%d", src, p.newRows[src], p.errs[src], p.lastRows[src])
		if len(l) > 0 {
			s += fmt.Sprintf(" lag median=%.1fs p95=%.1fs", l[len(l)/2].Seconds(), l[len(l)*95/100].Seconds())
		}
		parts = append(parts, s)
	}
	return "stats: " + strings.Join(parts, " | ")
}

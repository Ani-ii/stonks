// Command evaluate scores stored signals against end-of-day prices (M4, v1).
//
//	go run ./cmd/evaluate -db ~/collector.db
//
// It downloads missing price files (NSE bhavcopy, NSE index, BSE bhavcopy), scores every
// signal that has no outcome yet, prints one line per signal and a scorecard.
// It only reads signals/announcements and writes prices and outcomes: safe next to the collector.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"stonks/evaluator"
	"stonks/prices"
	"stonks/store"
)

var ist = time.FixedZone("IST", 5*3600+30*60)

const day = "2006-01-02"

func main() {
	dbPath := flag.String("db", "collector.db", "SQLite file shared with the collector")
	model := flag.String("model", "", "score only signals from this model name (default: all)")
	indexName := flag.String("index", "Nifty 50", "index used to remove market moves")
	target := flag.Float64("target", 2.5, "take-profit, % from entry open")
	stop := flag.Float64("stop", 1.5, "stop-loss, % from entry open")
	cost := flag.Float64("cost", 0.3, "round-trip cost, %")
	band := flag.Float64("band", 1.0, "excess move below this % counts as NEUTRAL")
	minVol := flag.Float64("min-rel-volume", 0.2, "entry-day volume below this multiple of the 20-day average marks the signal ILLIQUID")
	fetch := flag.Bool("fetch", true, "download missing price files")
	useBSE := flag.Bool("bse", true, "also download BSE bhavcopy (URL unverified)")
	before := flag.Int("days-before", 45, "calendar days of history to load before the first filing")
	redo := flag.Bool("redo", false, "re-score signals that already have an outcome")
	pause := flag.Duration("pause", 700*time.Millisecond, "pause between downloads")
	nseBase := flag.String("nse-base", prices.NSEBase, "NSE archive base URL (for tests)")
	bseBase := flag.String("bse-base", prices.BSEBase, "BSE base URL (for tests)")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	db, err := sql.Open("sqlite", "file:"+*dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	ev := store.NewEval(db)
	if err := ev.EnsureSchema(ctx); err != nil {
		log.Fatalf("schema: %v", err)
	}

	rows, err := ev.ToScore(ctx, *model, *redo)
	if err != nil {
		log.Fatalf("list signals: %v", err)
	}
	if len(rows) == 0 {
		fmt.Println("No signals waiting to be scored.")
	} else {
		first := time.UnixMilli(rows[0].PublishedMs).In(ist)
		last := time.UnixMilli(rows[len(rows)-1].PublishedMs).In(ist)
		today := time.Now().In(ist)
		to := last.AddDate(0, 0, 10)
		if to.After(today) {
			to = today
		}
		if *fetch {
			fetchDays(ctx, ev, first.AddDate(0, 0, -*before), to, today, *indexName, *useBSE, *nseBase, *bseBase, *pause)
		}
		cfg := evaluator.Config{TargetPct: *target, StopPct: *stop, CostPct: *cost, NeutralBand: *band, VolDays: 20, MinRelVolume: *minVol}
		fmt.Printf("\nScoring %d signals (target %.1f%%, stop %.1f%%, cost %.1f%%, neutral band %.1f%%, index %s)\n\n",
			len(rows), *target, *stop, *cost, *band, *indexName)
		for _, r := range rows {
			if ctx.Err() != nil {
				break
			}
			pub := time.UnixMilli(r.PublishedMs).In(ist)
			from, till := pub.AddDate(0, 0, -45).Format(day), pub.AddDate(0, 0, 10).Format(day)
			stock, src, err := findBars(ctx, ev, r, from, till)
			if err != nil {
				log.Fatalf("prices: %v", err)
			}
			index, err := ev.Bars(ctx, "IDX", *indexName, from, till)
			if err != nil {
				log.Fatalf("index prices: %v", err)
			}
			sig := evaluator.Signal{Symbol: r.Symbol, Direction: strings.ToUpper(r.Direction), PublishedAt: pub}
			res := evaluator.Evaluate(sig, stock, index, cfg)
			if err := ev.SaveOutcome(ctx, r.SignalID, src, res, time.Now()); err != nil {
				log.Fatalf("save outcome: %v", err)
			}
			printLine(r, res)
		}
	}

	sc, err := ev.Scorecard(ctx, *model)
	if err != nil {
		log.Fatalf("scorecard: %v", err)
	}
	printScorecard(sc)
}

// findBars looks for the stock's daily bars: first under the filing's own exchange code,
// then by ISIN on NSE, then by ISIN on BSE. Returns the source that had them.
func findBars(ctx context.Context, ev *store.Eval, r store.ScoreRow, from, to string) ([]evaluator.Bar, string, error) {
	type try struct{ src, code, isin string }
	var tries []try
	if r.Source == "NSE" && r.Symbol != "" {
		tries = append(tries, try{"NSE", r.Symbol, ""})
	}
	if r.Source == "BSE" && r.ScripCode != "" {
		tries = append(tries, try{"BSE", r.ScripCode, ""})
	}
	if r.ISIN != "" {
		tries = append(tries, try{"NSE", "", r.ISIN}, try{"BSE", "", r.ISIN})
	}
	for _, t := range tries {
		var bars []evaluator.Bar
		var err error
		if t.isin != "" {
			bars, err = ev.BarsByISIN(ctx, t.src, t.isin, from, to)
		} else {
			bars, err = ev.Bars(ctx, t.src, t.code, from, to)
		}
		if err != nil {
			return nil, "", err
		}
		if len(bars) > 0 {
			return bars, t.src, nil
		}
	}
	return nil, "", nil
}

type job struct {
	src   string
	url   func(time.Time) string
	parse func([]byte) ([]prices.DayBar, error)
}

// fetchDays downloads every missing weekday file between from and to, newest first.
// Holidays (404) are remembered for past dates. A web page instead of a file (what BSE sends
// for dates it has no file for) counts as "no file" only after a real file arrived from that
// source in this run, so a block page cannot silently mark everything as missing. A source that
// fails 5 times in a row, or returns a file in an unexpected format, is skipped for the rest of the run.
func fetchDays(ctx context.Context, ev *store.Eval, from, to, today time.Time, indexName string, useBSE bool, nseBase, bseBase string, pause time.Duration) {
	cl := prices.NewClient()
	jobs := []job{
		{"NSE", func(d time.Time) string { return prices.NSEBhavURL(nseBase, d) },
			func(b []byte) ([]prices.DayBar, error) { return prices.ParseUDiFF(b, "NSE") }},
		{"IDX", func(d time.Time) string { return prices.NSEIndexURL(nseBase, d) },
			func(b []byte) ([]prices.DayBar, error) {
				bar, err := prices.ParseNSEIndex(b, indexName)
				if err != nil {
					return nil, err
				}
				return []prices.DayBar{bar}, nil
			}},
	}
	if useBSE {
		jobs = append(jobs, job{"BSE", func(d time.Time) string { return prices.BSEBhavURL(bseBase, d) },
			func(b []byte) ([]prices.DayBar, error) { return prices.ParseUDiFF(b, "BSE") }})
	}
	todayStr := today.Format(day)
	streak := map[string]int{}
	got := map[string]int{}
	start := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, ist)
	end := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, ist)
	for d := end; !d.Before(start); d = d.AddDate(0, 0, -1) {
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		date := d.Format(day)
		for _, j := range jobs {
			if ctx.Err() != nil {
				return
			}
			if streak[j.src] >= 5 {
				continue
			}
			if has, err := ev.HasDay(ctx, j.src, date); err != nil || has {
				continue
			}
			data, err := cl.Get(ctx, j.url(d))
			time.Sleep(pause)
			switch {
			case errors.Is(err, prices.ErrNotFound):
				if date < todayStr {
					_ = ev.SaveDay(ctx, j.src, date, "NOT_FOUND", nil, time.Now())
				}
				streak[j.src] = 0
				continue
			case errors.Is(err, prices.ErrHTML):
				if got[j.src] > 0 && date < todayStr {
					_ = ev.SaveDay(ctx, j.src, date, "NOT_FOUND", nil, time.Now())
					streak[j.src] = 0
					continue
				}
				streak[j.src]++
				if streak[j.src] == 5 {
					fmt.Printf("WARN %s: got web pages instead of files 5 times in a row (blocked, or no files for these dates); skipped for this run\n", j.src)
				}
				continue
			case err != nil:
				streak[j.src]++
				fmt.Printf("WARN %s %s: %v\n", j.src, date, err)
				continue
			}
			bars, err := j.parse(data)
			if err != nil {
				streak[j.src] = 5 // wrong format: stop this source and show the reason once
				fmt.Printf("WARN %s %s: %v\nWARN %s skipped for this run\n", j.src, date, err, j.src)
				continue
			}
			if err := ev.SaveDay(ctx, j.src, date, "OK", bars, time.Now()); err != nil {
				log.Fatalf("save prices: %v", err)
			}
			streak[j.src] = 0
			got[j.src]++
		}
	}
	fmt.Printf("Price files downloaded this run: NSE %d, index %d, BSE %d\n", got["NSE"], got["IDX"], got["BSE"])
}

func printLine(r store.ScoreRow, res evaluator.Result) {
	name := []rune(r.Company)
	if len(name) > 26 {
		name = name[:26]
	}
	head := fmt.Sprintf("%s %-26s %-7s %.2f %-6s", r.Source, string(name), strings.ToUpper(r.Direction), r.Confidence, r.Materiality)
	if res.EntryDate == "" {
		why := res.Status
		if res.Bucket != "" {
			why += " (" + res.Bucket + ")"
		}
		fmt.Printf("%s | %s\n", head, why)
		return
	}
	tag := ""
	if res.Status != evaluator.StatusOK {
		tag = " [" + res.Status + ", not scored]"
	}
	mark := "x"
	if res.Hit {
		mark = "ok"
	}
	fmt.Printf("%s | %s %s | gap %+.2f%% open-close %+.2f%% excess %+.2f%% | vol x%.1f | actual %s %s | %s %+.2f%%%s\n",
		head, res.Bucket, res.EntryDate, res.GapPct, res.OpenToClosePct, res.ExcessTotalPct, res.RelVolume,
		res.Label, mark, res.ExitReason, res.TradePct, tag)
}

func printScorecard(sc store.Scorecard) {
	fmt.Println("\n=== Scorecard (after-close and pre-open filings, scored on daily bars) ===")
	if len(sc.Status) > 0 {
		keys := make([]string, 0, len(sc.Status))
		for k := range sc.Status {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, sc.Status[k]))
		}
		fmt.Println("Outcome status: " + strings.Join(parts, ", ") + "  (only OK is scored)")
	}
	n, hits := 0, 0
	for _, l := range sc.Lines {
		n += l.N
		hits += l.Hits
		avg := "n/a"
		win := "n/a"
		if l.Trades > 0 {
			avg = fmt.Sprintf("%+.2f%%", l.AvgTradePct)
			win = fmt.Sprintf("%.0f%%", 100*float64(l.Wins)/float64(l.Trades))
		}
		fmt.Printf("  signal %-7s %-12s n=%-3d direction hit %3.0f%% | trades %d win rate %s avg after costs %s\n",
			l.Direction, l.Bucket, l.N, 100*float64(l.Hits)/float64(l.N), l.Trades, win, avg)
	}
	if n == 0 {
		fmt.Println("  nothing scored yet")
		return
	}
	fmt.Printf("All: n=%d, signal matched actual direction %.0f%%\n", n, 100*float64(hits)/float64(n))
	fmt.Printf("Actual directions: UP %d, DOWN %d, NEUTRAL %d\n", sc.Labels["UP"], sc.Labels["DOWN"], sc.Labels["NEUTRAL"])
	fmt.Printf("Baselines: always UP %.0f%%, always DOWN %.0f%%, always NEUTRAL %.0f%%\n",
		100*float64(sc.Labels["UP"])/float64(n), 100*float64(sc.Labels["DOWN"])/float64(n), 100*float64(sc.Labels["NEUTRAL"])/float64(n))
	if n < 100 {
		fmt.Println("Note: fewer than 100 scored signals; treat these numbers as anecdotes, not evidence.")
	}
}

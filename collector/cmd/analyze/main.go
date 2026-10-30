// Command analyze runs the filter and an analyzer over the newest filings in the collector database
// and prints the results. It never writes to the database and never sends Telegram messages.
//
//	go run ./cmd/analyze -db ~/collector.db -n 5 -provider mock
//	go run ./cmd/analyze -db ~/collector.db -n 5 -provider gemini -model gemini-3.5-flash-lite
//
// The Gemini key is read from the GEMINI_API_KEY environment variable (never from a flag or file).
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"stonks/analyzer"
	"stonks/announce"
	"stonks/attach"
	"stonks/filter"
	"stonks/match"
	"stonks/store"
)

func main() {
	dbPath := flag.String("db", "collector.db", "SQLite file written by the collector")
	n := flag.Int("n", 5, "how many ANALYZE-class filings to run through the model")
	scan := flag.Int("scan", 300, "how many of the newest filings to classify")
	provider := flag.String("provider", "mock", "mock or gemini")
	model := flag.String("model", "gemini-3.5-flash-lite", "Gemini model id (pin an exact id, not a -latest alias)")
	usePDF := flag.Bool("pdf", false, "also download each filing's PDF and give its text to the model (needs pdftotext)")
	flag.Parse()

	var an analyzer.Analyzer
	switch *provider {
	case "mock":
		an = analyzer.Mock{}
	case "gemini":
		key := os.Getenv("GEMINI_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, "GEMINI_API_KEY is not set. Run: source ~/.stonks.env")
			os.Exit(2)
		}
		an = analyzer.NewGemini(key, *model)
	default:
		fmt.Fprintln(os.Stderr, "unknown -provider; use mock or gemini")
		os.Exit(2)
	}

	db, err := sql.Open("sqlite", "file:"+*dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	ctx := context.Background()
	rows, err := store.New(db).Recent(ctx, *scan)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading database:", err)
		os.Exit(1)
	}

	counts := map[filter.Decision]int{}
	reasons := map[string]int{}   // decision + reason -> count
	unclassified := map[string]int{} // category / subcategory strings that matched no rule
	for _, a := range rows {
		d := filter.Classify(a)
		counts[d.Decision]++
		reasons[d.Decision.String()+": "+d.Reason]++
		if d.Reason == "unclassified" {
			unclassified[strings.TrimSpace(a.Category+" / "+a.Subcategory)]++
		}
	}
	// Pick up to n ANALYZE filings. BSE first (its headlines are more informative), and skip a filing that is
	// the same event as one already picked from the other exchange (analyse once, not twice).
	var picked []announce.Announcement
	for _, src := range []string{"BSE", "NSE"} {
		for _, a := range rows {
			if a.Source != src || len(picked) >= *n || filter.Classify(a).Decision != filter.Analyze {
				continue
			}
			dup := false
			for _, p := range picked {
				if match.SameEvent(p, a, 15*time.Minute) {
					dup = true
					break
				}
			}
			if !dup {
				picked = append(picked, a)
			}
		}
	}
	fmt.Printf("Classified the newest %d filings: ANALYZE=%d REVIEW=%d SKIP=%d\n", len(rows),
		counts[filter.Analyze], counts[filter.Review], counts[filter.Skip])
	fmt.Println("Top reasons:")
	for _, kv := range top(reasons, 12) {
		fmt.Printf("  %3d  %s\n", kv.n, kv.k)
	}
	if len(unclassified) > 0 {
		fmt.Println("Categories that matched no rule (candidates for new rules):")
		for _, kv := range top(unclassified, 15) {
			fmt.Printf("  %3d  %s\n", kv.n, kv.k)
		}
	}
	fmt.Printf("\nRunning %d through %s\n\n", len(picked), an.Name())

	var att *attach.Fetcher
	if *usePDF {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		att = attach.New(cache + "/stonks-attach")
	}
	var inTok, outTok int
	for _, a := range picked {
		in := analyzer.Input{Filing: a}
		note := ""
		if att != nil {
			if txt, err := att.Text(ctx, a); err != nil {
				note = " [no PDF text: " + err.Error() + "]"
			} else {
				in.Text = txt
				note = fmt.Sprintf(" [PDF text: %d chars]", len(txt))
			}
		}
		sig, err := an.Analyze(ctx, in)
		var rl *analyzer.ErrRateLimited
		if errors.As(err, &rl) {
			fmt.Printf("RATE LIMITED (retry after %v); stopping\n", rl.RetryAfter)
			break
		}
		if err != nil {
			fmt.Printf("[%s] %s: ERROR %v\n", a.Source, a.Company, err)
			continue
		}
		inTok += sig.InputTokens
		outTok += sig.OutputTokens
		head := a.Headline
		if len(head) > 80 {
			head = head[:80] + "..."
		}
		fmt.Printf("[%s] %s | %s | %s%s\n    -> %s (confidence %.2f, materiality %s): %s\n",
			a.Source, a.Company, strings.TrimSpace(a.Category+" / "+a.Subcategory), head, note,
			strings.ToUpper(sig.Direction), sig.Confidence, sig.Materiality, sig.Reason)
	}
	fmt.Printf("\nTokens used: %d in, %d out\n", inTok, outTok)
}

type kv struct {
	k string
	n int
}

func top(m map[string]int, limit int) []kv {
	var out []kv
	for k, n := range m {
		out = append(out, kv{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Command runner turns new filings into stored signals (and, if you ask for it, Telegram alerts).
// Run it next to the collector; both use the same SQLite file.
//
//	go run ./cmd/runner -db ~/collector.db -provider mock -notify print        # no model, no Telegram
//	go run ./cmd/runner -db ~/collector.db -provider gemini -notify print      # real model, alerts printed
//	go run ./cmd/runner -db ~/collector.db -provider gemini -notify telegram   # real model, real alerts
//
// Secrets come from environment variables only: GEMINI_API_KEY, TELEGRAM_BOT_TOKEN, TELEGRAM_CHAT_ID
// (put them in ~/.stonks.env and run `source ~/.stonks.env` first).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	_ "modernc.org/sqlite"

	"stonks/analyzer"
	"stonks/attach"
	"stonks/notify"
	"stonks/runner"
	"stonks/store"
)

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func main() {
	dbPath := flag.String("db", "collector.db", "SQLite file written by the collector")
	provider := flag.String("provider", "mock", "mock or gemini")
	model := flag.String("model", "gemini-3.5-flash-lite", "Gemini model id (pin an exact id, not a -latest alias)")
	usePDF := flag.Bool("pdf", true, "download each filing's PDF and give its text to the model (needs pdftotext)")
	notifyMode := flag.String("notify", "off", "off, print or telegram")
	interval := flag.Duration("interval", 10*time.Second, "pause between passes")
	since := flag.Duration("since", 6*time.Hour, "only consider filings published within this window")
	dailyCap := flag.Int("daily-cap", 450, "maximum model calls per rolling 24 hours (free tier: 500); 0 = no cap")
	minMat := flag.String("min-materiality", "medium", "alerts need at least this materiality: high or medium")
	minConf := flag.Float64("min-confidence", 0.5, "alerts need at least this confidence")
	flag.Parse()

	var an analyzer.Analyzer
	switch *provider {
	case "mock":
		an = analyzer.Mock{}
	case "gemini":
		key := os.Getenv("GEMINI_API_KEY")
		if key == "" {
			die("GEMINI_API_KEY is not set. Run: source ~/.stonks.env")
		}
		an = analyzer.NewGemini(key, *model)
	default:
		die("unknown -provider %q; use mock or gemini", *provider)
	}

	var notifier runner.Notifier
	switch *notifyMode {
	case "off":
	case "print":
		notifier = notify.Printer{W: os.Stdout}
	case "telegram":
		tg := notify.NewTelegram(os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID"))
		if err := tg.Check(); err != nil {
			die("%v", err)
		}
		notifier = tg
	default:
		die("unknown -notify %q; use off, print or telegram", *notifyMode)
	}

	db, err := sql.Open("sqlite", "file:"+*dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		die("%v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	st := store.New(db)
	if err := st.Migrate(ctx); err != nil {
		die("migrate: %v", err)
	}

	logger := log.New(os.Stdout, "", log.LstdFlags)
	r := runner.New(st, an, runner.Config{
		Since: *since, DailyCap: *dailyCap, MinMateriality: *minMat, MinConfidence: *minConf,
	}, logger)
	r.N = notifier
	if *usePDF {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		att := attach.New(cache + "/stonks-attach")
		r.Text, r.Link = att.Text, att.URL
	}
	logger.Printf("runner started: db=%s model=%s pdf=%v notify=%s since=%v daily-cap=%d (Ctrl+C to stop)",
		*dbPath, an.Name(), *usePDF, *notifyMode, *since, *dailyCap)
	r.Run(ctx, *interval)
	logger.Println("runner stopped")
}

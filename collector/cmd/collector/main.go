// Command collector polls NSE and BSE announcements and stores them in SQLite.
//
//	go run ./cmd/collector -db collector.db -interval 5s
package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (no C compiler needed on Windows)

	"stonks/fetch"
	"stonks/poller"
	"stonks/store"
)

func main() {
	dbPath := flag.String("db", "collector.db", "SQLite file")
	interval := flag.Duration("interval", 5*time.Second, "delay between polling cycles")
	bsePages := flag.Int("bse-pages", 3, "max BSE pages per cycle")
	bseDays := flag.Int("bse-days", 0, "BSE date range start: days back from today (0 = today only)")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmicroseconds)
	db, err := sql.Open("sqlite", "file:"+*dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		logger.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // single writer

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	st := store.New(db)
	if err := st.Migrate(ctx); err != nil {
		logger.Fatalf("migrate: %v", err)
	}
	p := poller.New(fetch.New(), st, *interval, logger)
	p.MaxBSEPages = *bsePages
	p.BSEDays = *bseDays
	logger.Printf("collector started: db=%s interval=%v (Ctrl+C to stop)", *dbPath, *interval)
	p.Run(ctx)
}

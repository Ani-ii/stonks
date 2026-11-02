package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stonks/evaluator"
	"stonks/prices"
)

func openEvalDB(t *testing.T) (*sql.DB, *Eval) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatal(err)
	}
	ev := NewEval(db)
	if err := ev.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ev.EnsureSchema(context.Background()); err != nil { // idempotent
		t.Fatalf("second EnsureSchema: %v", err)
	}
	return db, ev
}

func addSignal(t *testing.T, db *sql.DB, annID int64, source, symbol, scrip, company, dir, model string, pubMs int64) int64 {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO announcements (id, source, source_id, symbol, scrip_code, company, disseminated_ms, first_seen_ms, raw_json)
 VALUES (?,?,?,?,?,?,?,?,'{}')`, annID, source, "s"+company, symbol, scrip, company, pubMs, pubMs); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO signals (announcement_id, model, direction, confidence, materiality, reason, created_ms)
 VALUES (?,?,?,0.8,'high','r',?)`, annID, model, dir, pubMs)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestPricesRoundTripAndLookup(t *testing.T) {
	ctx := context.Background()
	_, ev := openEvalDB(t)
	now := time.Now()
	d1 := []prices.DayBar{{Src: "NSE", Code: "ACME", ISIN: "INE001", Date: "2026-10-27", Open: 99, High: 101, Low: 98, Close: 100, Volume: 1000}}
	d2 := []prices.DayBar{{Src: "NSE", Code: "ACME", ISIN: "INE001", Date: "2026-10-28", Open: 110, High: 113, Low: 109.5, Close: 112, Volume: 3000}}
	if err := ev.SaveDay(ctx, "NSE", "2026-10-28", "OK", d2, now); err != nil {
		t.Fatal(err)
	}
	if err := ev.SaveDay(ctx, "NSE", "2026-10-27", "OK", d1, now); err != nil {
		t.Fatal(err)
	}
	if err := ev.SaveDay(ctx, "NSE", "2026-10-26", "NOT_FOUND", nil, now); err != nil {
		t.Fatal(err)
	}
	if ok, _ := ev.HasDay(ctx, "NSE", "2026-10-26"); !ok {
		t.Fatal("holiday should be remembered")
	}
	if ok, _ := ev.HasDay(ctx, "NSE", "2026-10-25"); ok {
		t.Fatal("unknown day reported as handled")
	}
	bars, err := ev.Bars(ctx, "NSE", "ACME", "2026-10-01", "2026-11-10")
	if err != nil || len(bars) != 2 || bars[0].Date != "2026-10-27" || bars[1].Close != 112 {
		t.Fatalf("bars: %+v %v", bars, err)
	}
	byISIN, err := ev.BarsByISIN(ctx, "NSE", "INE001", "2026-10-01", "2026-11-10")
	if err != nil || len(byISIN) != 2 {
		t.Fatalf("by isin: %+v %v", byISIN, err)
	}
	if none, _ := ev.BarsByISIN(ctx, "NSE", "", "2026-10-01", "2026-11-10"); len(none) != 0 {
		t.Fatal("empty ISIN must not match")
	}
	// Saving the same day again replaces, it does not duplicate.
	d2[0].Close = 111
	if err := ev.SaveDay(ctx, "NSE", "2026-10-28", "OK", d2, now); err != nil {
		t.Fatal(err)
	}
	bars, _ = ev.Bars(ctx, "NSE", "ACME", "2026-10-01", "2026-11-10")
	if len(bars) != 2 || bars[1].Close != 111 {
		t.Fatalf("replace failed: %+v", bars)
	}
}

func TestScoreListAndScorecard(t *testing.T) {
	ctx := context.Background()
	db, ev := openEvalDB(t)
	pub := time.Date(2026, 10, 27, 19, 13, 0, 0, time.FixedZone("IST", 5*3600+1800)).UnixMilli()
	s1 := addSignal(t, db, 1, "NSE", "ACME", "", "Acme", "up", "m1", pub)
	s2 := addSignal(t, db, 2, "BSE", "", "500001", "Beta", "DOWN", "m1", pub+1000)
	addSignal(t, db, 3, "NSE", "GAMMA", "", "Gamma", "neutral", "m2", pub+2000)

	rows, err := ev.ToScore(ctx, "m1", false)
	if err != nil || len(rows) != 2 || rows[0].SignalID != s1 || rows[1].Source != "BSE" || rows[1].ScripCode != "500001" {
		t.Fatalf("to score: %+v %v", rows, err)
	}
	if all, _ := ev.ToScore(ctx, "", false); len(all) != 3 {
		t.Fatalf("model filter off should list 3, got %d", len(all))
	}

	now := time.Now()
	good := evaluator.Result{Status: "OK", Bucket: "after_close", EntryDate: "2026-10-28", Label: "UP", Hit: true, ExitReason: "target", TradePct: 2.2, GapPct: 10}
	bad := evaluator.Result{Status: "OK", Bucket: "after_close", EntryDate: "2026-10-28", Label: "UP", Hit: false, ExitReason: "stop", TradePct: -1.8}
	if err := ev.SaveOutcome(ctx, s1, "NSE", good, now); err != nil {
		t.Fatal(err)
	}
	if err := ev.SaveOutcome(ctx, s2, "BSE", evaluator.Result{Status: "NO_PRICES"}, now); err != nil {
		t.Fatal(err)
	}
	// OK outcome is not listed again; NO_PRICES is retried; redo lists everything.
	rows, _ = ev.ToScore(ctx, "m1", false)
	if len(rows) != 1 || rows[0].SignalID != s2 {
		t.Fatalf("expected only the NO_PRICES signal, got %+v", rows)
	}
	if rows, _ = ev.ToScore(ctx, "m1", true); len(rows) != 2 {
		t.Fatalf("redo should list both, got %d", len(rows))
	}
	if err := ev.SaveOutcome(ctx, s2, "BSE", bad, now); err != nil { // replaces NO_PRICES
		t.Fatal(err)
	}

	sc, err := ev.Scorecard(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Lines) != 2 || sc.Labels["UP"] != 2 || sc.Status["OK"] != 2 {
		t.Fatalf("scorecard: %+v", sc)
	}
	for _, l := range sc.Lines {
		switch l.Direction {
		case "UP":
			if l.N != 1 || l.Hits != 1 || l.Trades != 1 || l.Wins != 1 || l.AvgTradePct != 2.2 {
				t.Fatalf("UP line: %+v", l)
			}
		case "DOWN":
			if l.N != 1 || l.Hits != 0 || l.Wins != 0 || l.AvgTradePct != -1.8 {
				t.Fatalf("DOWN line: %+v", l)
			}
		default:
			t.Fatalf("unexpected line %+v", l)
		}
	}
}

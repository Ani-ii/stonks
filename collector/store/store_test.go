package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"stonks/analyzer"
	"stonks/announce"
)

func openTest(t *testing.T) *SQLite {
	t.Helper()
	var db *sql.DB
	var err error
	for _, name := range sql.Drivers() {
		if name == "sqlite" || name == "sqlite3" {
			db, err = sql.Open(name, "file:"+filepath.Join(t.TempDir(), "t.db"))
			break
		}
	}
	if db == nil || err != nil {
		t.Fatalf("no sqlite driver registered (%v)", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	s := New(db)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil { // must be safe to run twice
		t.Fatal(err)
	}
	return s
}

func ann(src, id, company string, at time.Time) announce.Announcement {
	return announce.Announcement{Source: src, SourceID: id, Company: company, Category: "Result", Subcategory: "Financial Results",
		Headline: "h" + id, AttachmentName: id + ".pdf", SubmittedAt: at, DisseminatedAt: at, Raw: []byte(`{}`)}
}

func TestInsertDedupeRecentAndFields(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	at := time.Date(2026, 10, 29, 20, 0, 0, 123_000_000, announce.IST)
	seen := at.Add(40 * time.Second)
	in, err := s.Insert(ctx, []announce.Announcement{ann("BSE", "a", "Acme Ltd", at), ann("BSE", "a", "Acme Ltd", at)}, seen, false)
	if err != nil || len(in) != 1 {
		t.Fatalf("duplicate in one batch must be stored once: n=%d err=%v", len(in), err)
	}
	ids, err := s.Seen(ctx, "BSE", []string{"a", "zzz"})
	if err != nil || !ids["a"] || ids["zzz"] {
		t.Fatalf("Seen wrong: %v %v", ids, err)
	}
	rows, err := s.Untriaged(ctx, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("untriaged: %d %v", len(rows), err)
	}
	r := rows[0]
	if r.A.Company != "Acme Ltd" || r.A.AttachmentName != "a.pdf" || !r.A.DisseminatedAt.Equal(at) || !r.FirstSeen.Equal(seen) || r.Backlog {
		t.Errorf("row fields wrong: %+v", r)
	}
}

func TestTriageSignalsPendingDuplicatesAndCap(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	base := time.Date(2026, 10, 29, 20, 0, 0, 0, announce.IST)
	s.Insert(ctx, []announce.Announcement{
		ann("BSE", "1", "Acme Ltd", base), ann("BSE", "2", "Beta Ltd", base.Add(time.Minute)), ann("NSE", "3", "Acme Limited", base.Add(2*time.Minute)),
	}, base.Add(time.Hour), false)

	rows, _ := s.Untriaged(ctx, 0, 10)
	if len(rows) != 3 || !rows[0].A.DisseminatedAt.Before(rows[2].A.DisseminatedAt) {
		t.Fatalf("untriaged should be 3 rows, oldest first: %d", len(rows))
	}
	now := base.Add(2 * time.Hour)
	for _, r := range rows {
		d := "ANALYZE"
		if r.A.SourceID == "2" {
			d = "SKIP"
		}
		if err := s.SetTriage(ctx, r.ID, d, "test", now); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := s.Untriaged(ctx, 0, 10); len(left) != 0 {
		t.Fatalf("all rows triaged, %d left", len(left))
	}
	if err := s.SetTriage(ctx, rows[2].ID, "DUPLICATE", "same event", now); err != nil { // update path
		t.Fatal(err)
	}
	const model = "mock"
	pend, err := s.PendingAnalysis(ctx, model, 0, 10)
	if err != nil || len(pend) != 1 || pend[0].A.SourceID != "1" {
		t.Fatalf("only filing 1 should be pending: %d %v", len(pend), err)
	}
	sig := analyzer.Signal{Direction: "up", Confidence: 0.8, Materiality: "high", Reason: " strong ", Model: model, InputTokens: 100, OutputTokens: 10}
	id, err := s.SaveSignal(ctx, SignalRecord{AnnouncementID: pend[0].ID, Sig: sig, TextHash: "abc", TextChars: 500, Created: now})
	if err != nil || id == 0 {
		t.Fatalf("save: id=%d err=%v", id, err)
	}
	if again, _ := s.SaveSignal(ctx, SignalRecord{AnnouncementID: pend[0].ID, Sig: sig, Created: now}); again != 0 {
		t.Error("second signal for the same filing and model must be ignored")
	}
	if pend, _ := s.PendingAnalysis(ctx, model, 0, 10); len(pend) != 0 {
		t.Error("filing with a signal must no longer be pending")
	}
	if other, _ := s.PendingAnalysis(ctx, "other-model", 0, 10); len(other) != 1 {
		t.Error("a different model should see the filing as pending")
	}
	refs, err := s.RecentSignals(ctx, 0)
	if err != nil || len(refs) != 1 || refs[0].TextHash != "abc" || refs[0].A.Company != "Acme Ltd" || refs[0].A.Source != "BSE" {
		t.Fatalf("recent signals: %+v %v", refs, err)
	}
	if n, _ := s.CountSignalsSince(ctx, now.Add(-time.Hour).UnixMilli(), model); n != 1 {
		t.Errorf("count since: %d", n)
	}
	if n, _ := s.CountSignalsSince(ctx, now.Add(time.Hour).UnixMilli(), model); n != 0 {
		t.Errorf("count in the future: %d", n)
	}
	if err := s.MarkNotified(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	var notified int64
	s.db.QueryRowContext(ctx, `SELECT notified_ms FROM signals WHERE id = ?`, id).Scan(&notified)
	if notified != now.UnixMilli() {
		t.Errorf("notified_ms = %d", notified)
	}
}

func TestSinceWindowExcludesOldFilings(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	old := time.Date(2026, 10, 1, 10, 0, 0, 0, announce.IST)
	s.Insert(ctx, []announce.Announcement{ann("BSE", "old", "Old Ltd", old)}, old, false)
	if rows, _ := s.Untriaged(ctx, old.Add(time.Hour).UnixMilli(), 10); len(rows) != 0 {
		t.Fatal("filing older than the window must not be returned")
	}
}

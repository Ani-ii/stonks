package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"testing"
	"time"

	"stonks/analyzer"
	"stonks/announce"
	"stonks/attach"
	"stonks/store"
)

// ---- in-memory store with the same semantics as store.SQLite ----

type fakeStore struct {
	rows    []store.Row
	triage  map[int64][2]string
	signals []store.SignalRecord
	sigID   []int64
	notified map[int64]bool
}

func newFake() *fakeStore {
	return &fakeStore{triage: map[int64][2]string{}, notified: map[int64]bool{}}
}

func (f *fakeStore) add(a announce.Announcement) int64 {
	id := int64(len(f.rows) + 1)
	f.rows = append(f.rows, store.Row{ID: id, A: a, FirstSeen: a.DisseminatedAt.Add(30 * time.Second)})
	return id
}

func (f *fakeStore) sorted(pred func(store.Row) bool, limit int) []store.Row {
	var out []store.Row
	for _, r := range f.rows {
		if pred(r) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].A.DisseminatedAt.Before(out[j].A.DisseminatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (f *fakeStore) Untriaged(_ context.Context, since int64, limit int) ([]store.Row, error) {
	return f.sorted(func(r store.Row) bool { _, ok := f.triage[r.ID]; return !ok && r.A.DisseminatedAt.UnixMilli() >= since }, limit), nil
}
func (f *fakeStore) SetTriage(_ context.Context, id int64, d, reason string, _ time.Time) error {
	f.triage[id] = [2]string{d, reason}
	return nil
}
func (f *fakeStore) hasSignal(id int64, model string) bool {
	for _, s := range f.signals {
		if s.AnnouncementID == id && s.Sig.Model == model {
			return true
		}
	}
	return false
}
func (f *fakeStore) PendingAnalysis(_ context.Context, model string, since int64, limit int) ([]store.Row, error) {
	return f.sorted(func(r store.Row) bool {
		return f.triage[r.ID][0] == "ANALYZE" && r.A.DisseminatedAt.UnixMilli() >= since && !f.hasSignal(r.ID, model)
	}, limit), nil
}
func (f *fakeStore) RecentSignals(_ context.Context, since int64) ([]store.SignalRef, error) {
	var out []store.SignalRef
	for i, s := range f.signals {
		a := f.rows[s.AnnouncementID-1].A
		if a.DisseminatedAt.UnixMilli() >= since {
			out = append(out, store.SignalRef{ID: f.sigID[i], AnnouncementID: s.AnnouncementID, A: a, TextHash: s.TextHash})
		}
	}
	return out, nil
}
func (f *fakeStore) SaveSignal(_ context.Context, rec store.SignalRecord) (int64, error) {
	if f.hasSignal(rec.AnnouncementID, rec.Sig.Model) {
		return 0, nil
	}
	f.signals = append(f.signals, rec)
	id := int64(len(f.signals))
	f.sigID = append(f.sigID, id)
	return id, nil
}
func (f *fakeStore) CountSignalsSince(_ context.Context, since int64, model string) (int, error) {
	n := 0
	for _, s := range f.signals {
		if s.Created.UnixMilli() >= since && s.Sig.Model == model && s.Sig.InputTokens > 0 {
			n++
		}
	}
	return n, nil
}
func (f *fakeStore) MarkNotified(_ context.Context, id int64, _ time.Time) error {
	f.notified[id] = true
	return nil
}

// ---- fakes for the model and Telegram ----

type fakeModel struct {
	calls  int
	answer func(in analyzer.Input) (analyzer.Signal, error)
}

func (m *fakeModel) Name() string { return "fake" }
func (m *fakeModel) Analyze(_ context.Context, in analyzer.Input) (analyzer.Signal, error) {
	m.calls++
	return m.answer(in)
}

func sig(dir string, conf float64, mat string) analyzer.Signal {
	return analyzer.Signal{Direction: dir, Confidence: conf, Materiality: mat, Reason: "because", Model: "fake", InputTokens: 100, OutputTokens: 10}
}

type fakeNotifier struct {
	msgs []string
	err  error
}

func (n *fakeNotifier) Send(_ context.Context, t string) (int64, error) {
	if n.err != nil {
		return 0, n.err
	}
	n.msgs = append(n.msgs, t)
	return 1, nil
}

var t0 = time.Date(2026, 10, 29, 20, 0, 0, 0, announce.IST)

func filing(src, id, company, cat, sub string, offset time.Duration) announce.Announcement {
	return announce.Announcement{Source: src, SourceID: id, Company: company, Category: cat, Subcategory: sub,
		Headline: "headline " + id, AttachmentName: id + ".pdf", DisseminatedAt: t0.Add(offset), SubmittedAt: t0.Add(offset)}
}

func setup(m *fakeModel) (*Runner, *fakeStore, *bytes.Buffer) {
	f, buf := newFake(), &bytes.Buffer{}
	r := New(f, m, Config{DailyCap: 0}, log.New(buf, "", 0))
	r.Now = func() time.Time { return t0.Add(10 * time.Minute) }
	return r, f, buf
}

func ok(dir string) *fakeModel {
	return &fakeModel{answer: func(analyzer.Input) (analyzer.Signal, error) { return sig(dir, 0.8, "high"), nil }}
}

func TestStepTriagesEverythingAndAnalysesOnlyAnalyzeClass(t *testing.T) {
	m := ok("up")
	r, f, _ := setup(m)
	f.add(filing("BSE", "1", "Acme Ltd", "Result", "Financial Results", 0))
	f.add(filing("BSE", "2", "Beta Ltd", "Board Meeting", "Board Meeting", time.Minute))
	f.add(filing("BSE", "3", "Gamma Ltd", "Company Update", "Press Release / Media Release", 2*time.Minute))
	r.Step(context.Background())
	want := map[int64]string{1: "ANALYZE", 2: "SKIP", 3: "REVIEW"}
	for id, d := range want {
		if f.triage[id][0] != d {
			t.Errorf("filing %d triage = %q, want %q", id, f.triage[id][0], d)
		}
	}
	if m.calls != 1 || len(f.signals) != 1 || f.signals[0].AnnouncementID != 1 {
		t.Fatalf("only filing 1 should be analysed: calls=%d signals=%d", m.calls, len(f.signals))
	}
	r.Step(context.Background()) // nothing new: no more model calls
	if m.calls != 1 {
		t.Fatalf("second pass must not re-analyse: calls=%d", m.calls)
	}
}

func TestAlertRules(t *testing.T) {
	answers := map[string]analyzer.Signal{
		"1": sig("up", 0.8, "high"),     // alert
		"2": sig("neutral", 0.9, "high"), // neutral never alerts
		"3": sig("up", 0.9, "low"),      // materiality too low
		"4": sig("down", 0.4, "medium"), // confidence too low
		"5": sig("down", 0.6, "medium"), // alert
	}
	m := &fakeModel{answer: func(in analyzer.Input) (analyzer.Signal, error) { return answers[in.Filing.SourceID], nil }}
	r, f, _ := setup(m)
	n := &fakeNotifier{}
	r.N = n
	r.Link = func(a announce.Announcement) string { return "https://example.test/" + a.AttachmentName }
	for i := 1; i <= 5; i++ {
		f.add(filing("BSE", fmt.Sprint(i), fmt.Sprintf("Company%d Ltd", i), "Result", "Financial Results", time.Duration(i)*time.Minute*20))
	}
	r.Cfg.EventWindow = time.Minute // keep the five companies independent
	r.Step(context.Background())
	if len(f.signals) != 5 || len(n.msgs) != 2 {
		t.Fatalf("signals=%d alerts=%d, want 5 and 2", len(f.signals), len(n.msgs))
	}
	if !strings.Contains(n.msgs[0], "▲ UP") || !strings.Contains(n.msgs[0], "Company1 Ltd") || !strings.Contains(n.msgs[0], "https://example.test/1.pdf") ||
		!strings.Contains(n.msgs[0], "Experimental signal") || !strings.Contains(n.msgs[0], "detected 30s later") {
		t.Errorf("alert text wrong:\n%s", n.msgs[0])
	}
	if !f.notified[1] || f.notified[2] {
		t.Errorf("notified flags wrong: %v", f.notified)
	}
}

func TestSameDocumentAndSameEventAreAnalysedOnce(t *testing.T) {
	m := ok("up")
	r, f, _ := setup(m)
	r.Text = func(_ context.Context, a announce.Announcement) (string, error) {
		if a.Company == "Knack Ltd" {
			return "identical results document text " + strings.Repeat("x", 300), nil
		}
		return "different text for " + a.SourceID + strings.Repeat("y", 300), nil
	}
	f.add(filing("BSE", "1", "Knack Ltd", "Result", "Financial Results", 0))
	f.add(filing("BSE", "2", "Knack Ltd", "Board Meeting", "Outcome of Board Meeting", 2*time.Minute)) // same PDF text, other file name
	f.add(filing("BSE", "3", "Schaeffler India Ltd", "Result", "Financial Results", 3*time.Minute))
	f.add(filing("NSE", "4", "Schaeffler India Limited", "Outcome of Board Meeting", "", 5*time.Minute)) // same event, other exchange
	r.Step(context.Background())
	if m.calls != 2 {
		t.Fatalf("expected 2 model calls, got %d", m.calls)
	}
	if got := f.triage[2]; got[0] != "DUPLICATE" || !strings.Contains(got[1], "same document") {
		t.Errorf("filing 2 should be a duplicate document: %v", got)
	}
	if got := f.triage[4]; got[0] != "DUPLICATE" || !strings.Contains(got[1], "same event") {
		t.Errorf("filing 4 should be a duplicate event: %v", got)
	}
}

func TestRateLimitPausesAndKeepsFilingPending(t *testing.T) {
	limited := true
	m := &fakeModel{answer: func(analyzer.Input) (analyzer.Signal, error) {
		if limited {
			return analyzer.Signal{}, &analyzer.ErrRateLimited{RetryAfter: 45 * time.Second}
		}
		return sig("up", 0.8, "high"), nil
	}}
	r, f, _ := setup(m)
	f.add(filing("BSE", "1", "Acme Ltd", "Result", "Financial Results", 0))
	if pause := r.Step(context.Background()); pause != 45*time.Second || len(f.signals) != 0 || f.triage[1][0] != "ANALYZE" {
		t.Fatalf("pause=%v signals=%d triage=%v", pause, len(f.signals), f.triage[1])
	}
	limited = false
	if pause := r.Step(context.Background()); pause != 0 || len(f.signals) != 1 {
		t.Fatalf("after the limit lifts the filing must be analysed: pause=%v signals=%d", pause, len(f.signals))
	}
}

func TestTransientKeepsPendingPermanentMarksError(t *testing.T) {
	mode := "transient"
	m := &fakeModel{answer: func(analyzer.Input) (analyzer.Signal, error) {
		switch mode {
		case "transient":
			return analyzer.Signal{}, &analyzer.ErrTransient{Err: errors.New("network down")}
		case "permanent":
			return analyzer.Signal{}, errors.New("answer is not JSON")
		}
		return sig("up", 0.8, "high"), nil
	}}
	r, f, _ := setup(m)
	f.add(filing("BSE", "1", "Acme Ltd", "Result", "Financial Results", 0))
	if pause := r.Step(context.Background()); pause != 30*time.Second || f.triage[1][0] != "ANALYZE" {
		t.Fatalf("transient: pause=%v triage=%v", pause, f.triage[1])
	}
	mode = "permanent"
	r.Step(context.Background())
	if f.triage[1][0] != "ERROR" || !strings.Contains(f.triage[1][1], "not JSON") {
		t.Fatalf("permanent failure should be recorded: %v", f.triage[1])
	}
	mode = "ok"
	r.Step(context.Background())
	if len(f.signals) != 0 {
		t.Error("an ERROR filing must not be retried automatically")
	}
}

func TestAttachmentFailures(t *testing.T) {
	var inputs []analyzer.Input
	m := &fakeModel{answer: func(in analyzer.Input) (analyzer.Signal, error) {
		inputs = append(inputs, in)
		return sig("up", 0.8, "high"), nil
	}}
	r, f, _ := setup(m)
	f.add(filing("BSE", "1", "Scan Ltd", "Result", "Financial Results", 0))
	r.Text = func(context.Context, announce.Announcement) (string, error) { return "", attach.ErrNoText }
	r.Step(context.Background())
	if len(inputs) != 1 || inputs[0].Text != "" || f.signals[0].TextChars != 0 {
		t.Fatalf("a scanned PDF should be analysed from metadata immediately: %v", inputs)
	}

	// A download problem is retried on later passes, then given up on.
	m2 := ok("up")
	r2, f2, _ := setup(m2)
	f2.add(filing("BSE", "9", "Net Ltd", "Result", "Financial Results", 0))
	r2.Text = func(context.Context, announce.Announcement) (string, error) { return "", errors.New("attachment http 503") }
	r2.Step(context.Background())
	r2.Step(context.Background())
	if m2.calls != 0 {
		t.Fatalf("model must not be called while the attachment is still being retried: calls=%d", m2.calls)
	}
	r2.Step(context.Background()) // third attempt: give up and analyse the metadata
	if m2.calls != 1 {
		t.Fatalf("after %d failed attempts the filing should be analysed from metadata: calls=%d", 3, m2.calls)
	}
}

func TestDailyCapPausesWithoutCallingTheModel(t *testing.T) {
	m := ok("up")
	r, f, buf := setup(m)
	r.Cfg.DailyCap = 2
	for i := 1; i <= 2; i++ { // two real calls already made today
		f.add(filing("BSE", fmt.Sprint("old", i), fmt.Sprintf("Old%d Ltd", i), "Result", "Financial Results", -time.Duration(i)*time.Hour))
		f.signals = append(f.signals, store.SignalRecord{AnnouncementID: int64(i), Sig: sig("up", 0.8, "high"), Created: t0})
		f.sigID = append(f.sigID, int64(i))
	}
	f.add(filing("BSE", "new", "New Ltd", "Result", "Financial Results", 0))
	if pause := r.Step(context.Background()); pause != 5*time.Minute || m.calls != 0 {
		t.Fatalf("pause=%v calls=%d", pause, m.calls)
	}
	if !strings.Contains(buf.String(), "daily cap") {
		t.Errorf("cap should be logged:\n%s", buf)
	}
}

func TestNotifierFailureKeepsTheSignal(t *testing.T) {
	r, f, buf := setup(ok("down"))
	r.N = &fakeNotifier{err: errors.New("telegram down")}
	f.add(filing("BSE", "1", "Acme Ltd", "Result", "Financial Results", 0))
	r.Step(context.Background())
	if len(f.signals) != 1 || len(f.notified) != 0 || !strings.Contains(buf.String(), "ERROR sending alert") {
		t.Fatalf("signal must be kept even if the alert failed: signals=%d notified=%v\n%s", len(f.signals), f.notified, buf)
	}
}

func TestOldFilingsOutsideTheWindowAreIgnored(t *testing.T) {
	m := ok("up")
	r, f, _ := setup(m)
	f.add(filing("BSE", "1", "Ancient Ltd", "Result", "Financial Results", -48*time.Hour))
	r.Step(context.Background())
	if m.calls != 0 || len(f.triage) != 0 {
		t.Fatalf("filing outside the %v window must be ignored: calls=%d triage=%v", r.Cfg.Since, m.calls, f.triage)
	}
}

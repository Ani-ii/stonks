package announce

import (
	"os"
	"testing"
	"time"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseNSE(t *testing.T) {
	got, err := ParseNSE(load(t, "nse_sample.json"))
	if err != nil || len(got) != 2 {
		t.Fatalf("err=%v len=%d", err, len(got))
	}
	a := got[0]
	if a.Source != "NSE" || a.SourceID != "106827793" || a.Symbol != "TRANSWORLD" || a.ISIN != "INE757B01015" {
		t.Errorf("bad ids: %+v", a)
	}
	if a.Category != "General Updates" || a.AttachmentURL == "" || len(a.Raw) == 0 {
		t.Errorf("bad content: %+v", a)
	}
	want := time.Date(2026, 10, 25, 0, 40, 36, 0, IST)
	if !a.DisseminatedAt.Equal(want) {
		t.Errorf("time %v want %v", a.DisseminatedAt, want)
	}
	if d := got[1].DisseminatedAt.Sub(got[1].SubmittedAt); d != time.Second {
		t.Errorf("second row dissemination lag %v want 1s", d)
	}
}

func TestParseBSE(t *testing.T) {
	got, err := ParseBSE(load(t, "bse_sample.json"))
	if err != nil || len(got) != 2 {
		t.Fatalf("err=%v len=%d", err, len(got))
	}
	a := got[1]
	if a.Source != "BSE" || a.ScripCode != "523218" || a.Subcategory != "Resignation of Chairman" || a.Category != "Company Update" {
		t.Errorf("bad fields: %+v", a)
	}
	want := time.Date(2026, 10, 25, 0, 10, 45, 87_000_000, IST)
	if !a.DisseminatedAt.Equal(want) {
		t.Errorf("time %v want %v (millisecond precision)", a.DisseminatedAt, want)
	}
}

func TestBSEEmptyDay(t *testing.T) {
	got, err := ParseBSE([]byte(`{"Table":[],"Table1":[{"ROWCNT":0}]}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("err=%v len=%d", err, len(got))
	}
}

func TestNSEEmptyAndGarbage(t *testing.T) {
	if got, err := ParseNSE([]byte(`[]`)); err != nil || len(got) != 0 {
		t.Fatalf("empty: err=%v len=%d", err, len(got))
	}
	if _, err := ParseNSE([]byte(`<HTML>Access Denied</HTML>`)); err == nil {
		t.Fatal("expected error for HTML body")
	}
}

func TestParseBSEPageTotal(t *testing.T) {
	rows, total, err := ParseBSEPage(load(t, "bse_sample.json"))
	if err != nil || len(rows) != 2 || total != 1 {
		t.Fatalf("err=%v rows=%d total=%d", err, len(rows), total)
	}
	if _, total, _ := ParseBSEPage([]byte(`{"Table":[],"Table1":[{"ROWCNT":0}]}`)); total != 0 {
		t.Fatalf("empty total=%d", total)
	}
}

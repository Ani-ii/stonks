package analyzer

import (
	"strings"
	"testing"
	"time"

	"stonks/announce"
)

func f(v float64) *float64 { return &v }

func filing(pub string) announce.Announcement {
	t, _ := time.ParseInLocation("2006-01-02 15:04", pub, announce.IST)
	return announce.Announcement{Source: "NSE", Company: "Acme", DisseminatedAt: t}
}

// A results table shaped like a typical filing: current quarter, previous quarter, year-ago quarter.
const table = `Particulars            30-Sep-2026   30-Jun-2026   30-Sep-2025
Revenue from operations   4,821.50      4,102.00      3,610.25
Net Profit after tax        312.40        250.10        180.75
`

func figs(rev, np Figures) *Extraction {
	return &Extraction{IsResults: true, PeriodEnd: "2026-09-30", PeriodType: "quarter", Basis: "consolidated", Revenue: rev, NetProfit: np}
}

func sig(dir string, conf float64, e *Extraction) Signal {
	return Signal{Direction: dir, Confidence: conf, Materiality: "high", Reason: "model reason", Extraction: e}
}

var good = func() *Extraction {
	return figs(Figures{f(4821.50), f(4102.00), f(3610.25)}, Figures{f(312.40), f(250.10), f(180.75)})
}

// The PG Electroplast case: the model said "down" although the figures it quoted show a rise.
func TestResolveOverridesWrongModelDirection(t *testing.T) {
	out := Resolve(sig("down", 0.85, good()), Input{Filing: filing("2026-10-27 18:00"), Text: table})
	if out.Direction != "up" || out.ModelDirection != "down" {
		t.Fatalf("got %+v", out)
	}
	if out.Confidence != CapDisagree || !strings.Contains(out.Flags, "model_disagrees") {
		t.Fatalf("confidence/flags: %+v", out)
	}
	if strings.Contains(out.Reason, "model reason") || !strings.Contains(out.Reason, "net_profit_yoy=+72.8%") {
		t.Fatalf("reason: %q", out.Reason)
	}
}

func TestResolveAgreementKeepsConfidenceAndModelReason(t *testing.T) {
	out := Resolve(sig("up", 0.8, good()), Input{Filing: filing("2026-10-27 18:00"), Text: table})
	if out.Direction != "up" || out.Confidence != 0.8 || out.Flags != "" || out.ModelDirection != "" {
		t.Fatalf("got %+v", out)
	}
	if !strings.HasPrefix(out.Reason, "model reason") || !strings.Contains(out.Reason, "[figures:") {
		t.Fatalf("reason: %q", out.Reason)
	}
}

// The Navoday case: FY March 2025 numbers filed in October 2026.
func TestResolveStale(t *testing.T) {
	e := good()
	e.PeriodEnd = "2025-03-31"
	out := Resolve(sig("down", 0.75, e), Input{Filing: filing("2026-10-27 19:00"), Text: table})
	if out.Direction != "neutral" || out.Materiality != "low" || out.Confidence > CapUncertain || out.Flags != "stale_period" {
		t.Fatalf("got %+v", out)
	}
}

func TestResolveFigureNotInText(t *testing.T) {
	e := good()
	e.NetProfit.Current = f(999.99) // not printed anywhere
	out := Resolve(sig("up", 0.9, e), Input{Filing: filing("2026-10-27 18:00"), Text: table})
	if out.Direction != "neutral" || out.Flags != "figure_not_in_text" || !strings.Contains(out.Reason, "net_profit.current") {
		t.Fatalf("got %+v", out)
	}
}

func TestResolveNoText(t *testing.T) {
	out := Resolve(sig("up", 0.9, good()), Input{Filing: filing("2026-10-27 18:00")})
	if out.Direction != "neutral" || out.Flags != "results_without_text" {
		t.Fatalf("got %+v", out)
	}
}

func TestResolveNonResultsUnchanged(t *testing.T) {
	s := sig("up", 0.7, nil)
	if out := Resolve(s, Input{Filing: filing("2026-10-27 18:00"), Text: "x"}); out != s {
		t.Fatalf("changed: %+v", out)
	}
	s.Extraction = &Extraction{IsResults: false}
	if out := Resolve(s, Input{Text: "x"}); out != s {
		t.Fatalf("changed: %+v", out)
	}
}

func TestParenthesisedNegativeIsFoundInText(t *testing.T) {
	txt := "Net loss for the period (45.20) 12.00 30.00\nRevenue 100.00 90.00 80.00"
	e := figs(Figures{f(100), f(90), f(80)}, Figures{f(-45.20), f(12), f(30)})
	out := Resolve(sig("neutral", 0.5, e), Input{Filing: filing("2026-10-27 18:00"), Text: txt})
	if out.Flags != "mixed_profit_revenue" && out.Direction != "down" {
		t.Fatalf("got %+v", out)
	}
	if out.Direction != "down" {
		t.Fatalf("profit to loss should be down: %+v", out)
	}
}

func TestCalcDirection(t *testing.T) {
	cases := []struct {
		name string
		e    Extraction
		want string
		ok   bool
	}{
		{"profit up", Extraction{NetProfit: Figures{Current: f(120), YearAgo: f(100)}}, "up", true},
		{"within band", Extraction{NetProfit: Figures{Current: f(105), YearAgo: f(100)}}, "neutral", true},
		{"loss to profit", Extraction{NetProfit: Figures{Current: f(5), YearAgo: f(-10)}}, "up", true},
		{"profit to loss", Extraction{NetProfit: Figures{Current: f(-5), YearAgo: f(10)}}, "down", true},
		{"zero base falls back to revenue", Extraction{NetProfit: Figures{Current: f(5), YearAgo: f(0)}, Revenue: Figures{Current: f(80), YearAgo: f(100)}}, "down", true},
		{"previous period only", Extraction{NetProfit: Figures{Current: f(150), Previous: f(100)}}, "up", true},
		{"nothing comparable", Extraction{NetProfit: Figures{Current: f(5)}}, "", false},
	}
	for _, c := range cases {
		got, _, ok := CalcDirection(c.e)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got %q,%v want %q,%v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestImplausibleChange(t *testing.T) {
	txt := "Profit 5000.00 100.00 90.00\nRevenue 100.00 90.00 80.00"
	e := figs(Figures{f(100), f(90), f(80)}, Figures{f(5000), f(100), f(90)}) // 55x vs year ago: unit/column slip
	out := Resolve(sig("up", 0.9, e), Input{Filing: filing("2026-10-27 18:00"), Text: txt})
	if out.Flags != "implausible_change" || out.Direction != "neutral" {
		t.Fatalf("got %+v", out)
	}
}

func TestMixedProfitRevenueFlagged(t *testing.T) {
	txt := "Revenue 80.00 90.00 100.00\nProfit 130.00 110.00 100.00"
	e := figs(Figures{f(80), f(90), f(100)}, Figures{f(130), f(110), f(100)})
	out := Resolve(sig("up", 0.8, e), Input{Filing: filing("2026-10-27 18:00"), Text: txt})
	if out.Direction != "up" || out.Flags != "mixed_profit_revenue" {
		t.Fatalf("got %+v", out)
	}
}

func TestParseGeminiCarriesExtraction(t *testing.T) {
	inner := `{"direction":"down","confidence":0.8,"materiality":"high","reason":"r","extraction":{"is_results":true,"period_end":"2026-09-30","period_type":"quarter","basis":"consolidated","unit":"Rs lakh","revenue":{"current":10,"previous_period":null,"year_ago":8},"net_profit":{"current":2,"previous_period":null,"year_ago":1},"net_profit_row_text":"PAT 2 1"}}`
	body := `{"candidates":[{"content":{"parts":[{"text":` + quote(inner) + `}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7}}`
	s, err := parseGemini([]byte(body), "gemini:x")
	if err != nil || s.Extraction == nil || !s.Extraction.IsResults {
		t.Fatalf("%v %+v", err, s)
	}
	if s.Extraction.Revenue.Previous != nil || *s.Extraction.Revenue.YearAgo != 8 || *s.Extraction.NetProfit.Current != 2 {
		t.Fatalf("figures: %+v", s.Extraction)
	}
	// And a plain answer without extraction still parses.
	plain := `{"direction":"neutral","confidence":0.3,"materiality":"low","reason":"r"}`
	body = `{"candidates":[{"content":{"parts":[{"text":` + quote(plain) + `}]}}]}`
	if s, err = parseGemini([]byte(body), "gemini:x"); err != nil || s.Extraction != nil {
		t.Fatalf("%v %+v", err, s)
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func TestSchemaRequiresExtraction(t *testing.T) {
	props := responseSchema["properties"].(map[string]any)
	if _, ok := props["extraction"]; !ok {
		t.Fatal("extraction missing from response schema")
	}
	found := false
	for _, r := range responseSchema["required"].([]string) {
		if r == "extraction" {
			found = true
		}
	}
	if !found {
		t.Fatal("extraction must be required so the model cannot skip it on a results filing")
	}
	if !strings.Contains(SystemPrompt, "is_results false") || strings.Contains(SystemPrompt, `omit "extraction"`) {
		t.Fatal("prompt must ask for extraction on every filing")
	}
}

// FDC case: profit figures all printed, but one revenue cell not found in the text.
func TestRevenueUnverifiedButProfitVerifiedKeepsSignal(t *testing.T) {
	txt := "Profit for the period 4906 13248 2837\nRevenue 57214 66769 99999"
	e := figs(Figures{f(57214), f(66769), f(47303)}, Figures{f(4906), f(13248), f(2837)}) // 47303 not printed
	out := Resolve(sig("up", 0.8, e), Input{Filing: filing("2026-10-27 18:00"), Text: txt})
	if out.Direction != "up" || out.Confidence != 0.8 || out.Flags != "revenue_unverified" {
		t.Fatalf("got %+v", out)
	}
	if !strings.Contains(out.Reason, "net_profit_yoy=+72.9") {
		t.Fatalf("reason: %q", out.Reason)
	}
	// the stored extraction is what the model returned, untouched
	if out.Extraction.Revenue.YearAgo == nil || *out.Extraction.Revenue.YearAgo != 47303 {
		t.Fatalf("stored extraction was modified: %+v", out.Extraction)
	}
}

// Panth case: the deciding line (net profit) is not in the document, so the signal is neutral.
func TestProfitNotInTextRejectsEvenIfRevenueIsFound(t *testing.T) {
	txt := "Total Income 9493.97 0.00 14559.40\nProfit for the period 505.24"
	e := figs(Figures{f(9493.97), f(0), f(14559.40)}, Figures{f(11959.3), nil, f(-5.79)})
	out := Resolve(sig("up", 0.8, e), Input{Filing: filing("2026-10-27 18:00"), Text: txt})
	if out.Direction != "neutral" || out.Flags != "figure_not_in_text" || !strings.Contains(out.Reason, "net_profit.current") {
		t.Fatalf("got %+v", out)
	}
}

// No profit figures printed at all: revenue becomes the deciding line and must verify.
func TestRevenueStrictWhenNoProfitFigures(t *testing.T) {
	txt := "Revenue 120.00 100.00"
	ok := figs(Figures{f(120), nil, f(100)}, Figures{})
	out := Resolve(sig("up", 0.7, ok), Input{Filing: filing("2026-10-27 18:00"), Text: txt})
	if out.Direction != "up" || out.Flags != "" {
		t.Fatalf("verified revenue: %+v", out)
	}
	bad := figs(Figures{f(120), nil, f(77)}, Figures{})
	out = Resolve(sig("up", 0.7, bad), Input{Filing: filing("2026-10-27 18:00"), Text: txt})
	if out.Direction != "neutral" || out.Flags != "figure_not_in_text" {
		t.Fatalf("unverified revenue must reject: %+v", out)
	}
}

func TestEBITDAUnverifiedIsDroppedNotFatal(t *testing.T) {
	e := good()
	e.EBITDA = Figures{f(777.77), nil, nil}
	out := Resolve(sig("up", 0.8, e), Input{Filing: filing("2026-10-27 18:00"), Text: table})
	if out.Direction != "up" || out.Flags != "ebitda_unverified" {
		t.Fatalf("got %+v", out)
	}
}

package analyzer

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Structured extraction for results filings (D-076, implements D-075 item 3).
//
// The model copies the figures out of the results table; CODE decides the direction from them. This
// stops the PG Electroplast failure (model text said profit "fell sharply" while the quoted figures
// showed a rise). Every figure is also checked against the filing text, so a made-up or mis-copied
// number cannot silently drive a signal. All constants are experiment settings fixed in advance and
// are not to be tuned on a handful of results (D-069).
const (
	GrowthUp           = 0.10 // growth above +10% -> up
	GrowthDown         = -0.10
	MaxStaleDays       = 100  // period end older than this at publication -> stale (annual results are due within 60 days)
	MaxPlausibleGrowth = 20.0 // |growth| above 20x -> probably a column or unit error
	CapUncertain       = 0.30 // confidence cap when the figures cannot be trusted
	CapDisagree        = 0.45 // confidence cap when the model's own call contradicts the figures (below the 0.5 alert threshold, D-066)
)

// Figures holds one line item for up to three periods, as printed in the table. nil = not printed.
type Figures struct {
	Current  *float64 `json:"current"`
	Previous *float64 `json:"previous_period"` // immediately preceding period (previous quarter), if shown
	YearAgo  *float64 `json:"year_ago"`        // same period last year
}

// Extraction is what the model copies from a results filing.
type Extraction struct {
	IsResults     bool    `json:"is_results"`
	PeriodEnd     string  `json:"period_end"`  // end of the current period, YYYY-MM-DD
	PeriodType    string  `json:"period_type"` // quarter, half_year or year
	Basis         string  `json:"basis"`       // consolidated or standalone
	Unit          string  `json:"unit"`        // as printed, e.g. "Rs in lakhs"
	Revenue       Figures `json:"revenue"`
	NetProfit     Figures `json:"net_profit"`
	EBITDA        Figures `json:"ebitda"`
	ProfitRowText string  `json:"net_profit_row_text"` // the table row used for net profit, copied
}

// growth returns (cur-base)/|base|. Loss to profit and profit to loss give sensible signs.
func growth(cur, base *float64) (float64, bool) {
	if cur == nil || base == nil || *base == 0 {
		return 0, false
	}
	return (*cur - *base) / math.Abs(*base), true
}

func dirOf(g float64) string {
	switch {
	case g > GrowthUp:
		return "up"
	case g < GrowthDown:
		return "down"
	}
	return "neutral"
}

// CalcDirection decides the direction from the figures: net profit versus the year-ago period first,
// then revenue year on year, then the same two against the previous period. basis names what was used.
func CalcDirection(e Extraction) (dir, basis string, ok bool) {
	type cand struct {
		name string
		f    Figures
		base func(Figures) *float64
	}
	yoy := func(f Figures) *float64 { return f.YearAgo }
	qoq := func(f Figures) *float64 { return f.Previous }
	for _, c := range []cand{
		{"net_profit_yoy", e.NetProfit, yoy}, {"revenue_yoy", e.Revenue, yoy},
		{"net_profit_prev_period", e.NetProfit, qoq}, {"revenue_prev_period", e.Revenue, qoq},
	} {
		if g, ok := growth(c.f.Current, c.base(c.f)); ok {
			return dirOf(g), fmt.Sprintf("%s=%+.1f%%", c.name, g*100), true
		}
	}
	return "", "", false
}

var numRe = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

// numbersIn collects every number printed in the text as absolute values in hundredths, so a figure the
// model returns can be looked up whatever its sign style ("(12.50)", "-12.50", "12.50").
func numbersIn(text string) map[int64]bool {
	out := map[int64]bool{}
	for _, m := range numRe.FindAllString(text, -1) {
		v, err := strconv.ParseFloat(strings.ReplaceAll(m, ",", ""), 64)
		if err == nil {
			out[int64(math.Round(v*100))] = true
		}
	}
	return out
}

// hasAny reports whether a line item has at least one printed figure.
func (f Figures) hasAny() bool { return f.Current != nil || f.Previous != nil || f.YearAgo != nil }

// missingIn lists the figures of one line item that do not appear anywhere in the filing text.
func missingIn(name string, f Figures, nums map[int64]bool) []string {
	var missing []string
	for _, it := range []struct {
		lbl string
		p   *float64
	}{{"current", f.Current}, {"previous", f.Previous}, {"year_ago", f.YearAgo}} {
		if it.p != nil && !nums[int64(math.Round(math.Abs(*it.p)*100))] {
			missing = append(missing, name+"."+it.lbl)
		}
	}
	return missing
}

// Resolve applies the structured extraction to a model signal. Filings that are not results pass
// through unchanged. For results, the direction comes from the figures; untrustworthy extractions
// (figure not in the text, stale period, no comparable figures, implausible jump) become neutral with
// low confidence. Flags records what happened so the scorecard can filter on it.
func Resolve(s Signal, in Input) Signal {
	e := s.Extraction
	if e == nil || !e.IsResults {
		return s
	}
	var flags []string
	finish := func(dir string, conf float64, reason string) Signal {
		if s.Direction != dir {
			s.ModelDirection = s.Direction
		}
		s.Direction = dir
		if s.Confidence > conf {
			s.Confidence = conf
		}
		if reason != "" {
			s.Reason = reason
		}
		s.Flags = strings.Join(flags, ",")
		return s
	}
	uncertain := func(flag, why string) Signal {
		flags = append(flags, flag)
		s.Materiality = "low"
		return finish("neutral", CapUncertain, why)
	}

	if strings.TrimSpace(in.Text) == "" {
		return uncertain("results_without_text", "Results filing but no document text to read figures from.")
	}
	// Verification: the line that decides the direction (net profit, or revenue when no profit figure is
	// printed) must be found in the text, or the signal is neutral. A secondary line that fails is dropped
	// and flagged, so one mis-copied revenue cell does not throw away a verified profit comparison.
	// The stored Extraction stays exactly as the model returned it; the calculation uses the copy ec.
	ec := *e
	nums := numbersIn(in.Text)
	strict := "net_profit"
	if !ec.NetProfit.hasAny() {
		strict = "revenue"
	}
	var strictMiss []string
	if strict == "net_profit" {
		strictMiss = missingIn("net_profit", ec.NetProfit, nums)
	} else {
		strictMiss = missingIn("revenue", ec.Revenue, nums)
	}
	if len(strictMiss) > 0 {
		return uncertain("figure_not_in_text", "Extracted figures not found in the filing text ("+strings.Join(strictMiss, ", ")+"); not trusted.")
	}
	if strict == "net_profit" && len(missingIn("revenue", ec.Revenue, nums)) > 0 {
		ec.Revenue = Figures{}
		flags = append(flags, "revenue_unverified")
	}
	if len(missingIn("ebitda", ec.EBITDA, nums)) > 0 {
		ec.EBITDA = Figures{}
		flags = append(flags, "ebitda_unverified")
	}
	if end, err := time.Parse("2006-01-02", e.PeriodEnd); err == nil {
		pub := in.Filing.DisseminatedAt
		if pub.Sub(end) > MaxStaleDays*24*time.Hour {
			return uncertain("stale_period", "Results are for the period ended "+e.PeriodEnd+", too old to be news at publication.")
		}
		if end.Sub(pub) > 48*time.Hour {
			return uncertain("period_in_future", "Period end "+e.PeriodEnd+" is after the publication date; extraction not trusted.")
		}
	} else {
		flags = append(flags, "no_period_end")
	}
	dir, basis, ok := CalcDirection(ec)
	if !ok {
		return uncertain("no_comparable_figures", "Results filing without comparable figures (no prior-period base).")
	}
	for _, f := range []Figures{ec.NetProfit, ec.Revenue} {
		for _, base := range []*float64{f.YearAgo, f.Previous} {
			if g, ok := growth(f.Current, base); ok && math.Abs(g) > MaxPlausibleGrowth {
				return uncertain("implausible_change", "Change of more than 20 times between periods ("+basis+"); likely a column or unit error.")
			}
		}
	}
	// Profit and revenue pulling in opposite directions is a mixed result: keep the call but flag it.
	pg, pok := growth(ec.NetProfit.Current, ec.NetProfit.YearAgo)
	rg, rok := growth(ec.Revenue.Current, ec.Revenue.YearAgo)
	if pok && rok && dirOf(pg) != "neutral" && dirOf(rg) != "neutral" && dirOf(pg) != dirOf(rg) {
		flags = append(flags, "mixed_profit_revenue")
	}
	if s.Direction == dir {
		s.Reason += " [figures: " + basis + "]"
		return finish(dir, 1, "")
	}
	flags = append(flags, "model_disagrees")
	return finish(dir, CapDisagree, fmt.Sprintf("Computed from the filing's figures (%s, %s basis). The model's own call was %s and was overridden.", basis, e.Basis, s.Direction))
}

// Package analyzer turns one announcement into a structured signal using an LLM provider.
// Providers (mock, Gemini; others later) sit behind one interface, so the evaluation code never
// depends on which model produced a signal.
package analyzer

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"stonks/announce"
)

// Input is everything the model gets to see: the filing's metadata and, optionally, text taken from
// the attached document (D-025: only fetched for filings that pass the filter).
type Input struct {
	Filing announce.Announcement
	Text   string
}

// Signal is the model's structured answer. Model records which model produced it, so results from
// different models are never mixed up in the scorecard.
//
// D-076 additions (all zero for non-results filings): Extraction holds the figures copied from a results
// table; ModelDirection is what the model said when code overrode it; Flags is a comma-separated list of
// what the consistency checks found (stale_period, figure_not_in_text, model_disagrees, ...). Flags is a
// string, not a slice, so Signal stays comparable with ==.
type Signal struct {
	Direction      string  // "up", "down" or "neutral"
	Confidence     float64 // 0..1
	Materiality    string  // "high", "medium" or "low"
	Reason         string  // one or two sentences
	Model          string
	InputTokens    int
	OutputTokens   int
	Extraction     *Extraction
	ModelDirection string
	Flags          string
}

type Analyzer interface {
	Name() string
	Analyze(ctx context.Context, in Input) (Signal, error)
}

// SystemPrompt fixes the question, so every provider answers the same one. The target must stay in
// step with how the evaluator measures outcomes.
const SystemPrompt = `You are an equity research assistant for companies listed on the Indian exchanges (NSE and BSE).
You are shown one corporate announcement. Judge its likely effect on the company's share price over the
next full trading session after the announcement becomes public, relative to the broad market.
Rules:
- Use only the information given. You have no price data and no later news.
- If the text does not contain what is needed to judge (for example, results announced without numbers), answer "neutral" with low confidence and say what is missing in the reason.
- Routine or procedural filings are "neutral" with materiality "low".
- Keep the reason to one or two sentences.
- Always include "extraction". If the filing is a financial results statement, set is_results true and copy the figures EXACTLY as printed in the table, in the table's own unit, for the headline statement (consolidated if both consolidated and standalone are given; say which in basis). Do not compute, convert or round anything. Read the column headings and use them to pick the right column: tables usually run current quarter, previous quarter, same quarter last year, then year-to-date, then full year. Revenue is total revenue from operations (or total income if that is all there is); net_profit is profit after tax for the period. For half-year or annual statements use the current period for "current" and the same period last year for "year_ago", and leave "previous_period" null. Use null for any figure that is not printed. net_profit_row_text is the table row you used for net profit, copied as printed. period_end is the end date of the current period as YYYY-MM-DD.
- For any filing that is not a results statement, return extraction with is_results false and nothing else.
Answer only with the requested JSON.`

// DefaultMaxChars limits attachment text sent to the model (after Compact). The first live PDF run showed
// 12,000 characters cut off the net-profit figures of a long results filing.
const DefaultMaxChars = 40000

var spaceRun = regexp.MustCompile(`[ \t]{3,}`)

// Compact removes the padding that `pdftotext -layout` adds to keep table columns aligned (runs of spaces,
// page-break characters, repeated blank lines) so that more real content fits in the character limit.
func Compact(s string) string {
	s = strings.NewReplacer("\r", "", "\f", "\n").Replace(s)
	var out []string
	blank := 0
	for _, l := range strings.Split(s, "\n") {
		l = spaceRun.ReplaceAllString(strings.TrimRight(l, " \t"), "  ")
		if strings.TrimSpace(l) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// BuildPrompt formats the filing for the model. Times are shown in IST, as the exchanges publish them.
func BuildPrompt(in Input, maxChars int) string {
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	f := in.Filing
	var b strings.Builder
	fmt.Fprintf(&b, "Exchange: %s\n", f.Source)
	fmt.Fprintf(&b, "Company: %s\n", f.Company)
	if f.Symbol != "" {
		fmt.Fprintf(&b, "NSE symbol: %s\n", f.Symbol)
	}
	if f.ScripCode != "" {
		fmt.Fprintf(&b, "BSE scrip code: %s\n", f.ScripCode)
	}
	fmt.Fprintf(&b, "Published (IST): %s\n", f.DisseminatedAt.In(announce.IST).Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "Category: %s\n", f.Category)
	if f.Subcategory != "" {
		fmt.Fprintf(&b, "Subcategory: %s\n", f.Subcategory)
	}
	if f.Headline != "" {
		fmt.Fprintf(&b, "Headline: %s\n", f.Headline)
	}
	if f.Subject != "" {
		fmt.Fprintf(&b, "Subject: %s\n", f.Subject)
	}
	if t := Compact(in.Text); t != "" {
		if len(t) > maxChars {
			t = t[:maxChars] + "\n[text truncated]"
		}
		fmt.Fprintf(&b, "\nAttached document text:\n%s\n", t)
	} else {
		b.WriteString("\n(No attached document text was provided; judge from the fields above.)\n")
	}
	return b.String()
}

// Validate normalises and checks a model answer; invalid answers are errors, never guesses.
func Validate(s Signal) (Signal, error) {
	s.Direction = strings.ToLower(strings.TrimSpace(s.Direction))
	s.Materiality = strings.ToLower(strings.TrimSpace(s.Materiality))
	s.Reason = strings.TrimSpace(s.Reason)
	switch s.Direction {
	case "up", "down", "neutral":
	default:
		return s, fmt.Errorf("invalid direction %q", s.Direction)
	}
	switch s.Materiality {
	case "high", "medium", "low":
	default:
		return s, fmt.Errorf("invalid materiality %q", s.Materiality)
	}
	if s.Confidence < 0 || s.Confidence > 1 {
		return s, fmt.Errorf("confidence %v outside 0..1", s.Confidence)
	}
	if s.Reason == "" {
		return s, fmt.Errorf("empty reason")
	}
	return s, nil
}

// Package filter is the free, rule-based first pass (D-024, D-044): it decides which announcements are
// worth an LLM call, using only the exchange-provided category fields. It never calls a model.
//
// Rules come from the category and subcategory strings seen in the first days of live data
// (see DECISIONS.md section 12). Patterns marked "not yet seen" are plausible additions.
package filter

import (
	"strings"

	"stonks/announce"
)

type Decision int

const (
	Skip    Decision = iota // routine; do not analyse
	Review                  // unclear or lower priority; sample before deciding
	Analyze                 // send to the analyzer
)

func (d Decision) String() string {
	switch d {
	case Analyze:
		return "ANALYZE"
	case Review:
		return "REVIEW"
	}
	return "SKIP"
}

type Result struct {
	Decision Decision
	Reason   string // the rule that matched
}

type rule struct {
	d       Decision
	pattern string // lowercase substring of "category | subcategory"
}

// Order matters: the first group that matches wins.
var analyze = []string{
	"outcome of board meeting", "financial results", "integrated filing", "acquisition",
	"award of order", "receipt of order", "change in management", "appointment",
	"resignation", "buy back", "buyback", "agreements", "memorandum of understanding",
	// not yet seen in our data, but clearly material:
	"dividend", "merger", "amalgamation", "demerger", "bonus", "stock split", "fund raising",
	"preferential", "rights issue", "credit rating",
}

// seen in live data and clearly material:
var analyzeMore = []string{"commencement of commercial production", "issue of securities"}

var skip = []string{
	"analyst / investor meet", "analysts/institutional investor meet", "newspaper publication",
	"trading window", "certificate under", "monitoring agency report", "amendments to memorandum", "shareholders meeting", "postal ballot", "egm", "agm",
	"esop", "esps", "statement of deviation", "insider trading", "board meeting intimation",
}

var review = []string{
	"press release", "media release", "investor presentation", "general", "updates", "others", "record date",
}

// Classify applies the rules to the category, subcategory and (for transcripts) the headline.
func Classify(a announce.Announcement) Result {
	text := strings.ToLower(a.Category + " | " + a.Subcategory)
	head := strings.ToLower(a.Headline)

	if strings.Contains(text, "transcript") || strings.Contains(head, "transcript") {
		return Result{Review, "earnings call transcript (lower priority)"}
	}
	// BSE results arrive as category "Result"; match it exactly so "Results of voting" does not count.
	if strings.TrimSpace(strings.ToLower(a.Category)) == "result" {
		return Result{Analyze, "category result"}
	}
	// Routine administrative appointments are not price-moving (seen in live data: company secretary).
	for _, p := range []string{"company secretary", "compliance officer"} {
		if strings.Contains(text, p) {
			return Result{Skip, p}
		}
	}
	for _, p := range append(append([]string{}, analyze...), analyzeMore...) {
		if strings.Contains(text, p) {
			return Result{Analyze, p}
		}
	}
	// BSE board-meeting intimations: category and subcategory are both "Board Meeting".
	if strings.TrimSpace(strings.ToLower(a.Category)) == "board meeting" {
		return Result{Skip, "board meeting intimation"}
	}
	for _, p := range skip {
		if strings.Contains(text, p) {
			return Result{Skip, p}
		}
	}
	for _, p := range review {
		if strings.Contains(text, p) {
			return Result{Review, p}
		}
	}
	return Result{Review, "unclassified"}
}

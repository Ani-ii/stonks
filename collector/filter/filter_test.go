package filter

import (
	"testing"

	"stonks/announce"
)

// Every row below uses category/subcategory text seen in the owner's live collector output.
func TestClassifyRealCategories(t *testing.T) {
	cases := []struct {
		src, cat, sub, head string
		want                Decision
	}{
		{"BSE", "Company Update", "Buy back", "", Analyze},
		{"BSE", "Board Meeting", "Outcome of Board Meeting", "", Analyze},
		{"BSE", "Result", "Financial Results", "", Analyze},
		{"BSE", "Company Update", "Acquisition", "", Analyze},
		{"BSE", "Company Update", "Change in Management", "", Analyze},
		{"BSE", "Company Update", "Resignation of Statutory Auditors", "", Analyze},
		{"BSE", "Company Update", "Memorandum of Understanding /Agreements", "", Analyze},
		{"BSE", "Company Update", "Award of Order / Receipt of Order", "", Analyze},
		{"BSE", "Company Update", "Appointment of Company Secretary / Compliance Officer", "", Skip},
		{"BSE", "Company Update", "Appointment of Director", "", Analyze},
		{"BSE", "Company Update", "Monitoring Agency Report", "", Skip},
		{"NSE", "Monitoring Agency Report", "", "", Skip},
		{"BSE", "Company Update", "Amendments to Memorandum & Articles of Association", "", Skip},
		{"NSE", "Certificate under SEBI (Depositories and Participants) Regulations, 2018", "", "", Skip},
		{"NSE", "Shareholders meeting", "", "", Skip},
		{"NSE", "Commencement of commercial production/operations", "", "", Analyze},
		{"BSE", "Company Update", "Issue of Securities", "", Analyze},
		{"BSE", "Company Update", "Allotment of Equity Shares", "", Review},
		{"NSE", "Giving guarantees/indemnity/ becoming a surety for third party", "", "", Review},
		{"NSE", "Effect(s) on listed entity due to changed regulatory  framework applicable", "", "", Review},
		{"BSE", "Board Meeting", "Board Meeting", "", Skip},
		{"BSE", "Company Update", "Analyst / Investor Meet", "", Skip},
		{"BSE", "Company Update", "Newspaper Publication", "", Skip},
		{"BSE", "Insider Trading / SAST", "Closure of Trading Window", "", Skip},
		{"BSE", "AGM/EGM", "Postal Ballot", "", Skip},
		{"BSE", "AGM/EGM", "EGM", "", Skip},
		{"BSE", "Company Update", "Certificate under Reg. 74 (5) of SEBI (DP) Regulations, 2018", "", Skip},
		{"BSE", "Company Update", "Reg. 32 (1), (3) - Statement of Deviation & Variation", "", Skip},
		{"BSE", "Company Update", "Allotment of ESOP / ESPS", "", Skip},
		{"BSE", "Company Update", "Investor Presentation", "", Review},
		{"BSE", "Company Update", "Press Release / Media Release", "", Review},
		{"BSE", "Company Update", "General", "", Review},
		{"BSE", "Company Update", "Earnings Call Transcript", "", Review},
		{"BSE", "Others", "Outcome without intimation", "", Review},
		{"NSE", "Outcome of Board Meeting", "", "", Analyze},
		{"NSE", "Integrated Filing- Financial", "", "", Analyze},
		{"NSE", "Change in Management", "", "", Analyze},
		{"NSE", "Agreements", "", "", Analyze},
		{"NSE", "Acquisition", "", "", Analyze},
		{"NSE", "Appointment", "", "", Analyze},
		{"NSE", "Resignation of Director/KMP/SMP", "", "", Analyze},
		{"NSE", "Analysts/Institutional Investor Meet/Con. Call Updates", "", "Welspun Living Limited has informed the Exchange about Schedule of Investor meet", Skip},
		{"NSE", "Analysts/Institutional Investor Meet/Con. Call Updates", "", "Tata Motors Limited has informed the Exchange about Transcript", Review},
		{"NSE", "Copy of Newspaper Publication", "", "", Skip},
		{"NSE", "Trading Window", "", "", Skip},
		{"NSE", "General Updates", "", "", Review},
		{"NSE", "Updates", "", "", Review},
		{"NSE", "Press Release", "", "", Review},
		{"NSE", "Investor Presentation", "", "", Review},
		{"NSE", "Record Date", "", "", Review},
	}
	for _, c := range cases {
		got := Classify(announce.Announcement{Source: c.src, Category: c.cat, Subcategory: c.sub, Headline: c.head})
		if got.Decision != c.want {
			t.Errorf("%s %q / %q (%q): got %v (%s), want %v", c.src, c.cat, c.sub, c.head, got.Decision, got.Reason, c.want)
		}
	}
}

func TestUnknownCategoryIsReviewNotSkip(t *testing.T) {
	r := Classify(announce.Announcement{Category: "Brand New Category"})
	if r.Decision != Review || r.Reason != "unclassified" {
		t.Fatalf("unknown categories must not be silently dropped: %+v", r)
	}
}

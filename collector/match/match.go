// Package match recognises the same corporate event published on both exchanges, so it is analysed once
// (saving model quota) instead of twice. First version (Q-5): same company name after normalisation, a
// different exchange, and exchange times close together. It can wrongly merge two different filings by
// the same company within the window; the window is deliberately short.
package match

import (
	"strings"
	"time"
	"unicode"

	"stonks/announce"
)

// NormalizeCompany makes "Schaeffler India Ltd", "SCHAEFFLER INDIA LIMITED" and "Shah Investor's Home Ltd"
// comparable: lower case, "&" read as "and", punctuation removed, a trailing "limited"/"ltd" dropped.
func NormalizeCompany(name string) string {
	name = strings.ToLower(strings.ReplaceAll(name, "&", " and "))
	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	words := strings.Fields(b.String())
	if n := len(words); n > 1 && (words[n-1] == "limited" || words[n-1] == "ltd") {
		words = words[:n-1]
	}
	return strings.Join(words, " ")
}

// SameEvent reports whether a and b look like one event published on both exchanges.
func SameEvent(a, b announce.Announcement, window time.Duration) bool {
	if a.Source == b.Source || a.Company == "" || b.Company == "" {
		return false
	}
	d := a.DisseminatedAt.Sub(b.DisseminatedAt)
	if d < 0 {
		d = -d
	}
	return d <= window && NormalizeCompany(a.Company) == NormalizeCompany(b.Company)
}

// SameAttachment reports whether two filings point at the same attached document (same URL, or same BSE file
// name). Results and "outcome of board meeting" filings often share one PDF; analysing it twice gave
// inconsistent answers in the live run.
func SameAttachment(a, b announce.Announcement) bool {
	key := func(x announce.Announcement) string {
		if x.AttachmentURL != "" {
			return x.AttachmentURL
		}
		return x.AttachmentName
	}
	ka, kb := key(a), key(b)
	return ka != "" && ka == kb
}

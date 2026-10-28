// Package announce normalizes NSE and BSE announcement JSON into one struct.
// Field names come from real responses captured by the M1 spike (see DECISIONS.md section 12).
package announce

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Exchange timestamps carry no zone; they are Indian Standard Time.
// A fixed zone avoids needing tzdata on Windows.
var IST = time.FixedZone("IST", 5*3600+30*60)

type Announcement struct {
	Source         string          // "NSE" or "BSE"
	SourceID       string          // NSE seq_id, BSE NEWSID (unique per source)
	Symbol         string          // NSE symbol (empty for BSE until mapped)
	ScripCode      string          // BSE SCRIP_CD (empty for NSE until mapped)
	ISIN           string          // NSE sm_isin (BSE rows have none)
	Company        string
	Category       string          // NSE desc, BSE CATEGORYNAME
	Subcategory    string          // BSE SUBCATNAME (NSE has none)
	Headline       string          // BSE HEADLINE, NSE attchmntText (generic sentence)
	Subject        string          // BSE NEWSSUB
	AttachmentName string          // BSE file name only; NSE full URL in AttachmentURL
	AttachmentURL  string          // NSE attchmntFile; BSE base URL still unknown
	SubmittedAt    time.Time       // NSE an_dt, BSE News_submission_dt
	DisseminatedAt time.Time       // NSE exchdisstime, BSE DissemDT (BSE has ms resolution)
	Raw            json.RawMessage // untouched row JSON (D-023)
}

func parseNSETime(s string) (time.Time, error) {
	return time.ParseInLocation("02-Jan-2006 15:04:05", s, IST)
}

// BSE times may have 0 to 3 fractional digits; Go accepts a fractional
// second when parsing even though the layout does not mention it.
func parseBSETime(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02T15:04:05", s, IST)
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case float64:
		return fmt.Sprintf("%.0f", x)
	default:
		return fmt.Sprint(x)
	}
}

// ParseNSE parses the body of /api/corporate-announcements (a JSON array).
func ParseNSE(body []byte) ([]Announcement, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("nse: not a JSON array: %w", err)
	}
	var out []Announcement
	for _, raw := range rows {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("nse row: %w", err)
		}
		id := str(m["seq_id"])
		if id == "" {
			continue
		}
		pub, err := parseNSETime(str(m["exchdisstime"]))
		if err != nil {
			return nil, fmt.Errorf("nse %s exchdisstime: %w", id, err)
		}
		sub, err := parseNSETime(str(m["an_dt"]))
		if err != nil {
			sub = pub
		}
		out = append(out, Announcement{
			Source: "NSE", SourceID: id,
			Symbol: str(m["symbol"]), ISIN: str(m["sm_isin"]), Company: str(m["sm_name"]),
			Category: str(m["desc"]), Headline: str(m["attchmntText"]),
			AttachmentURL: str(m["attchmntFile"]),
			SubmittedAt:   sub, DisseminatedAt: pub, Raw: append(json.RawMessage(nil), raw...),
		})
	}
	return out, nil
}

// ParseBSE parses the body of AnnSubCategoryGetData ({"Table":[...],"Table1":[{"ROWCNT":n}]}).
// A day with no announcements yields zero rows and no error.
func ParseBSE(body []byte) ([]Announcement, error) {
	var env struct {
		Table []json.RawMessage `json:"Table"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("bse: not a JSON object: %w", err)
	}
	var out []Announcement
	for _, raw := range env.Table {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("bse row: %w", err)
		}
		id := str(m["NEWSID"])
		if id == "" {
			continue
		}
		pub, err := parseBSETime(str(m["DissemDT"]))
		if err != nil {
			return nil, fmt.Errorf("bse %s DissemDT: %w", id, err)
		}
		sub, err := parseBSETime(str(m["News_submission_dt"]))
		if err != nil {
			sub = pub
		}
		out = append(out, Announcement{
			Source: "BSE", SourceID: id,
			ScripCode: str(m["SCRIP_CD"]), Company: str(m["SLONGNAME"]),
			Category: str(m["CATEGORYNAME"]), Subcategory: str(m["SUBCATNAME"]),
			Headline: str(m["HEADLINE"]), Subject: str(m["NEWSSUB"]),
			AttachmentName: str(m["ATTACHMENTNAME"]),
			SubmittedAt:    sub, DisseminatedAt: pub, Raw: append(json.RawMessage(nil), raw...),
		})
	}
	return out, nil
}

// ParseBSEPage is ParseBSE plus the total page count the feed reports (0 when there are no rows).
func ParseBSEPage(body []byte) ([]Announcement, int, error) {
	rows, err := ParseBSE(body)
	if err != nil {
		return nil, 0, err
	}
	var env struct {
		Table []map[string]any `json:"Table"`
	}
	_ = json.Unmarshal(body, &env)
	total := 0
	for _, m := range env.Table {
		var n int
		if _, err := fmt.Sscanf(str(m["TotalPageCnt"]), "%d", &n); err == nil && n > total {
			total = n
		}
	}
	return rows, total, nil
}

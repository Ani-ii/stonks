// Package prices downloads and parses end-of-day price files: NSE and BSE
// "UDiFF" bhavcopy (all stocks for one day) and the NSE index close file.
//
// Parsing is header-driven, so a reordered file still works. If a file is
// missing a column, the error lists the columns it does have, so the format
// can be fixed from one real sample.
package prices

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	NSEBase = "https://nsearchives.nseindia.com"
	BSEBase = "https://www.bseindia.com"
)

// DayBar is one stock (or index) on one trading day.
type DayBar struct {
	Src                    string // NSE, BSE or IDX
	Code                   string // NSE symbol, BSE scrip code, or index name
	ISIN                   string
	Date                   string // YYYY-MM-DD
	Open, High, Low, Close float64
	Volume                 int64
}

// ErrNotFound means HTTP 404: weekend, holiday, or not published yet.
var ErrNotFound = errors.New("prices: file not found (holiday or not published yet)")

// ErrHTML means the server answered 200 with a web page instead of a data file. BSE does
// this for dates it has no file for; it could also be a block page, so callers decide.
var ErrHTML = errors.New("prices: server returned a web page instead of a file")

func looksHTML(b []byte) bool {
	h := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(string(b[:min(len(b), 64)]), "\xef\xbb\xbf")))
	return strings.HasPrefix(h, "<!doctype html") || strings.HasPrefix(h, "<html")
}

func NSEBhavURL(base string, d time.Time) string {
	return base + "/content/cm/BhavCopy_NSE_CM_0_0_0_" + d.Format("20060102") + "_F_0000.csv.zip"
}

func NSEIndexURL(base string, d time.Time) string {
	return base + "/content/indices/ind_close_all_" + d.Format("02012006") + ".csv"
}

// BSEBhavURL is UNVERIFIED (assumed from the UDiFF naming); check it with curl.
func BSEBhavURL(base string, d time.Time) string {
	return base + "/download/BhavCopy/Equity/BhavCopy_BSE_CM_0_0_0_" + d.Format("20060102") + "_F_0000.CSV"
}

type Client struct{ HTTP *http.Client }

// NewClient uses HTTP/1.1 only, as the collector does for NSE (D-040).
func NewClient() *Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSHandshakeTimeout: 15 * time.Second,
	}
	return &Client{HTTP: &http.Client{Transport: tr, Timeout: 60 * time.Second}}
}

func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	if strings.Contains(url, "bseindia.com") {
		req.Header.Set("Referer", "https://www.bseindia.com/")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if looksHTML(body) {
		return nil, ErrHTML
	}
	return body, nil
}

// unwrap returns the CSV text, unzipping first if the data is a zip file.
func unwrap(b []byte) ([]byte, error) {
	if len(b) >= 2 && b[0] == 'P' && b[1] == 'K' {
		zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if strings.HasSuffix(strings.ToLower(f.Name), ".csv") {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, 128<<20))
			}
		}
		return nil, errors.New("prices: zip file contains no csv")
	}
	return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), nil
}

// table holds a parsed CSV: column positions by lower-case name, original names, data rows.
type table struct {
	col   map[string]int
	names []string
	rows  [][]string
}

func readTable(data []byte) (*table, error) {
	raw, err := unwrap(data)
	if err != nil {
		return nil, err
	}
	r := csv.NewReader(bytes.NewReader(raw))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	recs, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(recs) < 2 {
		return nil, errors.New("prices: file has no data rows")
	}
	t := &table{col: map[string]int{}, rows: recs[1:]}
	for i, h := range recs[0] {
		h = strings.TrimSpace(h)
		t.names = append(t.names, h)
		t.col[strings.ToLower(h)] = i
	}
	return t, nil
}

func (t *table) need(what string, cols ...string) error {
	for _, c := range cols {
		if _, ok := t.col[c]; !ok {
			return fmt.Errorf("prices: %s file has no column %q; its columns are: %s", what, c, strings.Join(t.names, ", "))
		}
	}
	return nil
}

func (t *table) get(rec []string, name string) string {
	i, ok := t.col[name]
	if !ok || i >= len(rec) {
		return ""
	}
	return strings.TrimSpace(rec[i])
}

func num(s string) float64 {
	f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), ",", ""), 64)
	if err != nil {
		return 0
	}
	return f
}

var dateLayouts = []string{"2006-01-02", "02-01-2006", "02-Jan-2006", "2006/01/02", "02/01/2006", "02-Jan-06", "20060102"}

func parseDate(s string) (string, error) {
	s = strings.TrimSpace(s)
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Format("2006-01-02"), nil
		}
	}
	return "", fmt.Errorf("prices: cannot read date %q", s)
}

// ParseUDiFF reads an NSE or BSE common bhavcopy (src "NSE" or "BSE").
// NSE rows are keyed by symbol (TckrSymb), BSE rows by scrip code (FinInstrmId).
func ParseUDiFF(data []byte, src string) ([]DayBar, error) {
	t, err := readTable(data)
	if err != nil {
		return nil, err
	}
	codeCol := "tckrsymb"
	if src == "BSE" {
		codeCol = "fininstrmid"
	}
	if err := t.need(src+" bhavcopy", codeCol, "traddt", "opnpric", "hghpric", "lwpric", "clspric"); err != nil {
		return nil, err
	}
	idx := map[string]int{}
	var out []DayBar
	for _, rec := range t.rows {
		code := t.get(rec, codeCol)
		o, h, l, c := num(t.get(rec, "opnpric")), num(t.get(rec, "hghpric")), num(t.get(rec, "lwpric")), num(t.get(rec, "clspric"))
		if code == "" || o <= 0 || c <= 0 {
			continue
		}
		d, err := parseDate(t.get(rec, "traddt"))
		if err != nil {
			return nil, err
		}
		b := DayBar{Src: src, Code: code, ISIN: t.get(rec, "isin"), Date: d, Open: o, High: h, Low: l, Close: c,
			Volume: int64(num(t.get(rec, "ttltradgvol")))}
		if i, dup := idx[code]; dup {
			if strings.EqualFold(t.get(rec, "sctysrs"), "EQ") {
				out[i] = b // prefer the main EQ series if a symbol appears twice
			}
			continue
		}
		idx[code] = len(out)
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, errors.New("prices: " + src + " bhavcopy has no usable price rows")
	}
	return out, nil
}

// ParseNSEIndex returns one index (for example "Nifty 50") from the NSE index close file.
func ParseNSEIndex(data []byte, name string) (DayBar, error) {
	t, err := readTable(data)
	if err != nil {
		return DayBar{}, err
	}
	if err := t.need("index", "index name", "index date", "open index value", "high index value", "low index value", "closing index value"); err != nil {
		return DayBar{}, err
	}
	var seen []string
	for _, rec := range t.rows {
		n := t.get(rec, "index name")
		seen = append(seen, n)
		if !strings.EqualFold(n, strings.TrimSpace(name)) {
			continue
		}
		d, err := parseDate(t.get(rec, "index date"))
		if err != nil {
			return DayBar{}, err
		}
		b := DayBar{Src: "IDX", Code: n, Date: d, Open: num(t.get(rec, "open index value")), High: num(t.get(rec, "high index value")),
			Low: num(t.get(rec, "low index value")), Close: num(t.get(rec, "closing index value")), Volume: int64(num(t.get(rec, "volume")))}
		if b.Close <= 0 || b.Open <= 0 {
			return DayBar{}, fmt.Errorf("prices: index %q has no usable values", name)
		}
		return b, nil
	}
	sort.Strings(seen)
	if len(seen) > 12 {
		seen = seen[:12]
	}
	return DayBar{}, fmt.Errorf("prices: index %q not found; some names in the file: %s", name, strings.Join(seen, ", "))
}

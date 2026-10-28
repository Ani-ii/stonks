// M1 spike: fetch announcements from BSE and NSE for 5 stocks and report
// fields, timestamps, latency and blocking behaviour. Standard library only.
//
// NOTE: endpoint URLs and parameter names are from memory of the unofficial
// endpoints and are UNVERIFIED. This spike exists to find out. If a source
// fails, read the printed status/body and adjust the constants below.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type stock struct{ NSE, BSE string }

var stocks = []stock{
	{"RELIANCE", "500325"}, {"TCS", "532540"}, {"INFY", "500209"},
	{"HDFCBANK", "500180"}, {"SBIN", "500112"},
}

const (
	ua      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"
	bseRef  = "https://www.bseindia.com/"
	// v2: endpoint and headers copied from the open-source `bse` Python library (v3.3.4).
	// v1 used AnnGetData and got 403. strScrip empty = exchange-wide feed (all stocks).
	bseURL  = "https://api.bseindia.com/BseIndiaAPI/api/AnnSubCategoryGetData/w?pageno=1&strCat=-1&subcategory=-1&strPrevDate=%s&strToDate=%s&strSearch=P&strScrip=%s&strType=C"
	nseHome = "https://www.nseindia.com"
	nseAll  = "https://www.nseindia.com/api/corporate-announcements?index=equities"
	nseURL  = "https://www.nseindia.com/api/corporate-announcements?index=equities&symbol=%s&from_date=%s&to_date=%s"
)

func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, Timeout: 20 * time.Second}
}

func get(c *http.Client, url, referer, origin string) (int, []byte, time.Duration, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, nil, 0, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	if origin != "" { // BSE checks these (as the Python library sends them)
		req.Header.Set("Origin", origin)
		req.Header.Set("Sec-Fetch-Site", "same-site")
		req.Header.Set("Connection", "keep-alive")
	}
	t0 := time.Now()
	resp, err := c.Do(req) // Go handles gzip automatically when we don't set Accept-Encoding
	if err != nil {
		return 0, nil, time.Since(t0), err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, time.Since(t0), err
}

// findRows locates the first array of JSON objects anywhere in the payload.
func findRows(v any) []map[string]any {
	switch x := v.(type) {
	case []any:
		var out []map[string]any
		for _, e := range x {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		if len(out) > 0 {
			return out
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if r := findRows(x[k]); len(r) > 0 {
				return r
			}
		}
	}
	return nil
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

func isTimeKey(k string) bool {
	l := strings.ToLower(k)
	return strings.Contains(l, "dt") || strings.Contains(l, "date") || strings.Contains(l, "time")
}

func report(src, sym string, status int, d time.Duration, body []byte, err error, dir string) {
	fmt.Printf("\n[%s %s] ", src, sym)
	if err != nil {
		fmt.Printf("ERROR after %v: %v\n", d.Round(time.Millisecond), err)
		return
	}
	fmt.Printf("status=%d time=%v bytes=%d\n", status, d.Round(time.Millisecond), len(body))
	_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s_%s.json", src, sym)), body, 0o644)
	var v any
	if json.Unmarshal(body, &v) != nil {
		fmt.Printf("  not JSON (blocked or wrong URL?): %s\n", snippet(body))
		return
	}
	rows := findRows(v)
	fmt.Printf("  rows=%d\n", len(rows))
	if len(rows) == 0 {
		fmt.Printf("  payload: %s\n", snippet(body))
		return
	}
	keys := make([]string, 0, len(rows[0]))
	for k := range rows[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("  fields: %s\n", strings.Join(keys, ", "))
	for i, r := range rows {
		if i >= 3 {
			break
		}
		var parts []string
		for _, k := range keys {
			if isTimeKey(k) {
				parts = append(parts, fmt.Sprintf("%s=%v", k, r[k]))
			}
		}
		fmt.Printf("  row%d time fields: %s\n", i+1, strings.Join(parts, " | "))
	}
}

func main() {
	days := flag.Int("days", 7, "look back this many days")
	out := flag.String("out", "out", "directory for raw JSON")
	repeat := flag.Int("repeat", 0, "extra polling test: repeat N requests per source (checks blocking/rate limits)")
	interval := flag.Duration("interval", 3*time.Second, "delay between repeated requests")
	wide := flag.Bool("wide", false, "also test the exchange-wide latest-announcements feeds (no symbol)")
	flag.Parse()
	_ = os.MkdirAll(*out, 0o755)

	to := time.Now()
	from := to.AddDate(0, 0, -*days)
	bc, nc := newClient(), newClient()

	// NSE needs cookies from the home page before the API answers.
	st, _, d, err := get(nc, nseHome, "", "")
	fmt.Printf("NSE warm-up: status=%d time=%v err=%v\n", st, d.Round(time.Millisecond), err)

	for _, s := range stocks {
		u := fmt.Sprintf(bseURL, from.Format("20060102"), to.Format("20060102"), s.BSE)
		st, b, d, err := get(bc, u, bseRef, bseRef)
		report("BSE", s.NSE, st, d, b, err, *out)

		u = fmt.Sprintf(nseURL, s.NSE, from.Format("02-01-2006"), to.Format("02-01-2006"))
		st, b, d, err = get(nc, u, nseHome+"/companies-listing/corporate-filings-announcements", "")
		report("NSE", s.NSE, st, d, b, err, *out)
	}

	if *wide {
		fmt.Println("\n== exchange-wide feed (all stocks, no symbol filter) ==")
		today := to.Format("20060102")
		st, b, d, err := get(bc, fmt.Sprintf(bseURL, today, today, ""), bseRef, bseRef)
		report("BSE", "ALL", st, d, b, err, *out)
		st, b, d, err = get(nc, nseAll, nseHome+"/companies-listing/corporate-filings-announcements", "")
		report("NSE", "ALL", st, d, b, err, *out)
	}

	if *repeat > 0 {
		fmt.Printf("\n== polling test: %d requests per source, every %v ==\n", *repeat, *interval)
		s := stocks[0]
		counts := map[string]map[int]int{"BSE": {}, "NSE": {}}
		var lat = map[string][]time.Duration{}
		for i := 0; i < *repeat; i++ {
			u := fmt.Sprintf(bseURL, from.Format("20060102"), to.Format("20060102"), s.BSE)
			st, _, d, _ := get(bc, u, bseRef, bseRef)
			counts["BSE"][st]++
			lat["BSE"] = append(lat["BSE"], d)
			u = fmt.Sprintf(nseURL, s.NSE, from.Format("02-01-2006"), to.Format("02-01-2006"))
			st, _, d, _ = get(nc, u, nseHome, "")
			counts["NSE"][st]++
			lat["NSE"] = append(lat["NSE"], d)
			time.Sleep(*interval)
		}
		for _, src := range []string{"BSE", "NSE"} {
			l := lat[src]
			sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
			fmt.Printf("%s status counts=%v median latency=%v max=%v\n", src, counts[src], l[len(l)/2].Round(time.Millisecond), l[len(l)-1].Round(time.Millisecond))
		}
	}
	fmt.Println("\nDone. Raw responses saved in", *out, "- paste this output back into the chat.")
}

// Command probe checks whether the NSE/BSE feeds are served from a cache, by logging response
// time, caching headers and a body hash on every poll.
//
//	go run ./cmd/probe -interval 3s -duration 2m
package main

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"stonks/announce"
	"stonks/fetch"
)

type result struct {
	body []byte
	hdr  http.Header
	err  error
	took time.Duration
}

func main() {
	interval := flag.Duration("interval", 3*time.Second, "delay between polls")
	duration := flag.Duration("duration", 2*time.Minute, "how long to probe")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	c := fetch.New()
	end := time.Now().Add(*duration)
	dates := map[string]map[string]bool{"NSE": {}, "BSE": {}}
	hashes := map[string]map[string]bool{"NSE": {}, "BSE": {}}
	var lastHash = map[string]string{}
	var speeds = map[string][]time.Duration{}
	polls := 0
	for time.Now().Before(end) && ctx.Err() == nil {
		polls++
		for _, src := range []string{"NSE", "BSE"} {
			t0 := time.Now()
			var r result
			if src == "NSE" {
				r.body, r.hdr, r.err = c.ProbeNSE(ctx)
			} else {
				now := time.Now().In(announce.IST)
				r.body, r.hdr, r.err = c.ProbeBSE(ctx, now, now)
			}
			r.took = time.Since(t0)
			if r.err != nil {
				fmt.Printf("%s %s ERROR %v\n", time.Now().Format("15:04:05"), src, r.err)
				continue
			}
			h := fmt.Sprintf("%x", sha256.Sum256(r.body))[:8]
			newest := "-"
			if src == "NSE" {
				if rows, err := announce.ParseNSE(r.body); err == nil && len(rows) > 0 {
					newest = rows[0].DisseminatedAt.In(announce.IST).Format("15:04:05")
				}
			} else if rows, _, err := announce.ParseBSEPage(r.body); err == nil && len(rows) > 0 {
				newest = rows[0].DisseminatedAt.In(announce.IST).Format("15:04:05.000")
			}
			var hp []string
			for _, k := range []string{"Date", "Age", "Cache-Control", "Expires", "Last-Modified", "X-Cache", "Via", "Server"} {
				if v := r.hdr.Get(k); v != "" {
					hp = append(hp, k+"="+v)
				}
			}
			dates[src][r.hdr.Get("Date")] = true
			hashes[src][h] = true
			speeds[src] = append(speeds[src], r.took)
			mark := ""
			if lastHash[src] != "" && lastHash[src] != h {
				mark = "  <-- BODY CHANGED"
			}
			lastHash[src] = h
			fmt.Printf("%s %s %4dms hash=%s newest=%s %s%s\n", time.Now().Format("15:04:05"), src, r.took.Milliseconds(), h, newest, strings.Join(hp, " "), mark)
		}
		select {
		case <-ctx.Done():
		case <-time.After(*interval):
		}
	}
	fmt.Printf("\nSUMMARY after %d polls per source (interval %v):\n", polls, *interval)
	for _, src := range []string{"NSE", "BSE"} {
		l := speeds[src]
		sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
		med := time.Duration(0)
		if len(l) > 0 {
			med = l[len(l)/2]
		}
		fmt.Printf("  %s: distinct Date header values=%d, distinct body hashes=%d, median response=%v\n", src, len(dates[src]), len(hashes[src]), med.Round(time.Millisecond))
	}
	fmt.Println("If distinct Date values is far fewer than the number of polls, responses are being cached.")
}

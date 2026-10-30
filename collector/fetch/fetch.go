// Package fetch talks to the unofficial NSE and BSE announcement endpoints.
// Endpoints, parameters and headers were verified by the M1 spike (DECISIONS.md section 12).
package fetch

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync/atomic"
	"time"

	"crypto/tls"

	"stonks/announce"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36"

// recycleEvery closes idle connections after this many requests (cheap insurance against stale connections).
const recycleEvery = 200

type Client struct {
	HTTP    *http.Client // BSE (HTTP/2 allowed; it has worked fine)
	NSEHTTP *http.Client // NSE: HTTP/1.1 only (D-040)
	NSEBase string       // https://www.nseindia.com
	BSEBase string       // https://api.bseindia.com
	reqs    atomic.Int64
}

func newHTTP(http11Only bool) *http.Client {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   !http11Only,
	}
	if http11Only {
		// A non-nil, empty TLSNextProto map switches HTTP/2 off, so ALPN negotiates HTTP/1.1.
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &http.Client{Jar: jar, Timeout: 8 * time.Second, Transport: tr}
}

// NewHTTPClient exposes the hardened client builder (HTTP/1.1 only when http11Only) for other packages,
// for example the attachment downloader.
func NewHTTPClient(http11Only bool) *http.Client { return newHTTP(http11Only) }

func New() *Client {
	return &Client{
		HTTP:    newHTTP(false),
		NSEHTTP: newHTTP(true),
		NSEBase: "https://www.nseindia.com",
		BSEBase: "https://api.bseindia.com",
	}
}

// StatusError is returned for any non-200 answer (403 means blocked).
type StatusError struct {
	Code    int
	Snippet string
}

func (e *StatusError) Error() string { return fmt.Sprintf("http %d: %s", e.Code, e.Snippet) }

func (c *Client) get(ctx context.Context, hc *http.Client, url string, headers map[string]string) ([]byte, error) {
	body, _, err := c.getH(ctx, hc, url, headers)
	return body, err
}

// getH is get plus the response headers (used by the probe to inspect caching).
// It recycles connections periodically and retries once on a fresh connection after a transport error
// (not after an HTTP status error such as 403, which retrying would not fix).
func (c *Client) getH(ctx context.Context, hc *http.Client, url string, headers map[string]string) ([]byte, http.Header, error) {
	if c.reqs.Add(1)%recycleEvery == 0 {
		c.HTTP.CloseIdleConnections()
		c.NSEHTTP.CloseIdleConnections()
	}
	body, hdr, err := c.doGet(ctx, hc, url, headers)
	if err != nil && ctx.Err() == nil {
		if _, isStatus := err.(*StatusError); !isStatus {
			hc.CloseIdleConnections()
			body, hdr, err = c.doGet(ctx, hc, url, headers)
		}
	}
	return body, hdr, err
}

func (c *Client) doGet(ctx context.Context, hc *http.Client, url string, headers map[string]string) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		s := strings.Join(strings.Fields(string(body)), " ")
		if len(s) > 120 {
			s = s[:120]
		}
		return nil, resp.Header, &StatusError{Code: resp.StatusCode, Snippet: s}
	}
	return body, resp.Header, nil
}

func (c *Client) nseReq() (string, map[string]string) {
	return c.NSEBase + "/api/corporate-announcements?index=equities",
		map[string]string{"Referer": c.NSEBase + "/companies-listing/corporate-filings-announcements"}
}

func (c *Client) bseReq(from, to time.Time, page int) (string, map[string]string) {
	url := fmt.Sprintf("%s/BseIndiaAPI/api/AnnSubCategoryGetData/w?pageno=%d&strCat=-1&subcategory=-1&strPrevDate=%s&strToDate=%s&strSearch=P&strScrip=&strType=C",
		c.BSEBase, page, from.In(announce.IST).Format("20060102"), to.In(announce.IST).Format("20060102"))
	return url, map[string]string{
		"Origin":         "https://www.bseindia.com/",
		"Referer":        "https://www.bseindia.com/",
		"Sec-Fetch-Site": "same-site",
		"Connection":     "keep-alive",
	}
}

// NSELatest returns the newest announcements across all stocks (the feed appeared to cap at 20 rows).
// On 401/403 it visits the home page once to refresh cookies and retries.
func (c *Client) NSELatest(ctx context.Context) ([]announce.Announcement, error) {
	url, h := c.nseReq()
	body, err := c.get(ctx, c.NSEHTTP, url, h)
	if se, ok := err.(*StatusError); ok && (se.Code == 401 || se.Code == 403) {
		_, _ = c.get(ctx, c.NSEHTTP, c.NSEBase, nil) // warm-up; its own status is ignored (it returned 403 once while the API still worked)
		body, err = c.get(ctx, c.NSEHTTP, url, h)
	}
	if err != nil {
		return nil, err
	}
	return announce.ParseNSE(body)
}

// BSEPage returns one page of BSE announcements (newest first) for the date range, all stocks.
// It also returns the total page count reported by BSE.
func (c *Client) BSEPage(ctx context.Context, from, to time.Time, page int) ([]announce.Announcement, int, error) {
	url, h := c.bseReq(from, to, page)
	body, err := c.get(ctx, c.HTTP, url, h)
	if err != nil {
		return nil, 0, err
	}
	return announce.ParseBSEPage(body)
}

// ProbeNSE and ProbeBSE return the raw body and response headers (for cache diagnostics).
func (c *Client) ProbeNSE(ctx context.Context) ([]byte, http.Header, error) {
	url, h := c.nseReq()
	return c.getH(ctx, c.NSEHTTP, url, h)
}

func (c *Client) ProbeBSE(ctx context.Context, from, to time.Time) ([]byte, http.Header, error) {
	url, h := c.bseReq(from, to, 1)
	return c.getH(ctx, c.HTTP, url, h)
}

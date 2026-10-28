package fetch

import (
	"crypto/tls"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"stonks/announce"
)

func sample(t *testing.T, name string) []byte {
	b, err := os.ReadFile("../announce/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNSELatestAndBSEPage(t *testing.T) {
	var bseQuery, bseOrigin, nseRef string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/corporate-announcements"):
			nseRef = r.Header.Get("Referer")
			w.Write(sample(t, "nse_sample.json"))
		case strings.HasPrefix(r.URL.Path, "/BseIndiaAPI/api/AnnSubCategoryGetData/w"):
			bseQuery = r.URL.RawQuery
			bseOrigin = r.Header.Get("Origin")
			w.Write(sample(t, "bse_sample.json"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New()
	c.NSEBase, c.BSEBase = srv.URL, srv.URL

	n, err := c.NSELatest(context.Background())
	if err != nil || len(n) != 2 || !strings.Contains(nseRef, "corporate-filings-announcements") {
		t.Fatalf("nse err=%v rows=%d ref=%q", err, len(n), nseRef)
	}
	day := time.Date(2026, 10, 25, 3, 0, 0, 0, announce.IST)
	b, total, err := c.BSEPage(context.Background(), day.AddDate(0, 0, -1), day, 2)
	if err != nil || len(b) != 2 || total != 1 {
		t.Fatalf("bse err=%v rows=%d total=%d", err, len(b), total)
	}
	for _, want := range []string{"pageno=2", "strPrevDate=20261024", "strToDate=20261025", "strScrip=&"} {
		if !strings.Contains(bseQuery, want) {
			t.Errorf("query %q missing %q", bseQuery, want)
		}
	}
	if bseOrigin != "https://www.bseindia.com/" {
		t.Errorf("origin header %q", bseOrigin)
	}
}

func TestBlockedIsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte("<HTML>Access Denied</HTML>"))
	}))
	defer srv.Close()
	c := New()
	c.NSEBase, c.BSEBase = srv.URL, srv.URL
	_, _, err := c.BSEPage(context.Background(), time.Now(), time.Now(), 1)
	se, ok := err.(*StatusError)
	if !ok || se.Code != 403 || !strings.Contains(se.Snippet, "Access Denied") {
		t.Fatalf("want 403 StatusError, got %v", err)
	}
	if _, err := c.NSELatest(context.Background()); err == nil {
		t.Fatal("NSE should fail after retry on 403")
	}
}

func TestRetriesOnceAfterTransportError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 { // simulate a reset connection: close without any response
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.Write(sample(t, "nse_sample.json"))
	}))
	defer srv.Close()
	c := New()
	c.NSEBase = srv.URL
	rows, err := c.NSELatest(context.Background())
	if err != nil || len(rows) != 2 || calls != 2 {
		t.Fatalf("err=%v rows=%d calls=%d (want success after exactly one retry)", err, len(rows), calls)
	}
}

func TestNoRetryOnStatusError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(403)
	}))
	defer srv.Close()
	c := New()
	c.BSEBase = srv.URL
	if _, _, err := c.BSEPage(context.Background(), time.Now(), time.Now(), 1); err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d (a 403 must not be retried)", err, calls)
	}
}

// The NSE client must speak HTTP/1.1 even when the server offers HTTP/2 (D-040).
func TestNSEClientUsesHTTP11(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(r.Proto))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	c := New()
	tr := c.NSEHTTP.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // test server uses a self-signed certificate
	body, _, err := c.getH(context.Background(), c.NSEHTTP, srv.URL, nil)
	if err != nil || string(body) != "HTTP/1.1" {
		t.Fatalf("NSE client protocol = %q err=%v, want HTTP/1.1", body, err)
	}
	// Control: the BSE client is allowed HTTP/2.
	tr2 := c.HTTP.Transport.(*http.Transport)
	tr2.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	body, _, err = c.getH(context.Background(), c.HTTP, srv.URL, nil)
	if err != nil || string(body) != "HTTP/2.0" {
		t.Fatalf("BSE client protocol = %q err=%v, want HTTP/2.0", body, err)
	}
}

package prices

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const nseSample = `TradDt,BizDt,Sgmt,Src,FinInstrmTp,FinInstrmId,ISIN,TckrSymb,SctySrs,OpnPric,HghPric,LwPric,ClsPric,LastPric,PrvsClsgPric,TtlTradgVol
2026-10-28,2026-10-28,CM,NSE,STK,1,INE001A01036,ACME,EQ,110.00,113.00,109.50,112.00,112.00,100.00,"3,000"
2026-10-28,2026-10-28,CM,NSE,STK,2,INE002A01018,BETA,EQ,50.00,51.00,49.00,50.50,50.50,50.00,900
2026-10-28,2026-10-28,CM,NSE,STK,3,INE003A01011,BETA,BE,49.00,49.50,48.00,48.50,48.50,50.00,10
2026-10-28,2026-10-28,CM,NSE,STK,4,INE004A01010,DEAD,EQ,0,0,0,0,0,10.00,0
`

const bseSample = `TradDt,BizDt,Sgmt,Src,FinInstrmTp,FinInstrmId,ISIN,TckrSymb,SctySrs,OpnPric,HghPric,LwPric,ClsPric,TtlTradgVol
28-Oct-2026,28-Oct-2026,CM,BSE,STK,532540,INE467B01029,TCS,A,200.5,210,199,205,12345
`

const indexSample = `Index Name,Index Date,Open Index Value,High Index Value,Low Index Value,Closing Index Value,Points Change,Change(%),Volume
Nifty 50,28-10-2026,24000.10,24100.00,23900.50,24050.25,50.15,0.21,250000000
Nifty Bank,28-10-2026,52000,52500,51800,52300,300,0.58,0
`

func zipped(t *testing.T, name, content string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(content))
	zw.Close()
	return buf.Bytes()
}

func TestParseNSEZip(t *testing.T) {
	bars, err := ParseUDiFF(zipped(t, "BhavCopy.csv", nseSample), "NSE")
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 { // DEAD skipped (no prices), BETA kept once
		t.Fatalf("got %d bars: %+v", len(bars), bars)
	}
	a := bars[0]
	if a.Code != "ACME" || a.ISIN != "INE001A01036" || a.Date != "2026-10-28" || a.Open != 110 || a.Close != 112 || a.Volume != 3000 {
		t.Fatalf("bad bar: %+v", a)
	}
	if bars[1].Code != "BETA" || bars[1].Close != 50.5 { // EQ series preferred over BE
		t.Fatalf("EQ series not preferred: %+v", bars[1])
	}
}

func TestParseBSEPlainCSV(t *testing.T) {
	bars, err := ParseUDiFF([]byte(bseSample), "BSE")
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 1 || bars[0].Code != "532540" || bars[0].Date != "2026-10-28" || bars[0].High != 210 {
		t.Fatalf("%+v", bars)
	}
}

func TestMissingColumnListsColumns(t *testing.T) {
	_, err := ParseUDiFF([]byte("A,B,C\n1,2,3\n"), "NSE")
	if err == nil || !strings.Contains(err.Error(), "A, B, C") {
		t.Fatalf("error should list the file's columns, got %v", err)
	}
}

func TestParseIndex(t *testing.T) {
	b, err := ParseNSEIndex([]byte(indexSample), "nifty 50")
	if err != nil {
		t.Fatal(err)
	}
	if b.Src != "IDX" || b.Date != "2026-10-28" || b.Open != 24000.10 || b.Close != 24050.25 {
		t.Fatalf("%+v", b)
	}
	if _, err := ParseNSEIndex([]byte(indexSample), "Nifty Midcap 150"); err == nil || !strings.Contains(err.Error(), "Nifty Bank") {
		t.Fatalf("want not-found error naming available indices, got %v", err)
	}
}

func TestClientStatusHandling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("hello"))
		case "/missing":
			http.NotFound(w, r)
		case "/page":
			w.Write([]byte("<!DOCTYPE html><html><head></head><body>site</body></html>"))
		default:
			http.Error(w, "boom", 500)
		}
	}))
	defer srv.Close()
	c := NewClient()
	if b, err := c.Get(context.Background(), srv.URL+"/ok"); err != nil || string(b) != "hello" {
		t.Fatalf("ok: %q %v", b, err)
	}
	if _, err := c.Get(context.Background(), srv.URL+"/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 should be ErrNotFound, got %v", err)
	}
	if _, err := c.Get(context.Background(), srv.URL+"/page"); !errors.Is(err, ErrHTML) {
		t.Fatalf("html page should be ErrHTML, got %v", err)
	}
	if _, err := c.Get(context.Background(), srv.URL+"/x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("500 should be a plain error, got %v", err)
	}
}

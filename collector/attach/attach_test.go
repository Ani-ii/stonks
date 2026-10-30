package attach

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"stonks/announce"
)

func need(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed (sudo apt install poppler-utils)")
	}
}

func read(t *testing.T, name string) []byte {
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestURL(t *testing.T) {
	f := New("")
	if got := f.URL(announce.Announcement{AttachmentURL: "https://nsearchives.nseindia.com/corporate/x.pdf"}); got != "https://nsearchives.nseindia.com/corporate/x.pdf" {
		t.Errorf("NSE url %q", got)
	}
	if got := f.URL(announce.Announcement{AttachmentName: "abc.pdf"}); got != f.BSEBase+"abc.pdf" {
		t.Errorf("BSE url %q", got)
	}
	if f.URL(announce.Announcement{}) != "" {
		t.Error("no attachment must give an empty url")
	}
	if _, err := f.Text(context.Background(), announce.Announcement{}); !errors.Is(err, ErrNoAttachment) {
		t.Errorf("want ErrNoAttachment, got %v", err)
	}
}

func TestTextExtractionAndCache(t *testing.T) {
	need(t)
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(read(t, "results.pdf"))
	}))
	defer srv.Close()
	f := New(t.TempDir())
	a := announce.Announcement{AttachmentURL: srv.URL + "/r.pdf"}
	text, err := f.Text(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Revenue from operations", "1,234.5 crore", "interim dividend", "Order book"} {
		if !strings.Contains(text, want) {
			t.Errorf("extracted text missing %q:\n%s", want, text)
		}
	}
	again, err := f.Text(context.Background(), a)
	if err != nil || again != text || hits != 1 {
		t.Fatalf("second call must come from the cache: hits=%d err=%v", hits, err)
	}
}

func TestNotPDFAndHTTPErrorAndNoText(t *testing.T) {
	need(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>Access Denied</html>")) })
	mux.HandleFunc("/404", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	mux.HandleFunc("/blank", func(w http.ResponseWriter, r *http.Request) { w.Write(read(t, "blank.pdf")) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) { w.Write(read(t, "results.pdf")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	f := New("")
	get := func(path string) error {
		_, err := f.Text(context.Background(), announce.Announcement{AttachmentURL: srv.URL + path})
		return err
	}
	if err := get("/html"); !errors.Is(err, ErrNotPDF) {
		t.Errorf("html: want ErrNotPDF, got %v", err)
	}
	if err := get("/404"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: got %v", err)
	}
	if err := get("/blank"); !errors.Is(err, ErrNoText) {
		t.Errorf("blank pdf: want ErrNoText, got %v", err)
	}
	f.MaxBytes = 100
	if err := get("/big"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("big: want ErrTooLarge, got %v", err)
	}
}

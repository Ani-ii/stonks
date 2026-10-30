// Package attach downloads a filing's attached PDF and extracts its text (D-025), so the analyzer can read
// the actual numbers instead of only the headline. Text is cached on disk by URL, so a filing seen on both
// exchanges or analysed twice is downloaded once. Extraction uses the poppler `pdftotext` program
// (install with: sudo apt install poppler-utils).
package attach

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"stonks/announce"
	"stonks/fetch"
)

var (
	ErrNoAttachment = errors.New("filing has no attachment")
	ErrNotPDF       = errors.New("attachment is not a PDF")
	ErrTooLarge     = errors.New("attachment too large")
	ErrNoText       = errors.New("PDF has no extractable text (probably a scan)")
)

type Fetcher struct {
	HTTP     *http.Client
	CacheDir string // empty disables the cache
	MaxBytes int64  // download limit (default 15 MB)
	MinChars int    // fewer extracted characters than this counts as "no text" (default 200)
	// BSEBase is where BSE attachment file names live. UNVERIFIED: taken from memory of the public site, to be
	// checked with a real attachment name (DECISIONS.md Q-7).
	BSEBase string
	Extract func(ctx context.Context, pdf []byte) (string, error)
}

func New(cacheDir string) *Fetcher {
	h := fetch.NewHTTPClient(true) // HTTP/1.1, as for NSE (D-040)
	h.Timeout = 30 * time.Second
	return &Fetcher{HTTP: h, CacheDir: cacheDir, MaxBytes: 15 << 20, MinChars: 200,
		BSEBase: "https://www.bseindia.com/xml-data/corpfiling/AttachLive/", Extract: PDFToText}
}

// URL returns where the attachment lives: NSE rows carry a full URL, BSE rows only a file name.
func (f *Fetcher) URL(a announce.Announcement) string {
	if a.AttachmentURL != "" {
		return a.AttachmentURL
	}
	if a.AttachmentName != "" {
		return f.BSEBase + a.AttachmentName
	}
	return ""
}

func (f *Fetcher) cachePath(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(f.CacheDir, hex.EncodeToString(sum[:])+".txt")
}

// Text returns the extracted attachment text, from the cache when possible.
func (f *Fetcher) Text(ctx context.Context, a announce.Announcement) (string, error) {
	url := f.URL(a)
	if url == "" {
		return "", ErrNoAttachment
	}
	if f.CacheDir != "" {
		if b, err := os.ReadFile(f.cachePath(url)); err == nil {
			return string(b), nil
		}
	}
	pdf, err := f.download(ctx, url)
	if err != nil {
		return "", err
	}
	text, err := f.Extract(ctx, pdf)
	if err != nil {
		return "", err
	}
	text = strings.TrimSpace(text)
	if len(text) < f.MinChars {
		return "", ErrNoText
	}
	if f.CacheDir != "" {
		if err := os.MkdirAll(f.CacheDir, 0o755); err == nil {
			_ = os.WriteFile(f.cachePath(url), []byte(text), 0o644)
		}
	}
	return text, nil
}

func (f *Fetcher) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/pdf,*/*")
	if strings.Contains(url, "bseindia") {
		req.Header.Set("Referer", "https://www.bseindia.com/")
	} else {
		req.Header.Set("Referer", "https://www.nseindia.com/")
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attachment http %d", resp.StatusCode)
	}
	limit := f.MaxBytes
	if limit <= 0 {
		limit = 15 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, ErrTooLarge
	}
	if !bytes.HasPrefix(body, []byte("%PDF")) {
		return nil, ErrNotPDF
	}
	return body, nil
}

// PDFToText runs pdftotext, keeping the table layout so numbers stay next to their labels.
func PDFToText(ctx context.Context, pdf []byte) (string, error) {
	cmd := exec.CommandContext(ctx, "pdftotext", "-layout", "-enc", "UTF-8", "-", "-")
	cmd.Stdin = bytes.NewReader(pdf)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", errors.New("pdftotext not found: sudo apt install poppler-utils")
		}
		return "", fmt.Errorf("pdftotext failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

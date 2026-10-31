package analyzer

import (
	"context"
	"strings"
	"testing"
	"time"

	"stonks/announce"
)

func sampleInput() Input {
	return Input{Filing: announce.Announcement{
		Source: "BSE", SourceID: "1", ScripCode: "520151", Company: "Transworld Shipping Lines Ltd",
		Category: "Company Update", Subcategory: "Acquisition", Headline: "Signing of Memorandum of Agreement",
		DisseminatedAt: time.Date(2026, 10, 25, 0, 44, 40, 0, announce.IST),
	}}
}

func TestBuildPromptContainsFieldsAndIST(t *testing.T) {
	p := BuildPrompt(sampleInput(), 0)
	for _, want := range []string{"Exchange: BSE", "Transworld Shipping Lines Ltd", "BSE scrip code: 520151",
		"Published (IST): 2026-10-25 00:44:40", "Subcategory: Acquisition", "Headline: Signing of Memorandum", "No attached document text"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestBuildPromptTruncatesAttachmentText(t *testing.T) {
	in := sampleInput()
	in.Text = strings.Repeat("x", 500)
	p := BuildPrompt(in, 100)
	if !strings.Contains(p, "[text truncated]") || strings.Count(p, "x") > 120 {
		t.Fatalf("attachment text not truncated:\n%s", p)
	}
}

func TestValidate(t *testing.T) {
	good := Signal{Direction: " UP ", Confidence: 0.6, Materiality: "High", Reason: " r "}
	s, err := Validate(good)
	if err != nil || s.Direction != "up" || s.Materiality != "high" || s.Reason != "r" {
		t.Fatalf("good signal rejected or not normalised: %+v %v", s, err)
	}
	for name, bad := range map[string]Signal{
		"direction":   {Direction: "sideways", Confidence: 0.5, Materiality: "low", Reason: "r"},
		"materiality": {Direction: "up", Confidence: 0.5, Materiality: "huge", Reason: "r"},
		"confidence":  {Direction: "up", Confidence: 1.5, Materiality: "low", Reason: "r"},
		"reason":      {Direction: "up", Confidence: 0.5, Materiality: "low", Reason: " "},
	} {
		if _, err := Validate(bad); err == nil {
			t.Errorf("%s: invalid signal accepted", name)
		}
	}
}

func TestMockIsDeterministicAndValid(t *testing.T) {
	m := Mock{}
	a, _ := m.Analyze(context.Background(), sampleInput())
	b, _ := m.Analyze(context.Background(), sampleInput())
	if a != b {
		t.Fatal("mock must be deterministic")
	}
	if _, err := Validate(a); err != nil {
		t.Fatalf("mock produced an invalid signal: %v", err)
	}
}

func TestCompactKeepsNumbersAndDropsPadding(t *testing.T) {
	in := "Revenue from operations          1,234.50          1,046.20\r\n\n\n\n\f   Profit after tax      210.40       152.90   \n"
	got := Compact(in)
	if strings.Contains(got, "   ") || strings.Contains(got, "\n\n\n") || strings.Contains(got, "\r") {
		t.Errorf("padding not removed: %q", got)
	}
	for _, want := range []string{"Revenue from operations", "1,234.50", "1,046.20", "Profit after tax", "210.40", "152.90"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q: %q", want, got)
		}
	}
	if len(got) >= len(in) {
		t.Errorf("compact text should be shorter: %d >= %d", len(got), len(in))
	}
}

func TestCompactionHappensBeforeTruncation(t *testing.T) {
	// 300 characters of real content padded with lots of spaces: the content must survive a 200 character limit.
	padded := strings.Repeat("Net profit"+strings.Repeat(" ", 40), 10)
	in := sampleInput()
	in.Text = padded + "FINAL NUMBER 99.9"
	p := BuildPrompt(in, 200)
	if !strings.Contains(p, "FINAL NUMBER 99.9") {
		t.Fatalf("content after the padding was cut off:\n%s", p)
	}
}

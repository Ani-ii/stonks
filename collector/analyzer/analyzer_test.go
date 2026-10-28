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

package analyzer

import (
	"context"
	"hash/fnv"
)

// Mock returns deterministic fake signals so the pipeline can be tested with no model and no cost.
type Mock struct{}

func (Mock) Name() string { return "mock" }

func (Mock) Analyze(_ context.Context, in Input) (Signal, error) {
	h := fnv.New32a()
	h.Write([]byte(in.Filing.Source + in.Filing.SourceID))
	n := h.Sum32()
	dirs := []string{"up", "down", "neutral"}
	mats := []string{"high", "medium", "low"}
	return Signal{
		Direction:   dirs[n%3],
		Confidence:  float64(n%100) / 100,
		Materiality: mats[(n/3)%3],
		Reason:      "mock signal (no model was called)",
		Model:       "mock",
	}, nil
}

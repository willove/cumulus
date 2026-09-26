package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/willove/cumulus/internal/fast"
)

// A malformed-LLM-reply analyze failure must degrade to the rule analyzer,
// not fail the query (one bad model JSON used to 500 the whole request).
func TestDegradeAnalyzerFallsBackToRules(t *testing.T) {
	bad := stubAnalyzer{err: fmt.Errorf("json: cannot unmarshal string into AnalyzeResult.fallback")}
	got, err := degradeAnalyzer{inner: bad}.Analyze(context.Background(), "治安管理处罚法对噪音扰民怎么处罚")
	if err != nil {
		t.Fatalf("degraded analyze errored: %v", err)
	}
	want, werr := fast.RuleAnalyzer{}.Analyze(context.Background(), "治安管理处罚法对噪音扰民怎么处罚")
	if werr != nil {
		t.Fatalf("rule analyzer errored: %v", werr)
	}
	if len(got.Primary) == 0 || len(got.Primary) != len(want.Primary) {
		t.Fatalf("degraded primary = %v, want %v", got.Primary, want.Primary)
	}
	// A healthy inner analyzer passes through untouched.
	ok := stubAnalyzer{res: fast.Analysis{Primary: map[string]float64{"噪音": 1}}}
	healthy := degradeAnalyzer{inner: ok}
	got2, err2 := healthy.Analyze(context.Background(), "x")
	if err2 != nil || len(got2.Primary) != 1 {
		t.Fatalf("passthrough broken: %v %v", got2, err2)
	}
}

type stubAnalyzer struct {
	res fast.Analysis
	err error
}

func (s stubAnalyzer) Analyze(ctx context.Context, query string) (fast.Analysis, error) {
	return s.res, s.err
}

package deep

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/facts"
)

// TestThresholdTakeoverParsing pins both R1 takeover points: default
// byte-identical, valid override honored, garbage and out-of-range fall back.
func TestThresholdTakeoverParsing(t *testing.T) {
	for _, c := range []struct {
		env  string
		want float64
	}{
		{"", sufficientScore}, {"garbage", sufficientScore}, {"-1", sufficientScore}, {"11", sufficientScore},
		{"6.5", 6.5}, {"10", 10},
	} {
		t.Setenv("CLUS_SUFFICIENT_SCORE", c.env)
		if got := sufficientLine(); got != c.want {
			t.Fatalf("sufficientLine(%q) = %v, want %v", c.env, got, c.want)
		}
	}
	for _, c := range []struct {
		env  string
		want float64
	}{
		{"", facts.CoverScore}, {"x", facts.CoverScore}, {"12", facts.CoverScore},
		{"5.5", 5.5},
	} {
		t.Setenv("CLUS_COVER_SCORE", c.env)
		if got := facts.CoverScoreLine(); got != c.want {
			t.Fatalf("CoverScoreLine(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

// TestSufficientTakeoverMovesTheStop is the behavioural half: the same 9.0
// covering window stops the loop at the default line and keeps searching
// when the operator (or the calib loop) raises the line past it.
func TestSufficientTakeoverMovesTheStop(t *testing.T) {
	q := "孙悟空的兵器是什么"
	srcs := docsWith(q, "blk/a", "blk/b", "blk/c")

	t.Setenv("CLUS_SUFFICIENT_SCORE", "")
	e := engineWith(&fixedScorer{scores: []float64{9, 9, 9}, pass: true})
	out, err := e.runDeep(context.Background(), q, srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.StopReason != "sufficient" {
		t.Fatalf("default line: a 9.0 cover must stop sufficient, got %q", out.StopReason)
	}

	t.Setenv("CLUS_SUFFICIENT_SCORE", "9.5")
	e2 := engineWith(&fixedScorer{scores: []float64{9, 9, 9}, pass: true})
	out2, err := e2.runDeep(context.Background(), q, srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out2.StopReason == "sufficient" {
		t.Fatal("raised line: the same 9.0 window must NOT stop the loop")
	}
}

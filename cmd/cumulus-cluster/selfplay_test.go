package main

import (
	"testing"

	"github.com/willove/cumulus/internal/deep"
)

// TestSelfplayRateParsing pins the sampler gate: off by default, garbage
// falls back to off, only a clean 0..1 float arms it.
func TestSelfplayRateParsing(t *testing.T) {
	for _, c := range []struct {
		env  string
		want float64
	}{
		{"", 0}, {"garbage", 0}, {"-1", 0}, {"1.5", 0},
		{"0", 0}, {"0.1", 0.1}, {"1", 1},
	} {
		t.Setenv("CLUS_SELFPLAY_RATE", c.env)
		if got := selfplayRate(); got != c.want {
			t.Fatalf("selfplayRate(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

// TestCiteStab pins the stability metric: identical cites 1, disjoint 0, the
// classic Jaccard ratio in between, and two empty sets read as 1 (both runs
// agreed there was nothing to cite).
func TestCiteStab(t *testing.T) {
	mk := func(ids ...string) []deep.Ref {
		out := make([]deep.Ref, len(ids))
		for i, id := range ids {
			out[i] = deep.Ref{SourceID: id}
		}
		return out
	}
	if got := citeStab(mk("a", "b"), mk("a", "b")); got != 1 {
		t.Fatalf("identical = %v, want 1", got)
	}
	if got := citeStab(mk("a"), mk("b")); got != 0 {
		t.Fatalf("disjoint = %v, want 0", got)
	}
	if got := citeStab(mk("a", "b"), mk("b", "c")); got != 1.0/3.0 {
		t.Fatalf("half-overlap = %v, want 1/3", got)
	}
	if got := citeStab(nil, nil); got != 1 {
		t.Fatalf("both-empty = %v, want 1", got)
	}
}

package belief

import (
	"reflect"
	"testing"
)

func TestObserveEMAMovesTowardObservation(t *testing.T) {
	b := New(map[string]float64{"f": 0.5}, 0.5)
	b.Observe("f", 1.0)
	if got := b.Get("f"); got != 0.75 {
		t.Fatalf("o=1 on 0.5: %v, want 0.75", got)
	}
	b.Observe("f", 1.0)
	if got := b.Get("f"); got != 0.875 {
		t.Fatalf("second o=1: %v, want 0.875", got)
	}
	b.Observe("f", 0.0)
	if got := b.Get("f"); got != 0.4375 {
		t.Fatalf("o=0 after 0.875: %v, want 0.4375", got)
	}
	// The property the naive-Bayes draft lacked: an observation that MATCHES
	// the prior leaves it untouched (o=b=0.8 stays 0.8).
	b2 := New(map[string]float64{"g": 0.8}, 0.5)
	b2.Observe("g", 0.8)
	if got := b2.Get("g"); got != 0.8 {
		t.Fatalf("matching observation must not move the prior: %v, want 0.8", got)
	}
	// Unknown ids start at maximum uncertainty (0.5).
	b3 := New(nil, 0.5)
	b3.Observe("new", 1.0)
	if got := b3.Get("new"); got != 0.75 {
		t.Fatalf("unknown start: %v, want 0.75", got)
	}
}

func TestKappaZeroIsNoOp(t *testing.T) {
	b := New(map[string]float64{"f": 0.3}, 0)
	b.Observe("f", 1.0)
	b.Observe("f", 0.0)
	if got := b.Get("f"); got != 0.3 {
		t.Fatalf("κ=0 must never move the posterior: %v", got)
	}
}

func TestOrderUntriedFirstThenPosterior(t *testing.T) {
	// Prior: a=1.0, b=0.5, c decays to the floor. d unknown (0.5).
	b := New(PriorFromRank([]string{"a", "b", "c"}), 0.5)
	// b proves productive; c proves barren.
	b.Observe("b", 1.0) // 0.5 → 0.75
	b.Observe("c", 0.0) // floor → 0
	ids := []string{"c", "d", "b", "a"}
	got := b.Order(ids)
	// Untried (d, a) by posterior desc (a=1.0 > d=0.5), then tried by
	// posterior desc (b=0.75 > c=0). Tried files never outrank untried ones.
	want := []string{"a", "d", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Order = %v, want %v", got, want)
	}
	// Determinism: same input, same order.
	if again := b.Order(ids); !reflect.DeepEqual(again, want) {
		t.Fatalf("Order not deterministic: %v vs %v", again, want)
	}
}

func TestPriorFromRankDecays(t *testing.T) {
	p := PriorFromRank([]string{"a", "b", "c", "d"})
	if p["a"] != 1.0 {
		t.Fatalf("first prior = %v, want 1", p["a"])
	}
	if p["b"] >= p["a"] || p["c"] >= p["b"] {
		t.Fatalf("prior must decay monotonically: %v", p)
	}
	if p["d"] < 0.04 {
		t.Fatalf("prior floor breached: %v", p["d"])
	}
	// Long lists stay above the floor, never at zero.
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = "f"
	}
	pd := PriorFromRank(ids)
	if pd["f"] <= 0 {
		t.Fatalf("late-admitted prior must stay positive: %v", pd["f"])
	}
}

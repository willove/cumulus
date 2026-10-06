package belief

import (
	"reflect"
	"testing"
)

func TestObserveMovesTowardObservation(t *testing.T) {
	b := New(map[string]float64{"d1": 0.5}, 0.5)
	b.Observe("d1", 1.0)
	got, _ := b.Get("d1")
	if got <= 0.5 || got >= 1.0 {
		t.Fatalf("kappa=0.5 from 0.5 toward 1.0 must land strictly between, got %v", got)
	}
	if want := 0.75; got != want {
		t.Fatalf("want %v (0.5 + 0.5*(1.0-0.5)), got %v", want, got)
	}
}

func TestObserveSaturatesUnderRepeatedOnes(t *testing.T) {
	b := New(map[string]float64{"d1": 0.1}, 0.5)
	for i := 0; i < 10; i++ {
		b.Observe("d1", 1)
	}
	got, _ := b.Get("d1")
	if got < 0.99 {
		t.Fatalf("repeated strong observation must saturate, got %v", got)
	}
}

func TestObserveUnknownIDStartsAtObservation(t *testing.T) {
	b := New(nil, 0.5)
	b.Observe("new", 0.3)
	got, ok := b.Get("new")
	if !ok || got != 0.3 {
		t.Fatalf("first observation must initialize at the observation, got %v ok=%v", got, ok)
	}
}

func TestOrderPutsUntriedFirst(t *testing.T) {
	b := New(map[string]float64{"tried-high": 0.9, "untried-low": 0.2}, 0.5)
	// tried-high 已在表里（试过），untried-low 也在表里——用第三条未登记的
	ids := []string{"tried-high", "untried-low", "never-seen"}
	got := b.Order(ids)
	if got[0] != "never-seen" {
		t.Fatalf("unregistered candidate must come first, got %v", got)
	}
	if got[1] != "tried-high" {
		t.Fatalf("tried candidates order by posterior desc, got %v", got)
	}
	if got[2] != "untried-low" {
		t.Fatalf("got %v", got)
	}
}

func TestOrderDeterministicOnTies(t *testing.T) {
	b := New(nil, 0.5)
	a := b.Order([]string{"b", "a", "c"})
	c := b.Order([]string{"c", "a", "b"})
	if !reflect.DeepEqual(a, c) {
		t.Fatalf("order must be deterministic: %v vs %v", a, c)
	}
}

func TestGetUnknownIsNotZeroBelief(t *testing.T) {
	b := New(nil, 0.5)
	if _, ok := b.Get("nope"); ok {
		t.Fatal("unobserved id must report no belief, not zero")
	}
}

package graph

import (
	"context"
	"testing"
)

func TestLinkCoOcurBumpsWeight(t *testing.T) {
	ctx := context.Background()
	st := NewMemory()
	if err := LinkCoOcur(ctx, st, "A", "B"); err != nil {
		t.Fatal(err)
	}
	if w := CoOccurWeight(ctx, st, "A", "B"); w < 0.7 {
		t.Fatalf("first co_occur weight=%v want >=0.7", w)
	}
	if err := LinkCoOcur(ctx, st, "B", "A"); err != nil {
		t.Fatal(err)
	}
	w := CoOccurWeight(ctx, st, "A", "B")
	if w <= 0.7 {
		t.Fatalf("repeat co_occur must bump weight, got %v", w)
	}
	ps := CoOccurPartners(ctx, st, "A")
	if len(ps) != 1 {
		t.Fatalf("profile size=%d want 1", len(ps))
	}
}

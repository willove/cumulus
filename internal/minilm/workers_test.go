package minilm

import (
	"math/rand"
	"testing"
)

// CLUS_EMBED_WORKERS parsing: a positive integer caps; unset/garbage/<1
// mean no cap (GOMAXPROCS — the pre-knob behaviour). A bad knob must never
// fail a backfill over; the host just runs hot.
func TestParseWorkerCap(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0}, {"2", 2}, {"1", 1}, {"16", 16},
		{"abc", 0}, {"0", 0}, {"-3", 0}, {"2.5", 0}, {" 4", 0},
	} {
		if got := parseWorkerCap(tc.in); got != tc.want {
			t.Errorf("parseWorkerCap(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// The worker cap is a pure scheduling change: whatever the parallelism, one
// matmul must produce bit-identical output — the accumulation order per
// element is fixed by construction. This pins that, because a throttle that
// altered results would silently poison every vector written by a capped
// backfill (and nothing downstream could tell).
func TestMatmulRowsTIsWorkerCountInvariant(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	const L, in, out = 24, 96, 160 // out > rowBloc so multiple blocks exist
	x := make([]float32, L*in)
	w := make([]float32, out*in)
	b := make([]float32, out)
	for i := range x {
		x[i] = float32(rng.NormFloat64())
	}
	for i := range w {
		w[i] = float32(rng.NormFloat64()) * 0.05
	}
	for i := range b {
		b[i] = float32(rng.NormFloat64()) * 0.1
	}
	run := func(cap int) []float32 {
		y := make([]float32, L*out)
		old := embedWorkerCap
		embedWorkerCap = cap
		defer func() { embedWorkerCap = old }()
		matmulRowsT(x, w, b, L, out, in, y)
		return y
	}
	seq, par := run(1), run(0) // single worker vs uncapped (GOMAXPROCS)
	for i := range seq {
		if seq[i] != par[i] {
			t.Fatalf("y[%d]: capped=%v uncapped=%v — the cap changed the arithmetic", i, seq[i], par[i])
		}
	}
}

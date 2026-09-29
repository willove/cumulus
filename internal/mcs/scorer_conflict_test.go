package mcs

import (
	"context"
	"errors"
	"testing"
)

// conflictStubScorer implements Scorer + BatchScorer + ConflictBatchScorer so
// one stub can pin which branch evalAll took.
type conflictStubScorer struct {
	perCalls  int
	batCalls  int
	confCalls int
	gotPrior  int
	failWith  error
}

func (c *conflictStubScorer) Score(_ context.Context, _ string, _ Sample) (float64, string, error) {
	c.perCalls++
	return 1, "per", nil
}

func (c *conflictStubScorer) ScoreBatch(_ context.Context, _ string, _ []string, samples []Sample) ([]BatchResult, error) {
	c.batCalls++
	out := make([]BatchResult, len(samples))
	for i := range samples {
		out[i] = BatchResult{Score: 2, Reasoning: "batch"}
	}
	return out, nil
}

func (c *conflictStubScorer) ScoreBatchConflict(_ context.Context, _ string, _ []string, samples, prior []Sample) ([]BatchResult, error) {
	c.confCalls++
	c.gotPrior = len(prior)
	if c.failWith != nil {
		return nil, c.failWith
	}
	out := make([]BatchResult, len(samples))
	for i := range samples {
		out[i] = BatchResult{Score: 3, Reasoning: "dims", Conflicts: []string{"K1"}}
	}
	return out, nil
}

func TestScorerConflictDefaultsOff(t *testing.T) {
	for _, v := range []string{"", "0", "garbage"} {
		t.Setenv("CLUS_SCORER_CONFLICT", v)
		if ScorerConflict() {
			t.Fatalf("ScorerConflict(%q) = true, want false", v)
		}
	}
	t.Setenv("CLUS_SCORER_CONFLICT", "1")
	if !ScorerConflict() {
		t.Fatal(`ScorerConflict("1") = false, want true`)
	}
}

// TestEvalAllConflictPathSelection pins the precedence: conflict branch when
// the flag and interface are both present (regardless of the batch flag),
// plain batch next, per-window otherwise.
func TestEvalAllConflictPathSelection(t *testing.T) {
	in := gateWindows(4)

	// Conflict on, batch off: the conflict branch still fires (it implies
	// batching by construction).
	t.Setenv("CLUS_SCORER_CONFLICT", "1")
	t.Setenv("CLUS_MCS_SCORER_BATCH", "")
	stub := &conflictStubScorer{}
	s := New(DefaultConfig(), stub)
	got, err := s.evalAll(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("conflict evalAll: %v", err)
	}
	if stub.confCalls != 1 || stub.batCalls != 0 || stub.perCalls != 0 {
		t.Fatalf("want conflict-only, got conf=%d bat=%d per=%d", stub.confCalls, stub.batCalls, stub.perCalls)
	}
	for i := range got {
		if got[i].Score != 3 || len(got[i].Conflicts) != 1 || got[i].Conflicts[0] != "K1" {
			t.Fatalf("window %d missing conflict annotations: %+v", i, got[i])
		}
	}

	// Conflict off, batch on: plain batch (v3a exactly).
	t.Setenv("CLUS_SCORER_CONFLICT", "")
	t.Setenv("CLUS_MCS_SCORER_BATCH", "1")
	stub2 := &conflictStubScorer{}
	s2 := New(DefaultConfig(), stub2)
	if _, err := s2.evalAll(context.Background(), "q", in); err != nil {
		t.Fatalf("batch evalAll: %v", err)
	}
	if stub2.confCalls != 0 || stub2.batCalls != 1 {
		t.Fatalf("want plain batch, got conf=%d bat=%d", stub2.confCalls, stub2.batCalls)
	}

	// Both off: per-window (default is byte-identical).
	t.Setenv("CLUS_SCORER_CONFLICT", "")
	t.Setenv("CLUS_MCS_SCORER_BATCH", "")
	stub3 := &conflictStubScorer{}
	s3 := New(DefaultConfig(), stub3)
	if _, err := s3.evalAll(context.Background(), "q", in); err != nil {
		t.Fatalf("per-window evalAll: %v", err)
	}
	if stub3.confCalls != 0 || stub3.batCalls != 0 || stub3.perCalls != len(in) {
		t.Fatalf("want per-window only, got conf=%d bat=%d per=%d", stub3.confCalls, stub3.batCalls, stub3.perCalls)
	}
}

// TestEvalAllConflictReceivesPrior: the digest rides the call — Prior set on
// the sampler reaches ScoreBatchConflict verbatim.
func TestEvalAllConflictReceivesPrior(t *testing.T) {
	t.Setenv("CLUS_SCORER_CONFLICT", "1")
	stub := &conflictStubScorer{}
	s := New(DefaultConfig(), stub)
	s.Prior = gateWindows(3)
	if _, err := s.evalAll(context.Background(), "q", gateWindows(2)); err != nil {
		t.Fatalf("evalAll: %v", err)
	}
	if stub.gotPrior != 3 {
		t.Fatalf("prior = %d windows, want 3", stub.gotPrior)
	}
}

// TestEvalAllConflictFailureIsHonest: the conflict branch keeps the
// total-failure contract.
func TestEvalAllConflictFailureIsHonest(t *testing.T) {
	t.Setenv("CLUS_SCORER_CONFLICT", "1")
	upstream := errors.New("status 503: upstream melted")
	stub := &conflictStubScorer{failWith: upstream}
	s := New(DefaultConfig(), stub)
	in := gateWindows(3)
	got, err := s.evalAll(context.Background(), "q", in)
	if err == nil {
		t.Fatal("conflict total failure returned nil error")
	}
	if !errors.Is(err, upstream) {
		t.Fatalf("upstream cause must survive, got %v", err)
	}
	for i, sm := range got {
		if sm.Score != ScoreFailed {
			t.Fatalf("window %d missing failure sentinel", i)
		}
	}
}

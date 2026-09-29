package mcs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// batchStubScorer implements BOTH Scorer and BatchScorer so one stub can pin
// which path evalAll actually took (call counters on both sides).
type batchStubScorer struct {
	mu       sync.Mutex
	perCall  int
	batCall  int
	lastBat  int
	failWith error
}

func (b *batchStubScorer) Score(_ context.Context, _ string, s Sample) (float64, string, error) {
	b.mu.Lock()
	b.perCall++
	b.mu.Unlock()
	return float64(len([]rune(s.Content)) % 10), "per-window", nil
}

func (b *batchStubScorer) ScoreBatch(_ context.Context, _ string, _ []string, samples []Sample) ([]BatchResult, error) {
	b.mu.Lock()
	b.batCall++
	b.lastBat = len(samples)
	b.mu.Unlock()
	if b.failWith != nil {
		return nil, b.failWith
	}
	out := make([]BatchResult, len(samples))
	for i, sm := range samples {
		out[i] = BatchResult{Score: float64(len([]rune(sm.Content)) % 10), Reasoning: "batch", Covers: []string{"f1"}}
	}
	return out, nil
}

func TestScorerBatchDefaultsOff(t *testing.T) {
	// The batched path must stay opt-in: an unconfigured process behaves
	// exactly like the per-window loop (same discipline as the worker cap).
	for _, v := range []string{"", "0", "garbage", "2"} {
		t.Setenv("CLUS_MCS_SCORER_BATCH", v)
		if ScorerBatch() {
			t.Fatalf("ScorerBatch(%q) = true, want false (only \"1\" opts in)", v)
		}
	}
	t.Setenv("CLUS_MCS_SCORER_BATCH", "1")
	if !ScorerBatch() {
		t.Fatal(`ScorerBatch("1") = false, want true`)
	}
}

// TestEvalAllBatchedOneCallDistributesInOrder pins the batched contract: one
// batch call for the round, results index-aligned with the input, and the
// per-window path untouched.
func TestEvalAllBatchedOneCallDistributesInOrder(t *testing.T) {
	t.Setenv("CLUS_MCS_SCORER_BATCH", "1")
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "8") // workers are irrelevant on the batch path; pin that too
	in := gateWindows(6)
	stub := &batchStubScorer{}
	s := New(DefaultConfig(), stub)

	got, err := s.evalAll(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("batched evalAll: %v", err)
	}
	if stub.batCall != 1 || stub.perCall != 0 {
		t.Fatalf("want exactly 1 batch call and 0 per-window calls, got batch=%d per=%d", stub.batCall, stub.perCall)
	}
	if stub.lastBat != len(in) {
		t.Fatalf("batch saw %d windows, want %d", stub.lastBat, len(in))
	}
	for i := range got {
		if got[i].Score != float64(len([]rune(in[i].Content))%10) {
			t.Fatalf("result %d not index-aligned: %+v", i, got[i])
		}
		if got[i].Reasoning != "batch" || len(got[i].Covers) != 1 || got[i].Covers[0] != "f1" {
			t.Fatalf("result %d missing batch annotations: %+v", i, got[i])
		}
	}
}

// TestEvalAllBatchFlagOffUsesPerWindowPath is the control arm: with the flag
// off, a scorer that COULD batch must still be scored per window — the
// default is byte-identical, the flag is the only switch.
func TestEvalAllBatchFlagOffUsesPerWindowPath(t *testing.T) {
	t.Setenv("CLUS_MCS_SCORER_BATCH", "")
	in := gateWindows(6)
	stub := &batchStubScorer{}
	s := New(DefaultConfig(), stub)

	got, err := s.evalAll(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("per-window evalAll: %v", err)
	}
	if stub.batCall != 0 || stub.perCall != len(in) {
		t.Fatalf("want 0 batch calls and %d per-window calls, got batch=%d per=%d", len(in), stub.batCall, stub.perCall)
	}
	if len(got) != len(in) {
		t.Fatalf("len = %d, want %d", len(got), len(in))
	}
}

// TestEvalAllBatchedSingleWindowSkipsBatch: a one-window round (the small-file
// shortcut) has nothing to amortize — it must not pay the batched prompt.
func TestEvalAllBatchedSingleWindowSkipsBatch(t *testing.T) {
	t.Setenv("CLUS_MCS_SCORER_BATCH", "1")
	stub := &batchStubScorer{}
	s := New(DefaultConfig(), stub)

	if _, err := s.evalAll(context.Background(), "q", gateWindows(1)); err != nil {
		t.Fatalf("single-window evalAll: %v", err)
	}
	if stub.batCall != 0 || stub.perCall != 1 {
		t.Fatalf("single window should go per-window, got batch=%d per=%d", stub.batCall, stub.perCall)
	}
}

// TestEvalAllBatchedFailureIsHonest: a whole-batch failure surfaces as the
// SAME honest total-failure error the per-window path produces — never as a
// page of ScoreFailed that reads downstream as 证据不足.
func TestEvalAllBatchedFailureIsHonest(t *testing.T) {
	t.Setenv("CLUS_MCS_SCORER_BATCH", "1")
	upstream := errors.New("status 429: token plan exhausted")
	stub := &batchStubScorer{failWith: upstream}
	s := New(DefaultConfig(), stub)
	in := gateWindows(4)

	got, err := s.evalAll(context.Background(), "q", in)
	if err == nil {
		t.Fatal("batched total failure returned nil error — it would read as 证据不足")
	}
	if !errors.Is(err, upstream) {
		t.Fatalf("upstream cause must survive, got %v", err)
	}
	if !strings.Contains(err.Error(), "all 4 windows") {
		t.Fatalf("error should name the window count, got %q", err.Error())
	}
	for i, sm := range got {
		if sm.Score != ScoreFailed {
			t.Fatalf("window %d should carry the failure sentinel, got %v", i, sm.Score)
		}
	}
}

// TestSampleBodyBatchedMatchesPerWindowShape runs both paths over the same
// deterministic stub and pins that the OUTPUT SHAPE is indistinguishable
// (same samples, same order) — the batched path is a transport change only.
func TestSampleBodyBatchedMatchesPerWindowShape(t *testing.T) {
	// A deterministic per-window scorer and its batched twin must produce the
	// identical page for the same body.
	perWin := New(DefaultConfig(), twinPerWindow{})
	t.Setenv("CLUS_MCS_SCORER_BATCH", "")
	a, err := perWin.SampleBody(context.Background(), "连接池", strings.Repeat("正文内容。", 60_000))
	if err != nil {
		t.Fatalf("per-window SampleBody: %v", err)
	}
	t.Setenv("CLUS_MCS_SCORER_BATCH", "1")
	batched := New(DefaultConfig(), twinBatched{})
	b, err := batched.SampleBody(context.Background(), "连接池", strings.Repeat("正文内容。", 60_000))
	if err != nil {
		t.Fatalf("batched SampleBody: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("batched path changed the output shape:\n per=%+v\n bat=%+v", a, b)
	}
}

type twinPerWindow struct{}

func (twinPerWindow) Score(_ context.Context, _ string, s Sample) (float64, string, error) {
	return float64(len([]rune(s.Content)) % 7), "twin", nil
}

type twinBatched struct{}

func (twinBatched) Score(ctx context.Context, q string, s Sample) (float64, string, error) {
	return twinPerWindow{}.Score(ctx, q, s)
}

func (twinBatched) ScoreBatch(_ context.Context, _ string, _ []string, samples []Sample) ([]BatchResult, error) {
	out := make([]BatchResult, len(samples))
	for i, sm := range samples {
		out[i] = BatchResult{Score: float64(len([]rune(sm.Content)) % 7), Reasoning: "twin"}
	}
	return out, nil
}

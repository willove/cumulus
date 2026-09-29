package mcs

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// gateScorer is a deterministic, concurrency-safe stub used to pin the
// evalAll ordering contract. It scores from the window content alone (no map
// iteration, no clock, no shared accumulator) so any output difference across
// worker caps is attributable to ordering, not to the stub.
type gateScorer struct {
	// failAt, when non-nil, makes those indices return an error — used to pin
	// the "scorer failed for all windows" aggregate message, which historically
	// named the LAST failing window in input order.
	failAt map[int]bool
	delay  chan struct{} // optional: blocks until closed, to force overlap
}

func (g gateScorer) Score(_ context.Context, _ string, s Sample) (float64, string, error) {
	if g.delay != nil {
		<-g.delay
	}
	// Derive the score from content length so it is stable per window.
	score := float64(len([]rune(s.Content)) % 10)
	if g.failAt != nil {
		// gateWindows encodes the position as Start = i*gateStride.
		if g.failAt[s.Start/gateStride] {
			return 0, "", fmt.Errorf("gate scorer refused idx=%d", s.Start/gateStride)
		}
	}
	return score, fmt.Sprintf("len=%d", len([]rune(s.Content))), nil
}

// gateStride is the Start increment gateWindows uses, so a stub can recover a
// window's input position from the Sample it is handed.
const gateStride = 7

func gateWindows(n int) []Sample {
	in := make([]Sample, 0, n)
	for i := 0; i < n; i++ {
		// Vary the length so scores differ and a mis-ordered result is visible.
		in = append(in, Sample{Start: i * gateStride, End: i*gateStride + 5, Content: strings.Repeat("窗", i+1), Arm: "lex"})
	}
	return in
}

func TestScorerWorkersDefaultsToSerial(t *testing.T) {
	// The cap must default to 1: an unconfigured process has to behave exactly
	// like the historical serial loop (D7 — every perf knob is opt-in).
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "")
	if got := ScorerWorkers(); got != 1 {
		t.Fatalf("ScorerWorkers() default = %d, want 1 (serial is the default)", got)
	}
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "garbage")
	if got := ScorerWorkers(); got != 1 {
		t.Fatalf("ScorerWorkers() on unparseable value = %d, want 1", got)
	}
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "0")
	if got := ScorerWorkers(); got != 1 {
		t.Fatalf("ScorerWorkers() on zero = %d, want 1 (a cap of 0 would deadlock)", got)
	}
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "8")
	if got := ScorerWorkers(); got != 8 {
		t.Fatalf("ScorerWorkers() = %d, want 8", got)
	}
}

// TestEvalAllConcurrencyPreservesOrder is the P0-2 gate: raising the worker cap
// must not move a single byte of evalAll's output. The scorer here blocks until
// the cap is high enough to actually overlap, so a sequential implementation
// would deadlock rather than quietly pass.
func TestEvalAllConcurrencyPreservesOrder(t *testing.T) {
	in := gateWindows(12)

	// Serial baseline.
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "1")
	s := New(DefaultConfig(), gateScorer{})
	serial, err := s.evalAll(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("serial evalAll: %v", err)
	}
	if len(serial) != len(in) {
		t.Fatalf("serial len = %d, want %d", len(serial), len(in))
	}
	for i := range serial {
		if serial[i].Start != in[i].Start {
			t.Fatalf("serial output not index-aligned at %d: %+v", i, serial[i])
		}
	}

	// Concurrent. delay is closed up front, but with a cap below the window
	// count the semaphore would still make progress; with 12 workers all 12 run
	// at once, so the scheduler genuinely reorders them.
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "12")
	closed := make(chan struct{})
	close(closed)
	s2 := New(DefaultConfig(), gateScorer{delay: closed})
	par, err := s2.evalAll(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("concurrent evalAll: %v", err)
	}
	if !reflect.DeepEqual(serial, par) {
		t.Fatalf("concurrency changed evalAll output:\n serial=%+v\n par   =%+v", serial, par)
	}

	// Repeat a few times: ordering bugs from a data race are intermittent.
	for i := 0; i < 20; i++ {
		s3 := New(DefaultConfig(), gateScorer{})
		got, err := s3.evalAll(context.Background(), "q", in)
		if err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
		if !reflect.DeepEqual(serial, got) {
			t.Fatalf("repeat %d diverged from serial baseline", i)
		}
	}
}

// TestEvalAllAllFailedNamesLastIndex pins the aggregate-error contract: the
// message historically names the last failing window in INPUT order, so the
// error must not depend on which goroutine finished last.
func TestEvalAllAllFailedNamesLastIndex(t *testing.T) {
	in := gateWindows(6)
	all := map[int]bool{}
	for i := range in {
		all[i] = true
	}

	var got string
	for _, workers := range []string{"1", "6"} {
		t.Setenv("CLUS_MCS_SCORER_WORKERS", workers)
		s := New(DefaultConfig(), gateScorer{failAt: all})
		_, err := s.evalAll(context.Background(), "q", in)
		if err == nil {
			t.Fatalf("workers=%s: want total-failure error, got nil", workers)
		}
		if workers == "1" {
			got = err.Error()
			continue
		}
		if err.Error() != got {
			t.Fatalf("total-failure message changed with concurrency:\n serial=%q\n par   =%q", got, err.Error())
		}
	}
	if !strings.Contains(got, "idx=5") {
		t.Fatalf("total-failure message should name the last window (idx=5), got %q", got)
	}
}

// TestEvalAllPartialFailureKeepsOrder pins that a mix of failures and successes
// still lands in input order, with the failure sentinel preserved in place.
func TestEvalAllPartialFailureKeepsOrder(t *testing.T) {
	in := gateWindows(8)
	fail := map[int]bool{1: true, 4: true, 6: true}

	t.Setenv("CLUS_MCS_SCORER_WORKERS", "1")
	s := New(DefaultConfig(), gateScorer{failAt: fail})
	serial, err := s.evalAll(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("serial: %v", err)
	}

	t.Setenv("CLUS_MCS_SCORER_WORKERS", "8")
	s2 := New(DefaultConfig(), gateScorer{failAt: fail})
	par, err := s2.evalAll(context.Background(), "q", in)
	if err != nil {
		t.Fatalf("concurrent: %v", err)
	}
	if !reflect.DeepEqual(serial, par) {
		t.Fatalf("partial-failure output diverged:\n serial=%+v\n par   =%+v", serial, par)
	}
	for _, i := range []int{1, 4, 6} {
		if serial[i].Score != ScoreFailed {
			t.Fatalf("index %d should carry the failure sentinel, got %v", i, serial[i].Score)
		}
	}
}

// TestEvalAllRaceClean runs the concurrent path under -race with an
// overlap-forcing stub.
func TestEvalAllRaceClean(t *testing.T) {
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "8")
	s := New(DefaultConfig(), gateScorer{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.evalAll(context.Background(), "q", gateWindows(20)); err != nil {
				t.Errorf("concurrent evalAll: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestScorerCapCannotDeadlockOneWindow guards the trivial case: a single-window
// round (the small-file shortcut) must not wait on the semaphore.
func TestScorerCapCannotDeadlockOneWindow(t *testing.T) {
	t.Setenv("CLUS_MCS_SCORER_WORKERS", "8")
	s := New(DefaultConfig(), gateScorer{})
	done := make(chan []Sample, 1)
	go func() {
		got, err := s.evalAll(context.Background(), "q", gateWindows(1))
		if err != nil {
			t.Error(err)
		}
		done <- got
	}()
	select {
	case got := <-done:
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
	case <-context.Background().Done():
		t.Fatal("unreachable")
	}
}

var _ = errors.New // keep the errors import honest for future gate cases

// allFailScorer fails every window, the way a live endpoint does when the
// account is exhausted (429/402) or the gateway is unreachable.
type allFailScorer struct{ err error }

func (a allFailScorer) Score(_ context.Context, _ string, _ Sample) (float64, string, error) {
	return 0, "", a.err
}

// TestSampleBodyPropagatesTotalScorerFailure pins the property the evalAll
// comment claims but nothing enforced: an endpoint outage must surface as an
// ERROR, never as "no evidence found".
//
// The distinction is the whole point. A quota-exhausted endpoint and a corpus
// that genuinely lacks the answer both look like "nothing relevant" to a
// downstream reader, and the second one gets reported to the user as
// 证据不足 — an answer about the corpus when the real problem is the
// account. Both SampleBody paths are covered because the small-file shortcut
// and the windowed path reach evalAll separately.
func TestSampleBodyPropagatesTotalScorerFailure(t *testing.T) {
	upstream := errors.New("status 429: token plan exhausted")
	sc := New(EnvConfig(), allFailScorer{err: upstream})

	// Small-file path: the whole body is one window.
	if _, err := sc.SampleBody(context.Background(), "连接池最大连接数", strings.Repeat("正文内容。", 20)); err == nil {
		t.Fatal("small-file path swallowed a total scorer failure — it would read as 证据不足")
	} else if !errors.Is(err, upstream) {
		t.Fatalf("the upstream cause must survive, got %v", err)
	}

	// Windowed path: a body above SmallFileRunes.
	big := strings.Repeat("正文内容。", 60_000)
	if _, err := sc.SampleBody(context.Background(), "连接池最大连接数", big); err == nil {
		t.Fatal("windowed path swallowed a total scorer failure")
	} else if !errors.Is(err, upstream) {
		t.Fatalf("the upstream cause must survive, got %v", err)
	}
}

// TestSampleBodyKeepsPartialFailure is the other half: when only SOME windows
// fail, the surviving ones must still come back (with the sentinel on the
// failures), because a partially-degraded corpus is still answerable and
// discarding it would be a worse failure than reporting it.
func TestSampleBodyKeepsPartialFailure(t *testing.T) {
	partial := partialFailScorer{failEvery: 3}
	sc := New(EnvConfig(), &partial)
	got, err := sc.SampleBody(context.Background(), "连接池最大连接数", strings.Repeat("正文内容。", 60_000))
	if err != nil {
		t.Fatalf("a partial failure must not abort the round: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("a partial failure returned no samples at all")
	}
	live, failed := 0, 0
	for _, sm := range got {
		if sm.Score == ScoreFailed {
			failed++
		} else {
			live++
		}
	}
	if live == 0 || failed == 0 {
		t.Fatalf("expected a mix of live and failed windows, got live=%d failed=%d", live, failed)
	}
}

type partialFailScorer struct{ failEvery, n int }

func (p *partialFailScorer) Score(_ context.Context, _ string, s Sample) (float64, string, error) {
	p.n++
	if p.n%p.failEvery == 0 {
		return 0, "", errors.New("transient upstream error")
	}
	return 5, "ok", nil
}

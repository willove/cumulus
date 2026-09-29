package kb

import (
	"context"
	"errors"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// C2's second half: the optional no-reference persist judge. Record mode
// (the default at the serve face) persists regardless and stamps the
// verdict on the cluster for later analysis; gate mode
// (CLUS_PERSIST_JUDGE=1, wired by the serve face) treats a negative verdict
// as a refusal; a judge ERROR is never a verdict — the write path fails
// open on errors and closes on verdicts only.
func TestPersistJudgeRecordGateAndFailOpen(t *testing.T) {
	ctx := context.Background()
	src := source.New("A", "txt", "", "a", "zh", "闯红灯的，处二百元罚款。", nil)
	corpus := []source.Source{src}
	newAns := func() fast.Answer {
		return fast.Answer{
			Query: "闯红灯会有什么处罚", Summary: "闯红灯处二百元罚款。",
			SourceID: src.ID, Confidence: 0.8,
			Samples: []mcs.Sample{{Source: src.ID, Start: 0, End: 20, Content: src.Body, Score: 8}},
		}
	}
	noJudge := func(context.Context, string, string) (bool, string, error) { return false, "not an answer", nil }
	errJudge := func(context.Context, string, string) (bool, string, error) {
		return false, "", errors.New("endpoint down")
	}

	// A wired judge with neither flag set must cost nothing — the measured
	// case was ~2 calls per query (~4,000 tokens of a 27,000 budget) spent
	// filing a verdict that gates nothing.
	t.Run("wired but neither gated nor recorded costs nothing", func(t *testing.T) {
		e := New(nil, cluster.NewMemory(), cluster.Local{N: 64})
		called := 0
		e.Judge = func(context.Context, string, string) (bool, string, error) {
			called++
			return false, "not an answer", nil
		}
		r, err := e.Persist(ctx, newAns(), corpus)
		if err != nil {
			t.Fatal(err)
		}
		if called != 0 {
			t.Fatalf("judge ran %d times with neither gate nor record requested", called)
		}
		if r.Judged {
			t.Fatalf("no verdict was requested, so none should be reported: %+v", r)
		}
		if !r.Persisted {
			t.Fatal("the answer must still persist")
		}
	})

	t.Run("record mode keeps the answer, stamps the verdict", func(t *testing.T) {
		e := New(nil, cluster.NewMemory(), cluster.Local{N: 64})
		// Record mode is now explicit (JudgeRecord), not implied by Judge being
		// wired: the judge is a model call per persisted answer, so it runs
		// only when it can gate or when the verdict was asked for.
		e.Judge, e.JudgeGates, e.JudgeRecord = noJudge, false, true
		r, err := e.Persist(ctx, newAns(), corpus)
		if err != nil || !r.Persisted {
			t.Fatalf("record mode must still persist: %+v %v", r, err)
		}
		if !r.Judged || r.JudgeOK || r.JudgeWhy != "not an answer" {
			t.Fatalf("verdict must ride on the result: %+v", r)
		}
		stored, err := e.Store.Get(ctx, r.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.JudgeOK == nil || *stored.JudgeOK || stored.JudgeWhy != "not an answer" {
			t.Fatalf("verdict must ride on the cluster: ok=%v why=%q", stored.JudgeOK, stored.JudgeWhy)
		}
	})
	t.Run("gate mode refuses a negative verdict", func(t *testing.T) {
		e := New(nil, cluster.NewMemory(), cluster.Local{N: 64})
		e.Judge, e.JudgeGates = noJudge, true
		r, err := e.Persist(ctx, newAns(), corpus)
		if err != nil {
			t.Fatal(err)
		}
		if r.Persisted {
			t.Fatal("gate mode must refuse persistence on a negative verdict")
		}
		if !r.Judged || r.JudgeOK {
			t.Fatalf("the refusal must carry its reason: %+v", r)
		}
		all, err := e.Store.All(ctx)
		if err != nil || len(all) != 0 {
			t.Fatalf("nothing may be written: %d clusters", len(all))
		}
	})
	t.Run("judge error fails open", func(t *testing.T) {
		e := New(nil, cluster.NewMemory(), cluster.Local{N: 64})
		e.Judge, e.JudgeGates = errJudge, true // even gating must not act on an error
		r, err := e.Persist(ctx, newAns(), corpus)
		if err != nil || !r.Persisted {
			t.Fatalf("a judge error must not block persistence: %+v %v", r, err)
		}
		if r.Judged {
			t.Fatal("an error is not a verdict — Judged must stay false")
		}
		if r.JudgeWhy == "" {
			t.Fatal("the error note must ride along for diagnosis")
		}
	})
}

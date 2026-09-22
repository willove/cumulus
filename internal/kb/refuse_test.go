package kb

import (
	"context"
	"testing"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

// refusingSynth models the synthesize_roi refusal. The summary text is what
// matters to RelevanceGate — answering text for the non-refused case, the
// refusal wording for the refused one.
type refusingSynth struct{ refused bool }

func (r refusingSynth) Synthesize(ctx context.Context, query string, samples []mcs.Sample) (string, error) {
	if r.refused {
		return "现有证据未提供连接池配置的答案。", nil
	}
	return "连接池最大连接数为 128，依据原文证据 [1]。", nil
}

func (r refusingSynth) Refused() bool { return r.refused }

// Gate G-pollute (fill): a refused synthesis must NOT persist a cluster —
// a cached non-answer poisons reuse. Sirchmunk saved a cluster with
// files_read=0 and answered from model memory; this suite refuses that.
func TestRefusedAnswerDoesNotPersistCluster(t *testing.T) {
	ctx := context.Background()
	fe := fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	fe.Synth = refusingSynth{refused: true}
	st := cluster.NewMemory()
	e := New(fe, st, cluster.Local{N: 64})
	srcs := fixtureSources()

	r, err := e.Ask(ctx, "连接池最大连接数是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Answer.Refused {
		t.Fatalf("synth refused, answer must carry the flag: %+v", r.Answer)
	}
	if r.Persisted || r.ClusterID != "" {
		t.Fatalf("refused answer must not persist a cluster (persisted=%v id=%s)", r.Persisted, r.ClusterID)
	}
	all, err := st.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("no cluster rows, got %d", len(all))
	}

	// Follow-up: nothing to reuse, still no cluster.
	r2, _ := e.Ask(ctx, "连接池最大连接数是多少", srcs)
	if r2.Persisted || r2.Reused {
		t.Fatalf("repeat of a refused query must not create/reuse a cluster: %+v", r2)
	}
	_ = source.StatusActive // keep the import used like the other kb tests
}

// Baseline: the SAME synth when NOT refused still persists normally — the
// gate must key on the refusal flag, not on the wording.
func TestNonRefusedAnswerStillPersists(t *testing.T) {
	ctx := context.Background()
	fe := fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	fe.Synth = refusingSynth{refused: false}
	e := New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	srcs := fixtureSources()
	r, err := e.Ask(ctx, "连接池最大连接数是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if r.Answer.Refused || !r.Persisted || r.ClusterID == "" {
		t.Fatalf("non-refused answer must persist: %+v", r)
	}
}

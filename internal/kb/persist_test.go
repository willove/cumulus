package kb

import (
	"context"
	"testing"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

func TestPersistMultiSourceAnswerAndCites(t *testing.T) {
	ctx := context.Background()
	a := source.New("A", "txt", "", "a", "en", "alpha", nil)
	b := source.New("B", "txt", "", "b", "en", "beta", nil)
	corpus := []source.Source{a, b}
	ans := fast.Answer{
		Query: "alpha and beta", Summary: "alpha beta", SourceID: a.ID, Confidence: 0.9,
		Samples: []mcs.Sample{
			{Source: a.ID, Start: 0, End: 5, Content: a.Body, Score: 8},
			{Source: b.ID, Start: 0, End: 4, Content: b.Body, Score: 8},
		},
	}
	e := New(nil, cluster.NewMemory(), cluster.Local{N: 64})
	cites := &memCites{}
	e.Cites = cites
	r, err := e.Persist(ctx, ans, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Persisted || len(cites.edges) != 2 || cites.edges[0].sourceID != a.ID || cites.edges[1].sourceID != b.ID {
		t.Fatalf("answer must persist with per-document cites: %+v, %+v", r, cites.edges)
	}
	reused, err := e.Ask(ctx, ans.Query, corpus)
	if err != nil {
		t.Fatal(err)
	}
	if !reused.Reused || reused.Answer.Summary != ans.Summary || len(reused.Answer.Samples) != 2 {
		t.Fatalf("multi-source evidence must validate without sampling: %+v", reused)
	}
	stored, err := e.Store.Get(ctx, r.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	if !e.priorStale(ctx, stored, []source.Source{a}) {
		t.Fatal("missing secondary source must invalidate the prior")
	}
}

func TestPersistRejectsNonAnswersWithoutReplacingCluster(t *testing.T) {
	ctx := context.Background()
	src := source.New("A", "txt", "", "a", "en", "alpha", nil)
	corpus := []source.Source{src}
	e := New(nil, cluster.NewMemory(), cluster.Local{N: 64})
	ans := fast.Answer{Query: "alpha", Summary: "alpha answer", SourceID: src.ID, Confidence: 0.9,
		Samples: []mcs.Sample{{Source: src.ID, Start: 0, End: 5, Content: src.Body, Score: 9}}}
	r, err := e.Persist(ctx, ans, corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"refused", "skipped"} {
		t.Run(kind, func(t *testing.T) {
			next := ans
			next.Summary = "alpha unavailable"
			next.Refused = kind == "refused"
			next.Skipped = kind == "skipped"
			got, err := e.Persist(ctx, next, corpus)
			if err != nil || got.Persisted {
				t.Fatalf("non-answer persisted: %+v, %v", got, err)
			}
			stored, err := e.Store.Get(ctx, r.ClusterID)
			if err != nil || stored.Content != ans.Summary {
				t.Fatalf("non-answer changed existing knowledge: %+v, %v", stored, err)
			}
		})
	}
}

func TestTidyPreservesMultiSourceEvidenceAndTopicReuse(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	emb := cluster.Local{N: 64}
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "最大", "上限", "128"}}), st, emb)
	a := source.New("A", "md", "", "a", "zh", "连接池最大 128。", nil)
	b := source.New("B", "md", "", "b", "zh", "连接池上限 128。", nil)
	q1, q2 := "连接池最大连接数是多少", "连接池最大连接数上限是多少"
	// Seed two sibling clusters (one source each) with the write-path fold
	// off, so THIS test owns the multi-source union via tidy. ReuseTheta=2
	// empties crossTopicNear; each new topic still creates its own cluster.
	e.ReuseTheta = 2.0
	for i, q := range []string{q1, q2} {
		r, err := e.Ask(ctx, q, []source.Source{[]source.Source{a, b}[i]})
		if err != nil || !r.Persisted {
			t.Fatalf("fixture must persist: %+v, %v", r, err)
		}
		if r.Merged {
			t.Fatalf("seed must not cross-merge (ReuseTheta=2): %+v", r)
		}
	}
	e.ReuseTheta = DefaultReuseTheta
	rep, err := cluster.Tidy(ctx, st, emb, 0, false, 0)
	if err != nil || rep.Merged != 1 {
		t.Fatalf("expected a tidy fold: %+v, %v", rep, err)
	}
	for _, q := range []string{q1, q2, q1, q2} {
		r, err := e.Ask(ctx, q, []source.Source{a, b})
		if err != nil || !r.Reused || len(r.Answer.Samples) != 2 {
			t.Fatalf("folded topic must reuse all evidence: %+v, %v", r, err)
		}
		ids := map[string]bool{}
		for _, sample := range r.Answer.Samples {
			ids[sample.Source] = true
		}
		if !ids[a.ID] || !ids[b.ID] {
			t.Fatalf("evidence lost source identity: %+v", r.Answer.Samples)
		}
	}
	all, err := st.All(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("search recreated the folded topic: %+v, %v", all, err)
	}
}

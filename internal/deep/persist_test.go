package deep

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

func TestFinalDeepAnswerPersistsAndReuses(t *testing.T) {
	for _, warmPartial := range []bool{false, true} {
		name := "fresh"
		if warmPartial {
			name = "partial-cache"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			corpus := []source.Source{
				source.New("A", "txt", "", "a", "en", "alpha alpha", nil),
				source.New("B", "txt", "", "b", "en", "beta", nil),
			}
			scorer := mcs.KeywordScorer{Keywords: []string{"alpha", "beta"}}
			e := New(kb.New(fast.New(scorer), cluster.NewMemory(), cluster.Local{N: 64}), NewMemoryConflict())
			e.Scorer = scorer
			query := "alpha and beta"
			if warmPartial {
				partial, err := e.KB.Ask(ctx, query, corpus)
				if err != nil || !partial.Persisted {
					t.Fatalf("fixture must cache partial answer: %+v, %v", partial, err)
				}
			}
			first, err := e.Ask(ctx, query, corpus)
			if err != nil {
				t.Fatal(err)
			}
			if first.Mode != ModeDEEP || !first.Cover.Complete || !first.Persisted || first.Reused {
				t.Fatalf("expected persisted final DEEP answer: %+v", first)
			}
			second, err := e.Ask(ctx, query, corpus)
			if err != nil {
				t.Fatal(err)
			}
			if !second.Reused || second.Escalated || !second.Cover.Complete || second.Answer.Summary != first.Answer.Summary {
				t.Fatalf("repeat must reuse complete final answer: first=%+v, second=%+v", first, second)
			}
			if second.ClusterID != first.ClusterID || len(second.Citations.Refs) != 2 {
				t.Fatalf("cache identity/evidence changed: %+v", second)
			}
			for _, ref := range second.Citations.Refs {
				if !ref.Resolved {
					t.Fatalf("reused multi-source citation must resolve: %+v", ref)
				}
			}
		})
	}
}

package deep

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// Consolidation absorbs neighbouring spans, but it has no body in scope to
// rebuild the text with. The kept window must therefore be re-read from its
// coordinates last — otherwise the span and the text describe different
// ranges, which is exactly what makes a citation fail to resolve.
func TestKeptWindowsResyncContentAfterConsolidation(t *testing.T) {
	body := strings.Repeat("0123456789", 10) // 100 runes, index-addressable
	b := []rune(body)
	src := source.Source{ID: "doc:1", Body: body}
	kept := []mcs.Sample{
		{Source: src.ID, Start: 0, End: 6, Content: string(b[0:6]), Score: 5},
		{Source: src.ID, Start: 40, End: 46, Content: string(b[40:46]), Score: 4},
	}
	got := topKeepsWith(kept, []source.Source{src})
	if len(got) != 1 {
		t.Fatalf("windows = %d, want the two spans consolidated into one", len(got))
	}
	w := got[0]
	if want := string(b[w.Start:w.End]); w.Content != want {
		t.Fatalf("kept window [%d,%d) carries %q, want %q — the text and its coordinates must describe the same span",
			w.Start, w.End, w.Content, want)
	}
}

// Widening admits documents the run did not start with. Citations must resolve
// against what was actually sampled, or a widened window shows up as an
// untitled, unresolved ref even though the answer was built from it.
func TestWidenedSourcesResolveAsCitations(t *testing.T) {
	ctx := context.Background()
	thin := source.New("薄手册", "md", "", "thin", "zh", "无关段落填充。", nil)
	rich := source.New("扩征手册", "md", "", "rich", "zh", "连接池最大 128。", nil)
	e := New(kb.New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}),
		cluster.NewMemory(), cluster.Local{N: 64}), NewMemoryConflict())
	widenCalls := 0
	e.Widen = func(context.Context, string, map[string]bool, int, map[string]bool) ([]source.Source, error) {
		widenCalls++
		return []source.Source{rich}, nil
	}

	res, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{thin})
	if err != nil {
		t.Fatal(err)
	}
	if widenCalls == 0 {
		t.Skip("the thin corpus answered without widening; nothing to check")
	}
	var got *Ref
	for i := range res.Citations.Refs {
		if res.Citations.Refs[i].SourceID == rich.ID {
			got = &res.Citations.Refs[i]
		}
	}
	if got == nil {
		t.Fatalf("citations = %+v, want the widened source %s cited", res.Citations.Refs, rich.ID)
	}
	if !got.Resolved || got.Title != "扩征手册" {
		t.Fatalf("widened citation = %+v, want it resolved with its title", *got)
	}
}

// A warm reuse hit hands DEEP the narrow anchor set. Escalation must still load
// the full corpus: the anchors are not a DEEP corpus, and confining sampling
// (and widening) to them silently reduces the search to the reused cluster's
// own sources.
func TestEscalationLoadsCorpusDespiteNarrowAnchors(t *testing.T) {
	ctx := context.Background()
	narrow := source.New("锚点手册", "md", "", "anchor", "zh", "连接池最大 128。", nil)
	full := []source.Source{
		narrow,
		source.New("另一份手册", "md", "", "other", "zh", "连接池最大 256，超时 30 秒。", nil),
	}
	e := New(kb.New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128", "256"}}),
		cluster.NewMemory(), cluster.Local{N: 64}), NewMemoryConflict())

	loads := 0
	load := func(context.Context) ([]source.Source, error) {
		loads++
		return full, nil
	}
	query := "连接池最大连接数是多少"
	fx := facts.Build(query)
	// A base with no samples must escalate.
	base := kb.Result{Answer: fast.Answer{Query: query, Mode: fast.ModeFAST, Skipped: true}}
	res, err := e.afterBase(ctx, time.Now(), query, base, []source.Source{narrow}, e.thresholdFor(fx), fx, load)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated {
		t.Fatalf("want escalation, got %+v", res.Answer)
	}
	if loads != 1 {
		t.Fatalf("escalation loaded the corpus %d times, want 1 — the narrow anchor set is not a DEEP corpus", loads)
	}
	cited := false
	for _, r := range res.Citations.Refs {
		if r.SourceID == full[1].ID {
			cited = true
		}
	}
	if !cited {
		t.Fatalf("citations = %+v, want the non-anchor document %s sampled and cited", res.Citations.Refs, full[1].ID)
	}
}

// The budget must stop every paying stage, not just initial admission: a gate
// guarding only the first loop lets self-correction and widening call the model
// afterwards.
func TestTokenBudgetStopsEveryPayingStage(t *testing.T) {
	ctx := context.Background()
	e := New(kb.New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}),
		cluster.NewMemory(), cluster.Local{N: 64}), NewMemoryConflict())
	// A corpus that cannot answer, so every stage would otherwise run.
	var srcs []source.Source
	for i := 0; i < 6; i++ {
		srcs = append(srcs, source.New("手册", "md", "", "cfg", "zh",
			strings.Repeat("无关填充 padding。\n", 12), nil))
	}
	scored, widenCalls, simCalls := 0, 0, 0
	e.OnFile = func(string, string, float64, int) { scored++ }
	e.Widen = func(context.Context, string, map[string]bool, int, map[string]bool) ([]source.Source, error) {
		widenCalls++
		return nil, nil
	}
	e.QuerySim = countingQuerySim{calls: &simCalls}
	e.TokenBudget = 1
	e.TokensUsed = func() int64 { return 99 }

	if _, err := e.Ask(ctx, "连接池最大连接数是多少", srcs); err != nil {
		t.Fatal(err)
	}
	if scored != 0 {
		t.Fatalf("%d files were scored after the budget was spent", scored)
	}
	if widenCalls != 0 {
		t.Fatalf("widening ran %d times after the budget was spent", widenCalls)
	}
	if simCalls != 0 {
		t.Fatalf("the query simulator ran %d times after the budget was spent", simCalls)
	}
	if !e.BudgetHit {
		t.Fatal("BudgetHit must record the stop")
	}
}

type countingQuerySim struct{ calls *int }

func (c countingQuerySim) Complement(context.Context, string, []string) ([]string, error) {
	*c.calls++
	return nil, nil
}

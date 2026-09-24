package fast

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// countingExpander is a KeywordExpander that records how many times it ran, so
// the FAST call-accounting gate can be asserted with the expander WIRED (the
// only place the old hardcoded LLMCalls:2 was wrong).
type countingExpander struct{ calls int }

func (c *countingExpander) Expand(_ context.Context, _ string, _ int) ([][]string, error) {
	c.calls++
	return [][]string{{"连接池", "最大"}}, nil
}

// M2: the expander is a third LLM-shaped call. LLMCalls used to be hardcoded 2,
// so the D5/§6.2 gate ("FAST 档 LLM 调用次数 ≤2，桩下可断言") was untested
// exactly where it could fail, and eval-run recorded the wrong cost.
func TestFASTCountsExpanderCall(t *testing.T) {
	ctx := context.Background()
	srcs := []source.Source{
		source.New("手册", "md", "file://d", "d", "zh", "连接池最大 128，超时 30 秒。", nil),
		source.New("别的", "md", "file://e", "e", "zh", "端口与机房说明。", nil),
	}
	// No expander: analyze + synthesize = 2.
	e := New(mcs.KeywordScorer{})
	ans, err := e.Search(ctx, "连接池最大是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if ans.LLMCalls != 2 {
		t.Fatalf("without an expander LLMCalls=%d, want 2", ans.LLMCalls)
	}

	// With an expander that fires: analyze + expander = 2, and NO synthesis.
	//
	// Three is unreachable offline, and that is the point: the expander only
	// runs when the primary/fallback cascade found NOTHING, i.e. the query
	// shares no keyword with any body — and then the offline KeywordScorer
	// scores every window 0, so no evidence is kept and no synthesis happens.
	// Billing a third call there (as the old hardcoded 2 did on the normal path)
	// is exactly what this accounting is meant to stop.
	exp := &countingExpander{}
	e2 := New(mcs.KeywordScorer{})
	e2.Expander = exp
	ans2, err := e2.Search(ctx, "完全无关的问法 xyzzy", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if exp.calls == 0 {
		t.Fatal("precondition: the expander must have run")
	}
	if ans2.LLMCalls != 2 {
		t.Fatalf("with an expander and no usable evidence LLMCalls=%d, want 2 (analyze+expander)", ans2.LLMCalls)
	}
	if !ans2.Skipped {
		t.Fatalf("no evidence must be reported as skipped: %+v", ans2)
	}
}

// M3: UsePrior must REORDER the cascade's hits, not generate its own candidate
// set. prior.Rank scores every active source (scan floor 0.3 + 0.2 active), so
// it never returns empty — feeding it through directly made a zero-signal query
// return an arbitrary pick and made the plain-cascade fallback dead code.
func TestUsePriorReranksButDoesNotInventCandidates(t *testing.T) {
	ctx := context.Background()
	srcs := []source.Source{
		source.New("手册", "md", "file://d", "d", "zh", "连接池最大 128，超时 30 秒。", nil),
		source.New("别的", "md", "file://e", "e", "zh", "端口与机房说明。", nil),
	}
	plain := New(mcs.KeywordScorer{})
	withPrior := New(mcs.KeywordScorer{})
	withPrior.UsePrior = true

	// A query with ZERO keyword overlap anywhere: the plain cascade finds
	// nothing and Search must skip. The prior must not resurrect a candidate.
	q := "zzzzz qqqqq"
	a, err := plain.Search(ctx, q, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Skipped || a.SourceID != "" {
		t.Fatalf("plain path must skip a zero-signal query: %+v", a)
	}
	b, err := withPrior.Search(ctx, q, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Skipped || b.SourceID != "" {
		t.Fatalf("UsePrior must not invent a candidate for a zero-signal query: %+v", b)
	}

	// With a real signal the prior still reorders — and it can only reorder
	// files the cascade actually matched.
	c, err := withPrior.Search(ctx, "连接池最大是多少", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if c.Skipped || c.SourceID != srcs[0].ID {
		t.Fatalf("prior path must still answer from the matching file: %+v", c)
	}
	got := withPrior.rankFields(mcs.Fields("连接池最大是多少"), srcs)
	if len(got) != 1 || got[0].src.ID != srcs[0].ID {
		t.Fatalf("prior must reorder only cascade hits: %+v", got)
	}
}

// The prior must FUSE with the cascade, not replace it. Sorting purely by prior
// score buried a strong cascade hit, because prior.Rank normalises its own top
// file to 1.0 and drops everything past its topK (so an unranked cascade hit
// scored 0 and sank below every prior entry).
func TestUsePriorFusesInsteadOfReplacing(t *testing.T) {
	srcs := []source.Source{
		// Dominant lexical match for the query below.
		source.New("强命中", "md", "file://d", "d", "zh",
			"连接池最大 128 连接池最大 128 连接池最大 128 连接池最大 128 连接池最大 128", nil),
		source.New("弱命中", "md", "file://e", "e", "zh", "连接池上限很高。", nil),
	}
	e := New(mcs.KeywordScorer{})
	e.UsePrior = true
	fields := mcs.Fields("连接池最大是多少")
	ranked := e.rankFields(fields, srcs)
	if len(ranked) != 2 {
		t.Fatalf("both cascade hits must survive the re-rank: %+v", ranked)
	}
	// The dominant lexical hit must stay first even though the prior may score
	// the two files differently.
	if ranked[0].src.ID != srcs[0].ID {
		t.Fatalf("cascade's dominant hit was buried by the prior: %v", []string{ranked[0].src.ID, ranked[1].src.ID})
	}
	// And a non-zero overlap in the query is still required — the prior must not
	// resurrect a file the cascade never matched.
	none := e.rankFields(mcs.Fields("zzzzz qqqqq"), srcs)
	if len(none) != 0 {
		t.Fatalf("prior must not invent candidates: %+v", none)
	}
	// The fused score is in [0,1] for every entry.
	for _, sc := range ranked {
		if sc.score < 0 || sc.score > 1 {
			t.Fatalf("fused score out of range: %v", sc.score)
		}
	}
}

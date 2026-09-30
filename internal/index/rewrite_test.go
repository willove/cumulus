package index

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

// TestRewriteWhenEmpty is the "帮信罪 → 帮助信息网络犯罪活动罪" case:
// BM25 returns zero for the colloquial term, the rewriter produces the
// formal term, BM25 finds the gold document.
func TestRewriteWhenEmpty(t *testing.T) {
	srcs := []source.Source{
		{ID: "a", Body: "专利法保护专利权人合法权益", Status: source.StatusActive},
		{ID: "b", Body: "商标法保护商标专用权", Status: source.StatusActive},
		{ID: "c", Body: "帮助信息网络犯罪活动罪 刑法第二百八十七条之二", Status: source.StatusActive},
	}
	idx := Build(srcs)

	// Simulate: LLM rewrites "帮信罪" → "帮助信息网络犯罪活动罪"
	stub := func(_ context.Context, q string) (string, error) {
		if q == "帮信罪是什么" {
			return "帮助信息网络犯罪活动罪是什么", nil
		}
		return "", nil
	}

	// BM25 first pass: "帮信罪" finds nothing (bigram 帮信 doesn't exist).
	// Rewriter fires: finds the formal term → BM25 retry → gold doc found.
	top := idx.RewriteWhenEmpty(context.Background(), "帮信罪是什么", srcs, 10, stub)
	found := false
	for _, s := range top {
		if s.ID == "c" {
			found = true
		}
	}
	if !found {
		t.Fatalf("rewrite must surface the gold doc (帮助信息网络犯罪活动罪), got %d results", len(top))
	}
}

// TestRewriteWhenEmptyNoTrigger: BM25 finds results → no rewrite attempted.
func TestRewriteWhenEmptyNoTrigger(t *testing.T) {
	srcs := []source.Source{
		{ID: "a", Body: "专利法第一条", Status: source.StatusActive},
		{ID: "b", Body: "专利法第二条", Status: source.StatusActive},
		{ID: "c", Body: "专利法第三条", Status: source.StatusActive},
		{ID: "d", Body: "专利法第四条", Status: source.StatusActive},
		// Noise: makes 专利 rarer → higher IDF → higher BM25 score.
		{ID: "n1", Body: "民法典调整民事关系", Status: source.StatusActive},
		{ID: "n2", Body: "刑法规定犯罪与刑罚", Status: source.StatusActive},
		{ID: "n3", Body: "行政法规范行政行为", Status: source.StatusActive},
		{ID: "n4", Body: "商标法保护商标权", Status: source.StatusActive},
		{ID: "n5", Body: "劳动法保障劳动者权益", Status: source.StatusActive},
	}
	idx := Build(srcs)
	rewrote := false
	stub := func(_ context.Context, _ string) (string, error) {
		rewrote = true
		return "", nil
	}
	top := idx.RewriteWhenEmpty(context.Background(), "专利法", srcs, 10, stub)
	if rewrote {
		t.Fatal("good results (≥MinRecall) must not trigger rewrite")
	}
	if len(top) == 0 {
		t.Fatal("should have results")
	}
}

// TestRewriteFiresOnVocabGapDespiteNoiseScore pins the production pathology
// measured on laws-full (2026-10-01): "帮信罪是什么" noise-scores 14.2 — far
// above MinRewriteScore — because fragment bigrams (信罪/罪是/什么) exist in
// unrelated documents, so the absolute-score trigger alone never fires and
// the vocabulary gap stays invisible. The idf² VocabGapFraction is what
// catches it: the query's discriminative bigrams (帮信/是什) have df=0.
// Regression shape: noise score ABOVE the line + gap fraction ≥ 0.5 →
// rewrite must still fire and surface the formal-name document.
func TestRewriteFiresOnVocabGapDespiteNoiseScore(t *testing.T) {
	srcs := []source.Source{
		// Gold: formal crime name (bigrams 帮助/助信/信息/… — no 帮信).
		{ID: "gold", Body: "帮助信息网络犯罪活动罪 刑法第二百八十七条之二 明知他人利用信息网络实施犯罪", Status: source.StatusActive},
		// Fragment docs: give the query's generic bigrams nonzero df so BM25
		// produces a confident-looking NOISE top score (the 数罪并罚批复 effect).
		{ID: "f1", Body: "背信罪是指违背信任委托的犯罪行为 背信罪是指违背信任委托的犯罪行为", Status: source.StatusActive},
		{ID: "f2", Body: "什么行为构成犯罪 什么行为构成犯罪 什么行为构成犯罪", Status: source.StatusActive},
		{ID: "f3", Body: "盗窃罪是侵犯财产的犯罪 盗窃罪是侵犯财产的犯罪", Status: source.StatusActive},
	}
	idx := Build(srcs)

	// Pin the pathology precondition: noise clears the absolute line…
	if top := idx.TopBM25Score("帮信罪是什么"); top < MinRewriteScore {
		t.Fatalf("fixture must reproduce noise above the absolute line, got topScore=%.3f", top)
	}
	// …yet the vocabulary gap betrays it.
	if g := idx.VocabGapFraction("帮信罪是什么"); g < RewriteGapFraction {
		t.Fatalf("fixture must reproduce the vocab gap, got gapFraction=%.3f", g)
	}

	stub := func(_ context.Context, q string) (string, error) {
		if q == "帮信罪是什么" {
			return "帮助信息网络犯罪活动罪是什么", nil
		}
		return "", nil
	}
	top := idx.RewriteWhenEmpty(context.Background(), "帮信罪是什么", srcs, 10, stub)
	if len(top) == 0 || top[0].ID != "gold" {
		var ids []string
		for _, s := range top {
			ids = append(ids, s.ID)
		}
		t.Fatalf("vocab-gap rewrite must surface the gold doc first, got %v", ids)
	}
}

// TestRewriteWhenEmptyStillEmpty: rewrite produces a term the corpus
// doesn't have → honest empty result (no fabrication).
func TestRewriteWhenEmptyStillEmpty(t *testing.T) {
	srcs := []source.Source{
		{ID: "a", Body: "专利法", Status: source.StatusActive},
	}
	idx := Build(srcs)
	stub := func(_ context.Context, _ string) (string, error) {
		return "完全不相关的改写", nil
	}
	top := idx.RewriteWhenEmpty(context.Background(), "某个词", srcs, 10, stub)
	if len(top) > 0 {
		t.Fatal("irrelevant rewrite must still yield empty — no fabrication")
	}
}

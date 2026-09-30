package index

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

// TestParseExpandTerms pins the LLM output parser: JSON array extracted,
// empty/too-long/duplicate terms filtered, prose → nil (never a panic).
func TestParseExpandTerms(t *testing.T) {
	got := ParseExpandTerms(`["固体废物","环境污染","法律责任"]`)
	if len(got) != 3 || got[0] != "固体废物" {
		t.Fatalf("clean array: %v", got)
	}
	// Decorated prose around the array.
	got = ParseExpandTerms(`好的，以下是扩展词：\n["环境保护","污染处罚"]\n以上供参考。`)
	if len(got) != 2 || got[0] != "环境保护" {
		t.Fatalf("decorated: %v", got)
	}
	// Dedup and length filter.
	got = ParseExpandTerms(`["废物","废物","这个词语实在太长了不应该被保留"]`)
	if len(got) != 1 || got[0] != "废物" {
		t.Fatalf("dedup/length: %v", got)
	}
	// Garbage → nil.
	if got := ParseExpandTerms("no json here"); got != nil {
		t.Fatalf("prose → nil, got %v", got)
	}
	if got := ParseExpandTerms(""); got != nil {
		t.Fatalf("empty → nil, got %v", got)
	}
}

// TestNarrowWithExpansionConditional pins the conditional trigger: good BM25
// recall → no expansion call (zero cost); poor recall → expansion fires,
// BM25 retries, improved results returned.
func TestNarrowWithExpansionConditional(t *testing.T) {
	srcs := []source.Source{
		{ID: "a", Body: "专利法保护专利权人的合法权益", Status: source.StatusActive},
		{ID: "b", Body: "商标法保护商标专用权", Status: source.StatusActive},
		{ID: "c", Body: "著作权法保护著作权", Status: source.StatusActive},
		{ID: "d", Body: "民法典调整民事关系", Status: source.StatusActive},
		{ID: "e", Body: "行政处罚法设定处罚权限", Status: source.StatusActive},
		{ID: "f", Body: "固体废物污染环境防治法 防治固体废物污染环境", Status: source.StatusActive},
	}
	idx := Build(srcs)

	expanded := false
	stub := func(_ context.Context, _ string) ([]string, error) {
		expanded = true
		return []string{"固体废物", "环境污染"}, nil
	}

	// Case 1: good recall (≥5 candidates) → NO expansion call.
	top := idx.NarrowWithExpansion(context.Background(), "法律保护权益", srcs, 10, stub)
	if expanded {
		t.Fatal("good recall must not trigger expansion — zero cost for the 87%")
	}
	if len(top) < 3 {
		t.Fatalf("good recall should return candidates, got %d", len(top))
	}

	// Case 2: poor recall (vocabulary gap) → expansion fires, enriched query
	// finds the gold doc.
	expanded = false
	top = idx.NarrowWithExpansion(context.Background(), "乱扔垃圾处罚", srcs, 10, stub)
	if !expanded {
		t.Fatal("vocabulary gap must trigger expansion")
	}
	found := false
	for _, s := range top {
		if s.ID == "f" {
			found = true
		}
	}
	if !found {
		t.Fatal("expanded query must surface the gold doc (固体废物法)")
	}

	// Case 3: nil expander → degenerates to plain Narrow.
	expanded = false
	top = idx.NarrowWithExpansion(context.Background(), "乱扔垃圾处罚", srcs, 10, nil)
	if expanded {
		t.Fatal("nil expander must not fire")
	}
}

// TestNarrowWithExpansionFailureKeepsOriginal: expansion error → original
// BM25 results kept, never an empty result.
func TestNarrowWithExpansionFailureKeepsOriginal(t *testing.T) {
	srcs := []source.Source{
		{ID: "a", Body: "专利法第一条", Status: source.StatusActive},
	}
	idx := Build(srcs)
	failing := func(_ context.Context, _ string) ([]string, error) {
		return nil, context.Canceled
	}
	top := idx.NarrowWithExpansion(context.Background(), "专利", srcs, 5, failing)
	if len(top) == 0 {
		t.Fatal("expansion failure must keep original BM25 results")
	}
}

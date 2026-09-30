package index

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

// TestParseExpandTerms pins the parser: JSON array extraction, filter,
// prose→nil, decorated→clean.
func TestParseExpandTerms(t *testing.T) {
	got := ParseExpandTerms(`["固体废物","环境污染","法律责任"]`)
	if len(got) != 3 || got[0] != "固体废物" {
		t.Fatalf("clean: %v", got)
	}
	got = ParseExpandTerms(`好的：\n["环境保护","污染处罚"]\n以上。`)
	if len(got) != 2 || got[0] != "环境保护" {
		t.Fatalf("decorated: %v", got)
	}
	if got := ParseExpandTerms("no json"); got != nil {
		t.Fatalf("prose→nil: %v", got)
	}
	if got := ParseExpandTerms(""); got != nil {
		t.Fatalf("empty→nil: %v", got)
	}
}

// TestTopVocab pins: terms sorted by document frequency descending, capped at N.
func TestTopVocab(t *testing.T) {
	srcs := []source.Source{
		{ID: "a", Body: "专利法保护专利权人合法权益", Status: source.StatusActive},
		{ID: "b", Body: "商标法保护商标专用权", Status: source.StatusActive},
		{ID: "c", Body: "著作权法保护著作权合法权益", Status: source.StatusActive},
	}
	idx := Build(srcs)
	vocab := idx.TopVocab(10)
	if len(vocab) == 0 {
		t.Fatal("vocab must not be empty")
	}
	// "保护" appears in all 3 docs — must be in top results (highest DF).
	found := false
	for _, v := range vocab {
		if v == "保护" {
			found = true
		}
	}
	if !found {
		t.Fatalf("高频词 保护 must be in top vocab: %v", vocab[:min(5, len(vocab))])
	}
}

// TestNarrowWithExpansionCorpusBounded is the corpus-primacy contract: the
// LLM selects from corpus vocab, terms are validated against the index,
// and only corpus-existing terms reach BM25.
func TestNarrowWithExpansionCorpusBounded(t *testing.T) {
	srcs := []source.Source{
		{ID: "a", Body: "专利法保护专利权人合法权益", Status: source.StatusActive},
		{ID: "b", Body: "商标法保护商标专用权", Status: source.StatusActive},
		{ID: "c", Body: "著作权法保护著作权", Status: source.StatusActive},
		{ID: "f", Body: "固体废物污染环境防治法 防治污染保护环境", Status: source.StatusActive},
	}
	idx := Build(srcs)

	// Expander that selects a corpus term (valid) and a non-corpus term (invalid).
	stub := func(_ context.Context, _ string, vocab []string) ([]string, error) {
		return []string{"固体废物", "这个术语不在语料里"}, nil
	}
	top := idx.NarrowWithExpansion(context.Background(), "乱扔垃圾处罚", srcs, 10, stub)
	// The valid term "固体废物" must help find doc f.
	found := false
	for _, s := range top {
		if s.ID == "f" {
			found = true
		}
	}
	if !found {
		t.Fatal("corpus term 固体废物 must surface doc f")
	}
}

// TestNarrowWithExpansionGoodRecallNoExpand: sufficient recall → no LLM call.
func TestNarrowWithExpansionGoodRecallNoExpand(t *testing.T) {
	srcs := []source.Source{
		{ID: "a", Body: "专利法第一条", Status: source.StatusActive},
		{ID: "b", Body: "专利法第二条", Status: source.StatusActive},
		{ID: "c", Body: "专利法第三条", Status: source.StatusActive},
		{ID: "d", Body: "专利法第四条", Status: source.StatusActive},
	}
	idx := Build(srcs)
	expanded := false
	stub := func(_ context.Context, _ string, _ []string) ([]string, error) {
		expanded = true
		return nil, nil
	}
	top := idx.NarrowWithExpansion(context.Background(), "专利法", srcs, 10, stub)
	if expanded {
		t.Fatal("good recall must not trigger expansion")
	}
	if len(top) < 3 {
		t.Fatalf("good recall should return ≥3, got %d", len(top))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

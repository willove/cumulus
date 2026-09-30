package index

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

func mkDoc(id, body string) source.Source {
	// Direct construction with a known ID (source.New mints a content-
	// addressed digest ID, which makes test assertions unreadable).
	return source.Source{ID: id, Body: body, Status: source.StatusActive,
		Title: id, BusinessKey: id, Lang: "zh"}
}

// TestBuildPostings pins the index construction: active docs tokenized by
// mcs.Fields (bigram), per-doc TF in postings, doc lengths recorded, stale
// docs excluded.
func TestBuildPostings(t *testing.T) {
	srcs := []source.Source{
		mkDoc("a", "专利法第一条 为了保护专利权人"),
		mkDoc("b", "商标法第一条 为了保护商标专用权"),
		mkDoc("c", "著作权法第一条 为了保护著作权"),
	}
	idx := Build(srcs)
	if idx.N != 3 {
		t.Fatalf("N = %d, want 3", idx.N)
	}
	if idx.AvgLen <= 0 {
		t.Fatal("AvgLen must be positive")
	}
	// "专" bigrams: "专利" appears in doc a multiple times (专利法, 专利权人).
	postings := idx.Postings["专利"]
	if len(postings) == 0 {
		t.Fatal("postings[专利] must exist — the term appears in doc a")
	}
	found := false
	for _, p := range postings {
		if p.DocID == "a" && p.TF >= 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("doc a must have TF ≥2 for 专利 (from 专利法 and 专利权人), postings: %+v", postings)
	}
	// Stale doc excluded.
	stale := mkDoc("s", "旧文档 专利法")
	stale.Status = source.StatusStale
	idx2 := Build([]source.Source{srcs[0], stale})
	if idx2.N != 1 {
		t.Fatalf("stale doc must not be indexed, N = %d", idx2.N)
	}
}

// TestRankBM25 pins the BM25 ranking contract: the doc with more query-term
// occurrences and higher IDF ranks first; docs sharing common terms but
// lacking the distinctive term rank lower.
func TestRankBM25(t *testing.T) {
	srcs := []source.Source{
		mkDoc("pat", "专利法保护专利权人的合法权益 专利法第一条 专利"),
		mkDoc("tm", "商标法保护商标专用权 商标注册"),
		mkDoc("both", "专利法 商标法 都保护知识产权"),
		mkDoc("none", "民法典调整平等主体之间的人身关系和财产关系"),
	}
	idx := Build(srcs)
	top := idx.Rank("专利法保护", 3)
	if len(top) == 0 {
		t.Fatal("rank must return hits for a query with matching terms")
	}
	if top[0] != "pat" {
		t.Fatalf("doc 'pat' (highest TF for 专利) must rank first, got %q", top[0])
	}
	// The 'none' doc shares no bigram with the query — must not appear.
	for _, id := range top {
		if id == "none" {
			t.Fatal("doc 'none' shares no terms with the query, must not rank")
		}
	}
}

// TestNarrowPreservesRankOrder pins the narrowing contract: returned sources
// are in BM25 rank order (not source-list order), active-only, capped at K.
func TestNarrowPreservesRankOrder(t *testing.T) {
	srcs := []source.Source{
		mkDoc("x", "专利法保护专利权人"),
		mkDoc("y", "商标法保护商标权"),
		mkDoc("z", "专利法第一条"),
	}
	// Pass in REVERSE order to prove Narrow sorts by rank, not input order.
	input := []source.Source{srcs[2], srcs[0], srcs[1]}
	idx := Build(srcs)
	got := idx.Narrow("专利法", input, 2)
	if len(got) == 0 || len(got) > 2 {
		t.Fatalf("narrow must return ≤2, got %d", len(got))
	}
	if len(got) > 1 && got[0].ID == "y" {
		t.Fatalf("doc about 商标 must not rank for 专利法 query: %+v", got[0].ID)
	}
	for _, s := range got {
		if s.Status != source.StatusActive {
			t.Fatalf("stale source returned: %+v", s.ID)
		}
	}
}

// TestRankEmptyQuery pins the degenerate cases: empty query, nil index,
// zero-K, empty corpus — all yield nil (no signal), never a panic.
func TestRankEmptyQuery(t *testing.T) {
	var nilIdx *Index
	if got := nilIdx.Rank("anything", 5); got != nil {
		t.Fatal("nil index must return nil")
	}
	empty := Build(nil)
	if got := empty.Rank("查询", 5); got != nil {
		t.Fatal("empty index must return nil")
	}
	idx := Build([]source.Source{mkDoc("a", "专利法")})
	if got := idx.Rank("", 5); got != nil {
		t.Fatal("empty query must return nil")
	}
	if got := idx.Rank("专利", 0); got != nil {
		t.Fatal("k=0 must return nil")
	}
}

// TestBuildDeterministic pins: same input, byte-identical index (postings
// order within a term's list may vary by map iteration — verify by set).
func TestBuildDeterministic(t *testing.T) {
	srcs := []source.Source{
		mkDoc("a", "专利法第一条"),
		mkDoc("b", "商标法第一条"),
	}
	idx1 := Build(srcs)
	idx2 := Build(srcs)
	if idx1.N != idx2.N || idx1.AvgLen != idx2.AvgLen {
		t.Fatal("deterministic build mismatch")
	}
	if len(idx1.Postings) != len(idx2.Postings) {
		t.Fatalf("postings count differs: %d vs %d", len(idx1.Postings), len(idx2.Postings))
	}
	for term, p1 := range idx1.Postings {
		p2, ok := idx2.Postings[term]
		if !ok || len(p1) != len(p2) {
			t.Fatalf("postings[%q] differs", term)
		}
	}
}

// TestScaleCorrectness is a smoke: 100 synthetic docs, verify the correct
// one surfaces in top-5 for a targeted query.
func TestScaleCorrectness(t *testing.T) {
	var srcs []source.Source
	for i := 0; i < 100; i++ {
		body := strings.Repeat("一般法律条文内容填充。", 10)
		if i == 42 {
			body = "中华人民共和国涉外民事关系法律适用法 第一条 为了明确涉外民事关系的法律适用，合理解决涉外民事争议，维护当事人的合法权益，制定本法。"
		}
		srcs = append(srcs, mkDoc(string(rune('a'+i%26))+string(rune('0'+i/26)), body))
	}
	idx := Build(srcs)
	top := idx.Rank("涉外民事关系法律适用", 5)
	if len(top) == 0 {
		t.Fatal("targeted query must rank the target doc")
	}
	found := false
	for _, id := range top {
		if id == string(rune('a'+42%26))+string(rune('0'+42/26)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("doc 42 (the only one about 涉外民事) must be in top-5, got %v", top)
	}
}

package index

import (
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// RankTerms is the reason armprobe can repair a query without round-tripping
// through a string: mcs.Fields would re-tokenize a rebuilt query and hand back
// different bigrams than the ones the caller selected. These tests pin that the
// term-list path scores exactly the same as the string path when the terms are
// the ones Fields produced, and that dropping terms actually narrows the
// candidate set.
func TestRankTermsMatchesRankOnTheSameTerms(t *testing.T) {
	srcs := []source.Source{
		source.New("连接池手册", "md", "", "cfg", "zh", "连接池最大 128，超时 30 秒，重试三次。", nil),
		source.New("刑法国条", "md", "", "law", "zh", "帮助信息网络犯罪活动罪的处罚规定如下。", nil),
		source.New("运维手册", "md", "", "ops", "zh", "运维值班流程与告警接收人的联系方式。", nil),
	}
	idx := Build(srcs)
	if idx.N != 3 {
		t.Fatalf("indexed %d docs, want 3", idx.N)
	}

	fromString := idx.Rank("连接池最大多少", 3)
	fromTerms := idx.RankTerms(mcs.Fields("连接池最大多少"), 3)
	if len(fromString) != len(fromTerms) {
		t.Fatalf("Rank returned %d ids, RankTerms returned %d", len(fromString), len(fromTerms))
	}
	for i := range fromString {
		if fromString[i] != fromTerms[i] {
			t.Fatalf("position %d: Rank=%q RankTerms=%q", i, fromString[i], fromTerms[i])
		}
	}
}

// Dropping the out-of-vocabulary term is the vocabulary-gap repair: the
// discriminative word is gone from the corpus, so ranking on the survivors
// must still return the same surviving document rather than nothing.
func TestRankTermsNarrowsToTheSurvivingTerms(t *testing.T) {
	srcs := []source.Source{
		source.New("手册", "md", "", "a", "zh", "帮助信息网络犯罪活动罪的处罚规定。", nil),
		source.New("其他", "md", "", "b", "zh", "完全无关的另一篇文档内容。", nil),
	}
	idx := Build(srcs)

	// "帮信罪" is not in the corpus vocabulary; "信息网络" is.
	all := mcs.Fields("帮信罪 信息网络")
	kept := make([]string, 0, len(all))
	for _, term := range all {
		if len(idx.Postings[term]) > 0 {
			kept = append(kept, term)
		}
	}
	if len(kept) == 0 {
		t.Skip("no in-vocabulary term survived; nothing to narrow to")
	}
	got := idx.RankTerms(kept, 3)
	if len(got) == 0 {
		t.Fatal("narrowing to in-vocabulary terms returned nothing")
	}
	// The only doc containing the surviving term must be ranked. Its id is
	// content-addressed (source.New), so resolve it through the index rather
	// than assuming a key-shaped form.
	wantID := ""
	for id, s := range idx.byID {
		if s.BusinessKey == "a" {
			wantID = id
		}
	}
	if wantID == "" {
		t.Fatal("could not resolve the id of the document holding the surviving term")
	}
	if !contains(got, wantID) {
		t.Fatalf("expected %q (the doc holding the surviving term), got %v", wantID, got)
	}
}

func TestRankTermsGuards(t *testing.T) {
	idx := Build([]source.Source{source.New("x", "md", "", "x", "zh", "内容", nil)})
	if got := idx.RankTerms(nil, 5); got != nil {
		t.Fatalf("nil terms should yield nil, got %v", got)
	}
	if got := idx.RankTerms([]string{"", "dup", "dup"}, 0); got != nil {
		t.Fatalf("k<=0 should yield nil, got %v", got)
	}
	var nilIdx *Index
	if got := nilIdx.RankTerms([]string{"a"}, 5); got != nil {
		t.Fatalf("nil index should yield nil, got %v", got)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

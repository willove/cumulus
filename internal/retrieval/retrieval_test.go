package retrieval

import (
	"reflect"
	"testing"
)

func TestFieldsCJKBigrams(t *testing.T) {
	got := Fields("连接池配置")
	want := []string{"连接", "接池", "池配", "配置"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestFieldsAlnumWholeToken(t *testing.T) {
	got := Fields("BM25 index v2")
	want := []string{"bm25", "index", "v2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestFieldsMixedAndPunctuation(t *testing.T) {
	// 标点断段；中文出二元组，英文整词
	got := Fields("最大连接数 max=100")
	want := []string{"最大", "大连", "连接", "接数", "max", "100"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func corpus() []Document {
	return []Document{
		{ID: "d1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列。"},
		{ID: "d2", Body: "部署手册：先改配置，再重启服务；端口默认 8484。"},
		{ID: "d3", Body: "财务报表：三季度收入增长，成本结构优化。"},
	}
}

func TestRankOrdersByRelevance(t *testing.T) {
	idx := Build(corpus())
	got := idx.Rank("连接池最大连接数", 3)
	if len(got) == 0 || got[0] != "d1" {
		t.Fatalf("d1 must rank first, got %v", got)
	}
	if len(got) > 1 && got[1] == "d3" {
		t.Fatalf("irrelevant doc must not outrank d2: %v", got)
	}
}

func TestRankDeterministic(t *testing.T) {
	idx := Build(corpus())
	a := idx.Rank("连接池 配置", 3)
	b := idx.Rank("连接池 配置", 3)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("ranking must be deterministic: %v vs %v", a, b)
	}
}

func TestRankTermsMatchesRankOnSameTerms(t *testing.T) {
	idx := Build(corpus())
	q := "连接池最大连接数"
	viaRank := idx.Rank(q, 2)
	viaTerms := idx.RankTerms(UniqueTerms(Fields(q)), 2)
	if !reflect.DeepEqual(viaRank, viaTerms) {
		t.Fatalf("RankTerms must agree with Rank: %v vs %v", viaRank, viaTerms)
	}
}

func TestSearchWindowsResolve(t *testing.T) {
	idx := Build(corpus())
	hits := idx.Search("连接池最大连接数", 2, 40)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	d, ok := idx.Doc(hits[0].DocID)
	if !ok {
		t.Fatalf("index lost doc %s", hits[0].DocID)
	}
	text, err := ResolveSpan(d.Body, hits[0].SpanCoord)
	if err != nil {
		t.Fatalf("span must resolve: %v", err)
	}
	if text == "" {
		t.Fatal("empty resolved span")
	}
}

func TestResolveSpanRejectsGarbage(t *testing.T) {
	if _, err := ResolveSpan("正文", "rune[0:999]"); err == nil {
		t.Fatal("out-of-range span must error, not silently resolve")
	}
	if _, err := ResolveSpan("正文", "nonsense"); err == nil {
		t.Fatal("malformed coord must error")
	}
}

func TestRankEmptyAndUnknownQuery(t *testing.T) {
	idx := Build(corpus())
	if idx.Rank("", 5) != nil {
		t.Fatal("empty query must return nil")
	}
	if idx.Rank("量子引力飞船", 5) != nil {
		t.Fatal("no-hit query must return nil, not everything")
	}
}

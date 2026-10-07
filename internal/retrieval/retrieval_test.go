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

// 混排文本（拉丁/数字直接接 CJK）必须在脚本边界分开：整串当一个词的
// 话 df 恒为 0，查询取不到任何候选——DuReader 真实问句 10% 死在这里。
func TestFieldsSplitsScriptBoundaries(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{"iphone6照片流在哪", []string{"iphone6", "照片", "片流", "流在", "在哪"}},
		{"8月去关山牧场穿什么", []string{"8", "月去", "去关", "关山", "山牧", "牧场", "场穿", "穿什", "什么"}},
		{"gtx960比gtx660强多少", []string{"gtx960", "gtx660", "强多", "多少"}}, // 单字 CJK 段丢弃
		{"GTX960", []string{"gtx960"}},
		{"连接池配置", []string{"连接", "接池", "池配", "配置"}},
	}
	for _, c := range cases {
		if got := Fields(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Fields(%q)\n want %v\n  got %v", c.text, c.want, got)
		}
	}
}

// 混排问句在真索引里必须能取到候选（回归：修复前整串一个词，Rank 恒空）。
func TestMixedScriptQueryFindsCandidates(t *testing.T) {
	idx := Build([]Document{
		{ID: "d1", Body: "iphone6 的照片流功能可以把照片同步到云端相册。"},
		{ID: "d2", Body: "连接池最大连接数默认为 100。"},
	})
	if ids := idx.Rank("iphone6照片流在哪", 3); len(ids) == 0 || ids[0] != "d1" {
		t.Fatalf("mixed-script query must reach the index, got %v", ids)
	}
}

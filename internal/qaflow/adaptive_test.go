package qaflow

import (
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/retrieval"
)

func adaptiveCorpus() *retrieval.Index {
	return retrieval.Build([]retrieval.Document{
		{ID: "d1", Body: "连接池最大连接数默认为 100，超过需调整配置。"},
		{ID: "d2", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
		{ID: "d3", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
		{ID: "d4", Body: "成本结构与分摊方法：成本按部门分摊，成本结构按季度复盘。"},
		{ID: "d5", Body: "成本结构与定价：成本结构决定底线，成本结构变动需重新定价。"},
		{ID: "d6", Body: "成本结构与预算：成本结构分解到项目，成本结构偏差超百分之五需说明。"},
	})
}

func covOf(query string, ws []EvidenceWindow) float64 {
	// 词面覆盖：查询词出现在任一窗口里
	hits := 0
	total := 0
	for _, term := range retrieval.Fields(query) {
		total++
		for _, w := range ws {
			if containsStr(w.Text, term) {
				hits++
				break
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

func containsStr(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOfStr(s, sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 小页覆盖够 → 不加宽（返回小页条数）。
func TestAdaptiveKeepsSmallPageWhenCovered(t *testing.T) {
	idx := adaptiveCorpus()
	f := AdaptiveK(idx, 60, 3, 9, 0.6, covOf)
	got, err := f(context.New("a"), Rewrite{Original: "连接池最大连接数"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 3 {
		t.Fatalf("covered query must stay on the small page, got %d", len(got))
	}
	if len(got) == 0 {
		t.Fatal("covered query must still return windows")
	}
}

// 小页覆盖不够 → 整页加宽（整页替换，不是拼两页）。
func TestAdaptiveWidensWholePage(t *testing.T) {
	idx := adaptiveCorpus()
	// 查询带语料外词（构财/财报 不在库里）→ 覆盖 3/5=0.6 → 超过阈值 0.7 才加宽
	// （阈值必须高于这个覆盖值，否则不触发）
	f := AdaptiveK(idx, 60, 3, 9, 0.7, covOf)
	got, err := f(context.New("b"), Rewrite{Original: "成本结构财报"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) <= 3 { // 命中文档只有 4 篇，但必须比小页宽
		t.Fatalf("must widen past the small page, got %d", len(got))
	}
	if len(got) != 4 { // d1/d2 不含"成本"相关词，只有 4 篇命中
		t.Fatalf("widen must return every matching doc, got %d", len(got))
	}
	first, _ := f(context.New("c"), Rewrite{Original: "成本结构财报"})
	if len(got) != len(first) {
		t.Fatalf("deterministic widening: %d vs %d", len(got), len(first))
	}
}

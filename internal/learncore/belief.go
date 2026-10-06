package learncore

import (
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/knowledge/belief"
)

// ObserveBelief 把一次已完成的评测运行折进候选区信念，返回观测条数。
//
// 观测定义（belief 包的语义，搬到这里不再重复）：对每个被引用窗口的
// 文档，o = 1 当该文档的金标 id 之一，否则 0；没被引用的文档不观测——
// 没试过就没有发言权。
//
// 为什么允许这里看金标而 executor 不能：学习是离线复盘，执行是现场答题。
// 闭卷只约束答题那一刻。
func ObserveBelief(b *belief.Belief, run evalfcore.RunState, items []evalfcore.Item) int {
	if b == nil {
		return 0
	}
	gold := make(map[string]map[string]bool, len(items))
	for _, it := range items {
		set := make(map[string]bool, len(it.GoldIDs))
		for _, g := range it.GoldIDs {
			set[g] = true
		}
		gold[it.ID] = set
	}
	observed := 0
	for _, res := range run.Results {
		g, ok := gold[res.ItemID]
		if !ok {
			continue
		}
		for _, docID := range res.CitedDocs {
			if g[docID] {
				b.Observe(docID, 1)
			} else {
				b.Observe(docID, 0)
			}
			observed++
		}
	}
	return observed
}

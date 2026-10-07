package qaflow

import (
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
)

// retrievePerFact 是多事实问句的逐事实取证据（破局件）。
//
// 出处：SUBQRAG（arXiv:2510.07718，2025.10）把复杂问题拆成有序子问题
// 链逐条检索、证据聚成可溯的 graph memory；cumulus 的 FactAware scorer
// 让一次评分更新全部事实的覆盖。我们的离线版：**每条事实用自己的词
// 各检一轮**（BM25 毫秒级，K≤4 轮），窗口合并去重后由 facts.Evaluate
// 逐条判覆盖——事实带全部支撑窗（证据路径可溯）。
//
// 为什么需要它：一次整句检索只会给一个窗口集，多事实问句的各条事实
// 在里面抢座位；排名靠前的事实把另一条事实的证据挤出 topK（真跑教训：
// "专利期限是多少年？专利权什么时候生效？"——公告/授予条款置顶，
// "期限为二十年"那条整个不在窗里，模型答"未涉及具体年数"）。
//
// K=1 不走这条（整句即事实，与旧路径逐字节一致——136 问里 124 问
// K=1，常态不能为新件付出行为变化）。
func retrievePerFact(c *context.Context, idx *retrieval.Index, fx []facts.Fact, k, width int) []retrieval.Hit {
	if len(fx) < 2 || idx == nil {
		return nil
	}
	var an query.Analysis
	if a, ok := context.Get(c, KeyAnalysis); ok {
		an = a
	}
	type keyed struct {
		hit retrieval.Hit
	}
	seen := map[string]bool{} // docID + span 去重
	var out []retrieval.Hit
	for _, f := range fx {
		// 事实自己的词，权取查询分析的权重（该词在分析里就拿分析权，
		// 否则平权）——事实检索复用同一套 IDF 加权口径
		weights := map[string]float64{}
		for _, term := range factTerms(f.Query) {
			if w, ok := an.Primary[term]; ok {
				weights[term] = w
			} else {
				weights[term] = 1.0
			}
		}
		// 窗口锚在事实自己的核心词组所在条文（SearchFactWeighted）——答
		// 案句收在窗里，不被密度/稀有度带到隔壁条文（真跑教训：专利期
		// 限两连问，窗口曾停在"授予专利权决定"那条）
		hits := idx.SearchFactWeighted(weights, k, width, f.Query)
		for _, h := range hits {
			key := h.DocID + "#" + h.SpanCoord
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, h)
		}
	}
	return out
}

// factTerms 事实的分词（与覆盖判定同口径——检索用的词和判覆盖用的词
// 必须同一种语言）。
func factTerms(q string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range facts.Fields(q) {
		if seen[t] || t == "" {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

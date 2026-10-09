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
// **K=1 的取舍**：原来 K=1 不走这条（整句即事实，与旧路径逐字节一致）。但长文档
// 探针证明它在长文里会漏——答案埋在 74%–98% 处而窗口切在 48%–57% 处，且加宽
// 能救（window hit 8/10 → 10/10）只是把整篇塞进窗口。现在 K=1 **仅在长文档**上
// 也走锚定切窗（`longDocAnchorable`），短文档与旧路径逐字节一致（零行为变化）。
func retrievePerFact(c *context.Context, idx *retrieval.Index, fx []facts.Fact, k, width int) []retrieval.Hit {
	if len(fx) == 0 || idx == nil {
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
		// **长文档：同一篇再取一个段位窗口**（BM25 每篇只有一个分数，所以"每篇一窗"
		// 在长文里必然漏掉远处的高密度段——真跑：答案在 98% 处、词密度与开头段并列
		// 最高，却完全没被覆盖）。段位窗口与锚定窗口**不重叠**，所以不是重复给。
		if len(fx) == 1 && longDocAnchorable(idx, f.ID) {
			hits = append(hits, idx.WindowMultiSpans(firstDocID(hits), termsOf(weights), width, multiSpanLimit)...)
		}
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

// longDocAnchorable 判断这条事实所在（或最可能命中）的文档是否**长到需要锚定切窗**。
//
// 口径：语料平均长度 > 窗口宽的 1.5 倍。理由很朴素——**窗口装不下整篇**时，
// 才需要"按事实的词组锚到具体条文"；短文档一个窗口就是全文，锚定没有意义。
// 用平均长度而不是单篇：avg 是廉价的全局判据（一次遍历），而真正的过滤交给
// `SearchFactWeighted` 的命中（没命中就等于没产出）。
func longDocAnchorable(idx *retrieval.Index, factID string) bool {
	if idx == nil || idx.N == 0 {
		return false
	}
	// 用 **AvgRunes（平均字符数）**，不是 AvgLen（token 数）：判"窗口装不装得下"
	// 是字符问题，用 token 数会把中文长文判成短文（真跑踩过）。
	return int(idx.AvgRunes) > defaultWidthRunes*3/2
}

// defaultWidthRunes 是默认窗口宽（与 Options.Width 的默认值同口径）。
const defaultWidthRunes = 400

// multiSpanLimit 是长文档下**同篇额外段位窗口**的数量上限。
//
// 为什么是 2 而不是更多：每多一个窗口就多一份要读的正文（合成面 token 也在涨），
// 而实测两处（开头程序规定 + 末尾结论）已经覆盖了探针里的全部失败形状。
const multiSpanLimit = 2

func firstDocID(hits []retrieval.Hit) string {
	if len(hits) == 0 {
		return ""
	}
	return hits[0].DocID
}

func termsOf(weights map[string]float64) []string {
	out := make([]string, 0, len(weights))
	for t := range weights {
		out = append(out, t)
	}
	return out
}

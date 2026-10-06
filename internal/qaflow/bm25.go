package qaflow

import (
	"errors"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/knowledge/belief"
	"github.com/willove/cumulus/internal/retrieval"
)

// maxRerankCompare 是语义重排只比前多少条。MiniLM 的甜蜜点是小集合高
// 区分（cumulus 用 50）；这里 50 同款。
const maxRerankCompare = 50

// BM25Evidence 把倒排索引接成 evidence stage 的取数函数。
//
// 三个决定落在这一行：
//   - 排序用原问（Rewrite.Original），不用假设视图——防漂移的落点，
//     假设视图只服务扩召回（cumulus 的 HyDE 用法，端口同语义）；
//   - 信念从 context 读（KeyBelief）：绑了就用后验加权，没绑就纯 BM25。
//     可选组件可见地缺席，不是静默换算法；
//   - 抽不出可回溯坐标的文档不进窗口——不可引用的证据没有存在价值，
//     宁可少一条，不许给一条核不掉的。
func BM25Evidence(idx *retrieval.Index, k, width int) func(*context.Context, Rewrite) ([]EvidenceWindow, error) {
	return func(c *context.Context, r Rewrite) ([]EvidenceWindow, error) {
		if r.Original == "" {
			return nil, errors.New("bm25 evidence: empty original question")
		}
		if idx == nil {
			return nil, errors.New("bm25 evidence: nil index")
		}
		var boost func(string) float64
		if b, ok := context.Get(c, KeyBelief); ok && b != nil {
			boost = BeliefBoost(b, 0.5, 1.5)
		}
		hits := idx.SearchWith(r.Original, k, width, boost)
		// 语义重排（可选组件）：绑了 embedder 才走；没绑/失败都保序并留痕
		hits, rerankState := rerankHits(c, hits, r.Original, maxRerankCompare)
		if err := context.Set(c, KeyRerank, rerankState); err != nil {
			return nil, err
		}
		windows := make([]EvidenceWindow, 0, len(hits))
		for _, h := range hits {
			if h.SpanCoord == "" {
				continue
			}
			windows = append(windows, EvidenceWindow{
				SourceID:  h.DocID,
				Span:      h.SpanCoord,
				Text:      h.SpanText,
				Score:     h.Score,
				Substrate: "text",
			})
		}
		return windows, nil
	}
}

// BindBelief 把候选区信念绑到 context（可选组件显式上线）。
func BindBelief(c *context.Context, b *belief.Belief) error {
	if b == nil {
		return errors.New("bind belief: nil belief")
	}
	return context.Set(c, KeyBelief, b)
}

package qaflow

import (
	"errors"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/query"
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
//   - 重排只在 embedder 绑了时发生；没绑就纯 BM25（可选组件可见地缺席）；
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
		hits := retrieveWeighted(c, idx, r.Effective(), k, width)
		// 语义重排（可选组件）：绑了 embedder 才走；没绑/失败都保序并留痕
		hits, rerankState := rerankHits(c, hits, r.Effective(), maxRerankCompare)
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
				Title:     h.Title,
				Span:      h.SpanCoord,
				Text:      h.SpanText,
				Score:     h.Score,
				Substrate: "text",
			})
		}
		texts := make([]string, 0, len(windows))
		for _, w := range windows {
			texts = append(texts, w.Text)
		}
		if err := context.Set(c, KeyCoverage, coverageOf(idx, r.Original, texts)); err != nil {
			return nil, err
		}
		return windows, nil
	}
}

// searchWeightedOrPlain：有分析（主关键词级）就按权检索，否则全词等权。
// retrieveWeighted：有查询分析（IDF 加权主关键词级）就按权检索，否则全词
// 等权。权是查询侧加权的显式化——处罚类泛词降权、稀有实体词着重。加权
// 级为空（问句几乎全是语料外词）时退回全词检索：那是鸿沟场景，交给级联
// 里的扩展兜底，不让检索先卡死。
func retrieveWeighted(c *context.Context, idx *retrieval.Index, q string, k, width int) []retrieval.Hit {
	if a, ok := context.Get(c, KeyAnalysis); ok && len(a.Primary) > 0 {
		hits := idx.SearchWeighted(a.Primary, k, width, nil)
		if on, _ := context.Get(c, KeyPriorOn); on {
			hits = priorRerank(c, idx, q, a, k, width, hits)
		}
		return hits
	}
	return idx.SearchWith(q, k, width, nil)
}

// priorRerank：BM25 候选池上做多信号重排（cumulus prior 移植：lexical
// 不做长度归一 + 标题 + 条文结构）。池取 BM25 前 k*3，prior 归一分作
// 为 boost 乘回 BM25——BM25 管词面命中，prior 管文档级置信（长文深命
// 中不被短文碰巧提到压住：真跑教训问"专利期限多少年"，置顶的是最高法
// 的专利代理短解释，答案在专利法第四十二条）。
func priorRerank(c *context.Context, idx *retrieval.Index, q string, a query.Analysis, k, width int, bm25Hits []retrieval.Hit) []retrieval.Hit {
	if len(bm25Hits) == 0 {
		return bm25Hits
	}
	words := make([]string, 0, len(a.Primary))
	for w := range a.Primary {
		words = append(words, w)
	}
	pool := idx.SearchWith(q, k*3, width, nil)
	if len(pool) == 0 {
		return bm25Hits
	}
	poolIDs := make([]string, 0, len(pool))
	for _, h := range pool {
		poolIDs = append(poolIDs, h.DocID)
	}
	boostMap := idx.BoostMap(words, poolIDs)
	// prior 信号落 context：响应里逐文档逐信号可查（取舍要看得见），
	// 也是"prior 到底跑没跑"的判据
	_ = context.Set(c, KeyPrior, idx.PriorRank(words, poolIDs, k))
	return idx.SearchWith(q, k, width, func(id string) float64 {
		if s, ok := boostMap[id]; ok {
			return s
		}
		return 1.0 // 池外文档（不该出现）保底 1，不误伤
	})
}

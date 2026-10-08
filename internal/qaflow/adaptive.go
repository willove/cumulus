package qaflow

import (
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/retrieval"
)

// AdaptiveK 是"按覆盖度决定要不要加宽"的取数包装：小页先取，覆盖不够就用
// 大页重取一次。
//
// 为什么不是"更聪明的打分"——两次真跑都证伪了那类思路（清单式 LLM 重排把
// evidence 从 85% 打到 52%；BM25 协调因子让 multidoc 27.1%→25.0%）。而诊断
// 数据指向另一头：DomainRAG multidoc 的 evidence@k3=27.1%、@k9=70.8%——
// 金标**在池子里但排在第 4–20 名**（44/48 在 top-20，只有 8/48 在 top-3）。
// 多实体问句（"A 与 B 在……上的共同目标"）天生需要更宽的一页。
//
// 口径：
//   - 覆盖用**可达词**（语料外词谁也覆盖不了，不进分母，与词面覆盖度同源）；
//   - 加宽是**整页替换**而不是"小页 + 大页剩余"——窗口预算有限，两页拼起来
//     等于偷偷加倍预算，对照就不公平；
//   - 覆盖写进遥测（KeyCoverage），"没加宽/加宽了、加宽前覆盖多少"可查。
func AdaptiveK(idx *retrieval.Index, width, smallK, bigK int, covThreshold float64, cover func(query string, ws []EvidenceWindow) float64) func(*context.Context, Rewrite) ([]EvidenceWindow, error) {
	if smallK <= 0 {
		smallK = 3
	}
	if bigK <= smallK {
		bigK = smallK * 3
	}
	return func(c *context.Context, r Rewrite) ([]EvidenceWindow, error) {
		small := retrieveK(idx, r.Effective(), smallK, width)
		cov := cover(r.Original, small)
		if cov >= covThreshold {
			setCoverage(c, idx, r.Original, small, cov)
			return small, nil
		}
		big := retrieveK(idx, r.Effective(), bigK, width)
		covBig := cover(r.Original, big)
		setCoverage(c, idx, r.Original, big, covBig)
		return big, nil
	}
}

// retrieveK 是一次 BM25 取数（窗口 + 坐标），检索失败按空处理由上层看见。
func retrieveK(idx *retrieval.Index, query string, k, width int) []EvidenceWindow {
	hits := idx.SearchWith(query, k, width, nil)
	out := make([]EvidenceWindow, 0, len(hits))
	for _, h := range hits {
		out = append(out, EvidenceWindow{
			SourceID: h.DocID,
			Title:    h.Title,
			Span:     h.SpanCoord,
			Text:     h.SpanText,
			Score:    h.Score,
		})
	}
	return out
}

func setCoverage(c *context.Context, idx *retrieval.Index, query string, ws []EvidenceWindow, cov float64) {
	info := coverageOf(idx, query, textsOf(ws))
	info.Value = cov
	_ = context.Set(c, KeyCoverage, info)
}

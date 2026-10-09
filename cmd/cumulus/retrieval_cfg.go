package main

import (
	"os"
	"strconv"

	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/qaflow"
)

// retrievalFor 是评测执行面的取数装配：默认按默认旋钮取数，CUMULUS_ADAPTIVE=1
// 时换成"按覆盖度决定要不要整页加宽"（实验档，默认关）。
//
// 诊断指向的是"给得够"而非"选得准"——multidoc evidence@k3=27.1%、@k9=70.8%。
// 但自适应夹在两者之间（覆盖度饱和分不出该不该加宽），所以默认仍是固定大页。
func (e *bm25Executor) retrievalFor() func(*context.Context, qaflow.Rewrite) ([]qaflow.EvidenceWindow, error) {
	if os.Getenv("CUMULUS_ADAPTIVE") != "1" {
		return qaflow.BM25Evidence(e.idx, e.topk(), e.width())
	}
	big := e.topk() * 3
	if os.Getenv("CUMULUS_TOPK") == "" {
		big = 9
	}
	thr := 0.6
	if v := os.Getenv("CUMULUS_ADAPTIVE_COV"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			thr = f
		}
	}
	idx := e.idx
	return qaflow.AdaptiveK(idx, e.width(), e.topk(), big, thr,
		func(q string, ws []qaflow.EvidenceWindow) float64 {
			return qaflow.LexicalCoverageFor(idx, q, ws)
		})
}

// weightedRetrieveFor 装加权重取（桥的落地处）。
//
// **两处调用（serve 与 eval）必须同一份实现**：桥的效果是量出来的，量的时候
// 两个入口的行为不一致，"护栏有没有用"这个问题就答不了。
func weightedRetrieveFor(idx *retrieval.Index, topK, width int) func(map[string]float64) ([]qaflow.EvidenceWindow, error) {
	return func(weights map[string]float64) ([]qaflow.EvidenceWindow, error) {
		hits := idx.SearchWeighted(weights, topK, width, nil)
		out := make([]qaflow.EvidenceWindow, 0, len(hits))
		for _, h := range hits {
			out = append(out, qaflow.EvidenceWindow{
				SourceID: h.DocID, Title: h.Title, Span: h.SpanCoord, Text: h.SpanText, Score: h.Score,
			})
		}
		return out, nil
	}
}

// analyzerFor 装查询分析（桥的触发判据要看它）。
func analyzerFor(idx *retrieval.Index) func(string) query.Analysis {
	return func(q string) query.Analysis { return query.Analyze(q, idx, idx.N) }
}

// bindBridge 把桥装进执行面（消融两处）：
//
//	CUMULUS_BRIDGE=0     关桥（退回朴素贵路）
//	CUMULUS_BRIDGE_GUARD=0 关护栏（采信桥，不管它把检索带偏）
//
// 三臂对照才能说清"护栏有没有用"：只跑"开桥"是自证，说服力为零。
func bindBridge(ex *bm25Executor, idx *retrieval.Index, llmForBridge *llm.OpenAICompleter, knobs map[string]float64) {
	ex.analyzer = analyzerFor(idx)
	if llmForBridge != nil {
		ex.expander = &query.Cached{Inner: &query.LLM{Client: llmForBridge}}
		ex.weightedRetrieve = weightedRetrieveFor(idx, 9, widthFromKnobs(knobs))
	}
}

// routeWithEnvToggles 把"零窗口是否先升级"接成消融开关。
//
// CUMULUS_ZERO_WINDOW_ESCALATE=1：首程零窗口时先升级（贵路 + 词汇桥）再判不知道。
// 默认关：保持今天的"零窗口直接拒答"——**改了要先量**（零窗口→升级会多花一次贵路，
// 也可能把"语料里真没有"拖成一次升级后才拒答，两种都说得通）。
func routeWithEnvToggles(rc qaflow.RouteConfig) qaflow.RouteConfig {
	rc.ZeroWindowEscalate = os.Getenv("CUMULUS_ZERO_WINDOW_ESCALATE") == "1"
	return rc
}

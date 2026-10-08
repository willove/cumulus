package main

import (
	"os"
	"strconv"

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

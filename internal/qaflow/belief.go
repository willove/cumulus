package qaflow

import (
	"github.com/willove/cumulus/internal/knowledge/belief"
)

// BeliefBoost 把候选区后验转成检索的加权系数。
//
// 语义来自 belief.Order 的同一组事实：观测过且有产出的文档上浮，
// 观测过但没产出的下沉，没观测过的不动（1.0）。系数有界
// [floor, ceil]，单个坏观测翻不了盘。
func BeliefBoost(b *belief.Belief, floor, ceil float64) func(docID string) float64 {
	if b == nil {
		return nil
	}
	return func(docID string) float64 {
		p, observed := b.Get(docID)
		if !observed {
			return 1
		}
		f := floor + p*(ceil-floor)
		if f < floor {
			return floor
		}
		if f > ceil {
			return ceil
		}
		return f
	}
}

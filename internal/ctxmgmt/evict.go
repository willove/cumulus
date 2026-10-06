// Package ctxmgmt 是合成前的上下文管理：窗口预算、近重合并、按源配额。
//
// 研究出处（04 第 4 条，Volt 的驱逐策略）：语义去重同类（cosine ≥ 0.92
// 合并）、按类配额、窗口要小——无关上下文提高选错工具和编造参数的概率，
// 且税负正比于相关与无关条目的语义重叠度（ContextRot 的“噪声税”）。
// 窗口小不只是省钱，是降压。
//
// 这也是 MiniLM 在本项目里**研究处方的位置**：不是重排（真语料 0 翻转
// 已证伪），是合并。三条纪律：
//   - 每个驱逐决定都留痕（合并了谁、丢了谁、为什么）——驱逐不可见于
//     调试和 cumulus 当年“MCS 静默不触发”是同一类事故；
//   - embedder 缺席时语义合并不做，词法/预算/配额照做（降级可见）；
//   - 只蒸干，不加水：预算不够时按打分丢，不打分重排。
package ctxmgmt

import (
	"math"
	"sort"
)

// Budget 是驱逐策略的旋钮。
type Budget struct {
	MaxWindows   int     // 窗口预算（0 = 不限额）
	PerSourceMax int     // 每个来源的窗口上限（0 = 不限）
	DedupCosine  float64 // 合并阈值：cosine ≥ 此值的近重窗口合并（0 = 不合并）
}

// WithDefaults 是导出的默认值补齐（零值 = 默认预算）。
func (b Budget) WithDefaults() Budget { return b.withDefaults() }

func (b Budget) withDefaults() Budget {
	if b.MaxWindows < 0 {
		b.MaxWindows = 0
	}
	if b.PerSourceMax < 0 {
		b.PerSourceMax = 0
	}
	if b.DedupCosine < 0 {
		b.DedupCosine = 0
	}
	return b
}

// Window 是驱逐的最小单元。
type Window struct {
	SourceID string
	Span     string
	Text     string
	Score    float64
}

// MergeRecord 是一次合并的记录：并进了谁、为什么（余弦值）。
type MergeRecord struct {
	Kept   Window
	Merged Window
	Cosine float64
}

// DropRecord 是一次丢弃的记录：丢了谁、为什么。
type DropRecord struct {
	Window Window
	Reason string // per-source / budget
}

// Log 是全部驱逐决定的账。合成前打印/入库——可审计是硬要求。
type Log struct {
	Merged  []MergeRecord
	Dropped []DropRecord
}

// Apply 执行驱逐，返回保留的窗口与完整日志。
//
// vectors 是窗口的向量（按下标对应，nil 项 = 没有向量）。vectors 为
// nil 或与窗口数不符时语义合并不做（降级而不是失败）。
func Apply(windows []Window, vectors [][]float32, b Budget) ([]Window, Log) {
	b = b.withDefaults()
	var log Log
	if len(windows) == 0 {
		return windows, log
	}

	// 1) 按源配额（先配额后合并：配额是硬规则，合并是优化）
	kept := windows
	if b.PerSourceMax > 0 {
		perSrc := map[string]int{}
		var out []Window
		for _, w := range kept {
			if perSrc[w.SourceID] >= b.PerSourceMax {
				log.Dropped = append(log.Dropped, DropRecord{Window: w, Reason: "per-source"})
				continue
			}
			perSrc[w.SourceID]++
			out = append(out, w)
		}
		kept = out
	}

	// 2) 语义近重合并（阈值 cosine ≥ DedupCosine；同分保留先出现的）
	if b.DedupCosine > 0 && len(vectors) == len(windows) {
		var out []Window
		var outVecs [][]float32
		for i, w := range kept {
			dup := false
			for j := range out {
				if cos := cosine(vectors[i], outVecs[j]); cos >= b.DedupCosine {
					log.Merged = append(log.Merged, MergeRecord{Kept: out[j], Merged: w, Cosine: cos})
					dup = true
					break
				}
			}
			if !dup {
				out = append(out, w)
				outVecs = append(outVecs, vectors[i])
			}
		}
		kept = out
	}

	// 3) 窗口预算：按打分保留前 MaxWindows（同分保原序）
	if b.MaxWindows > 0 && len(kept) > b.MaxWindows {
		sorted := append([]Window(nil), kept...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
		keep := map[string]bool{}
		for _, w := range sorted[:b.MaxWindows] {
			keep[w.SourceID+"#"+w.Span] = true
		}
		var out []Window
		for _, w := range kept {
			if keep[w.SourceID+"#"+w.Span] {
				out = append(out, w)
			} else {
				log.Dropped = append(log.Dropped, DropRecord{Window: w, Reason: "budget"})
			}
		}
		kept = out
	}
	return kept, log
}

func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

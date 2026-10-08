package calib

import (
	"math"
	"sort"
)

// AUC 是信号的**区分度**：随机取一对（一对一错），信号给对的那对分数更高的
// 比例。0.5 = 与抛硬币同；>0.5 越高越好；<0.5 说明信号方向反了（用它时要反过来）。
//
// 为什么用 AUC 而不是"某个阈值下的准确率"：阈值本身是被挑出来的（挑过的准确率
// 天然偏乐观），AUC 衡量的是**排序能力**——"高分的样本更可能是对的"这件事成立
// 多少，与阈值无关。研究问题"哪个信号更能预测正确性"问的正是排序，不是某个点。
// 平分按 0.5 记（并列不算赢，也不算输）。
//
// n<2 或只有一类标签时返回 0.5（无法判别，不假装有区分度）。
func AUC(samples []Sample) float64 {
	type pt struct {
		score float64
		pos   bool
	}
	pts := make([]pt, 0, len(samples))
	pos, neg := 0, 0
	for _, s := range samples {
		pts = append(pts, pt{score: s.Confidence, pos: s.Correct})
		if s.Correct {
			pos++
		} else {
			neg++
		}
	}
	if pos == 0 || neg == 0 {
		return 0.5
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].score < pts[j].score })
	// Mann–Whitney U（含并列按 0.5）
	rankSum := 0.0
	i := 0
	for i < len(pts) {
		j := i
		for j+1 < len(pts) && pts[j+1].score == pts[i].score {
			j++
		}
		avgRank := (float64(i+1) + float64(j+1)) / 2 // 名次从 1 开始
		for k := i; k <= j; k++ {
			if pts[k].pos {
				rankSum += avgRank
			}
		}
		i = j + 1
	}
	u := rankSum - float64(pos)*(float64(pos)+1)/2
	return u / (float64(pos) * float64(neg))
}

// LiftAt 报"信号最高的 q 比例里，正确率比整体高多少"（>1 = 有区分度）。
// 它比 AUC 更贴地回答"高置信那批是不是真的更可信"。
func LiftAt(samples []Sample, q float64) float64 {
	if q <= 0 || q > 1 || len(samples) == 0 {
		return 1
	}
	sorted := append([]Sample(nil), samples...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Confidence > sorted[j].Confidence })
	take := int(math.Round(float64(len(sorted)) * q))
	if take <= 0 {
		take = 1
	}
	if take > len(sorted) {
		take = len(sorted)
	}
	ok := 0
	for _, s := range sorted[:take] {
		if s.Correct {
			ok++
		}
	}
	top := float64(ok) / float64(take)
	all := 0
	for _, s := range sorted {
		if s.Correct {
			all++
		}
	}
	base := float64(all) / float64(len(sorted))
	if base == 0 {
		return 1
	}
	return top / base
}

package retrieval

import (
	"math"
	"sort"
)

// RerankByCosine 用段落级语义相似度重排 BM25 候选。
//
// 位置在 cumulus 的同款点：BM25 收窄之后、窗口定序之前。两条设计约束
// 来自 cumulus 的实测（internal/index/rerank.go 的注释）：
//   - 只比小集合：段落级比较的区分度在小集合里高，全库扫描是 MiniLM
//     的弱项（法条相似文本间分不开）；
//   - 同分保持原序：向量打平时的抖动会把词法序随机化，那是 cumulus
//     被假向量坑过的方式（BM25 序必须可恢复）。
//
// 纯函数：向量由调用方算好（rerank 不做 IO）。passageVecs 与 hits 等长
// 对应；任一条算不出向量时传 nil 项——nil 项保持原序位置，不参与重排。
func RerankByCosine(hits []Hit, queryVec []float32, passageVecs [][]float32) []Hit {
	if len(hits) <= 1 || queryVec == nil {
		return hits
	}
	type scored struct {
		hit   Hit
		cos   float64
		order int
	}
	items := make([]scored, len(hits))
	for i, h := range hits {
		s := scored{hit: h, cos: -1, order: i} // 没有向量的排最后且保序
		if i < len(passageVecs) && passageVecs[i] != nil {
			s.cos = cosine32(queryVec, passageVecs[i])
		}
		items[i] = s
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].cos > items[j].cos
	})
	out := make([]Hit, len(items))
	for i, s := range items {
		out[i] = s.hit
	}
	return out
}

// cosine32 与 embed.Cosine 同算法；retrieval 不引 embed 是为了让纯函数
// 侧不依赖组件侧（embed 将来只有 MiniLM 一个实现，也不值得开这条边）。
func cosine32(a, b []float32) float64 {
	if len(a) != len(b) {
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

// Package embed 是向量面：句子/段落 → 384 维稠密向量。
//
// 端口唯一实现是本地 MiniLM（paraphrase-multilingual-MiniLM-L12-v2，
// vendor 于 internal/minilm）。**没有线上实现**：把语料送出本机边界，
// 与本地知识库的前提冲突。模型卡给的两个用途（聚类、语义搜索）在本项目
// 的落点分别是簇语义（后期）与 BM25 收窄后的段落级重排（本包服务的对象）。
package embed

import (
	"context"
	"errors"
	"math"
)

// ErrDimMismatch 向量维度不一致。
var ErrDimMismatch = errors.New("embed: dimension mismatch")

// Embedder 把文本映射到 L2 归一化的定长向量。
type Embedder interface {
	Dims() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Cosine 两个（已归一化）向量的 cosine。未归一化也给出正确值——
// 只是多一次除法。任一为零向量返回 0。
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0 // 维度不符按“不相似”处理，调用方用 Norm 自查
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

// L2Norm 归一化一份向量（就地）。零向量不动。
func L2Norm(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
	return v
}

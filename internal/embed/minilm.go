package embed

import (
	"context"
	"fmt"

	"github.com/willove/cumulus/internal/minilm"
)

// MiniLM 是端口的唯一实现：包一层 vendored 的
// sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2。
//
// 包适配器而不是让 minilm 直接实现端口：minilm 是 vendored 件
// （改动要留日志），端口是契约——中间这层隔离让 vendored 升级和契约
// 演进互不牵连。
type MiniLM struct {
	Inner *minilm.Embedder
}

// NewMiniLM 用模型目录构造。目录里没有权重时不报错——缺权重是
// Available() 为假，由调用方决定是否挂载（组件分类器会因此不激活）。
func NewMiniLM(dir string) *MiniLM {
	return &MiniLM{Inner: minilm.New(dir)}
}

// Available 权重是否就位（manifest + 文件齐）。
func (m *MiniLM) Available() bool { return minilm.Available() }

// Dims 384（MiniLM-L12-v2）。
func (m *MiniLM) Dims() int { return m.Inner.Dims() }

// Embed 走本地推理，float64→float32 收窄（归一化由 minilm 保证）。
func (m *MiniLM) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := m.Inner.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embed minilm: %w", err)
	}
	out := make([][]float32, len(vecs))
	for i, v := range vecs {
		f := make([]float32, len(v))
		for j, x := range v {
			f[j] = float32(x)
		}
		out[i] = f
	}
	return out, nil
}

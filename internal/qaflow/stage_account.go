package qaflow

import (
	"errors"
	"github.com/willove/cumulus/internal/context"
)

type AccountStage struct{}

func (AccountStage) Name() string     { return "account" }
func (AccountStage) Reads() []string  { return []string{KeySynthUsage.String()} }
func (AccountStage) Writes() []string { return []string{KeyUsage.String()} }
func (AccountStage) Run(c *context.Context) error {
	u, ok := context.Get(c, KeySynthUsage)
	if !ok {
		// 没有合成发生额（例如拒答）：零发生额，但仍要记一笔
		return context.Set(c, KeyUsage, Usage{})
	}
	return context.Set(c, KeyUsage, u)
}
func (AccountStage) Verify(c *context.Context) error {
	u, ok := context.Get(c, KeyUsage)
	if !ok {
		return errors.New("usage missing")
	}
	if !u.CostKnown {
		// 不是错误：显式记录“成本未知”
		return nil
	}
	if u.PromptTokens < 0 || u.CompletionTokens < 0 {
		return errors.New("negative token counts")
	}
	return nil
}

// ---------- stage 6: 学习（可选） ----------

// LearnStage 默认不进流程。只有开启自进化的部署才追加它，
// 且必须由护栏保护（learncore 的职责，v0.1 未接线）。
type LearnStage struct{ Enabled bool }

func (LearnStage) Name() string     { return "learn" }
func (LearnStage) Reads() []string  { return []string{KeyUsage.String()} }
func (LearnStage) Writes() []string { return []string{"knowledge.belief"} }
func (s LearnStage) Run(c *context.Context) error {
	if !s.Enabled {
		return nil // 不写任何 key：不启用就不留痕迹
	}
	// TODO: 受管变更五阶段（learncore）
	return nil
}
func (LearnStage) Verify(c *context.Context) error { return nil }

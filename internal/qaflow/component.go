package qaflow

import (
	"github.com/willove/cumulus/internal/context"
)

// BeliefBooster 是第一个用上分类器的可选组件：它只要求一件事——
// knowledge.belief 被绑上。绑了即激活（检索开始用后验加权），撤了即
// 停用（回到纯 BM25）。它不需要知道上下文怎么变，也不许在依赖没了
// 以后继续跑。
type BeliefBooster struct {
	active bool
}

func (BeliefBooster) Name() string       { return "belief-booster" }
func (BeliefBooster) Requires() []string { return []string{KeyBelief.String()} }

func (b *BeliefBooster) Activate(c *context.Context) error {
	b.active = true
	return nil
}

func (b *BeliefBooster) Deactivate(c *context.Context) error {
	b.active = false
	return nil
}

// Active 供测试与 status 面查询。
func (b *BeliefBooster) Active() bool { return b.active }

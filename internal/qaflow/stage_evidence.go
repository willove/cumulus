package qaflow

import (
	"errors"
	"fmt"
	"github.com/willove/cumulus/internal/context"
)

type EvidenceStage struct {
	Retrieve func(c *context.Context, rewrite Rewrite) ([]EvidenceWindow, error)
}

func (EvidenceStage) Name() string    { return "evidence-supply" }
func (EvidenceStage) Reads() []string { return []string{KeyRewrite.String(), KeyDeepen.String()} }
func (EvidenceStage) Writes() []string {
	return []string{KeyWindows.String(), KeyRerank.String(), KeyDeep.String(), KeyCoverage.String(), KeyPrior.String()}
}
func (s EvidenceStage) Run(c *context.Context) error {
	r, ok := context.Get(c, KeyRewrite)
	if !ok {
		return errors.New("rewrite missing; stage 1 must run first")
	}
	if s.Retrieve == nil {
		return errors.New("no retrieval backend wired")
	}
	ws, err := s.Retrieve(c, r)
	if err != nil {
		return err
	}
	return context.Set(c, KeyWindows, ws)
}

// Verify 强制每条窗口可回溯：SourceID 与 Span 都必须有。
// 这是“引用可核”在流程里的第一道闸，缺一个都不行。
func (EvidenceStage) Verify(c *context.Context) error {
	ws, ok := context.Get(c, KeyWindows)
	if !ok {
		return errors.New("evidence windows missing")
	}
	for i, w := range ws {
		if w.SourceID == "" || w.Span == "" {
			return fmt.Errorf("window %d: source id and span are both required (citation must resolve)", i)
		}
	}
	return nil
}

// ---------- stage 3: 充足性路由 ----------

// RouteStage 用可观测信号决定 fast / escalate / refuse（04 落点 1）：
// 置信代理由三个便宜信号算出——查询词覆盖度、候选区分度、死路率。
// 置信度只当路由代理，不当正确性裁决（BioHarness）；信号与决策全部
// 留痕，事后可回答"这次为什么升级/拒答"。阈值与校准程序走 RouteConfig
// （v0.2 §2.1：阈值不手调）——档位与程序进提交视图。

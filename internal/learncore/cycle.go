package learncore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/willove/cumulus/internal/evalfcore"
)

// CycleRecord 一次周期的落盘记录（clus_learning 的对应物）。
type CycleRecord struct {
	ID          string             `json:"id"`
	At          time.Time          `json:"at"`
	Observation Observation        `json:"observation"`
	Proposal    *Proposal          `json:"proposal,omitempty"`
	Candidate   *evalfcore.Summary `json:"candidate,omitempty"` // 候选评估摘要
	Verdict     string             `json:"verdict"`             // promoted / rejected / rolled-back / nothing-to-learn
	Reasons     []string           `json:"reasons,omitempty"`
	KnobsBefore map[string]float64 `json:"knobs_before,omitempty"`
	KnobsAfter  map[string]float64 `json:"knobs_after,omitempty"`
}

// Cycle 跑一次受管变更。零值不可用，字段都有名有姓。
type Cycle struct {
	Reg          *Registry
	Guard        Guardrail
	Hypo         Hypothesizer
	Base         Baseline                                          // 护栏基线（来自已冻结的基线运行）
	NewExec      func(knobs map[string]float64) evalfcore.Executor // 用候选旋钮造执行面
	Fingerprints evalfcore.Fingerprints
	RunStore     evalfcore.RunStore // 候选评估的 episode 档案
	Budget       int                // token 硬顶：候选评估花费超过即拒（cumulus 40 万 token 的对应物；0 = 不限）
}

// RunWithObservation 是 Run 的完整体：观察数据由调用方从基线运行喂进来。
// 基线运行没有时拒跑（无护栏基线拒跑）。
func (c *Cycle) RunWithObservation(ctx context.Context, obs Observation, items []evalfcore.Item, cycleID string) (CycleRecord, error) {
	rec := CycleRecord{ID: cycleID, At: time.Now(), KnobsBefore: c.Reg.Snapshot()}

	if c.Base.IsZero() {
		rec.Verdict = "rejected"
		rec.Reasons = []string{"no guardrail baseline: refuse to run (grammar §四)"}
		return rec, nil
	}
	rec.Observation = obs

	// —— 3. 提议 ——
	prop, err := c.Hypo.Propose(obs)
	if err != nil {
		rec.Verdict = "nothing-to-learn"
		rec.Reasons = []string{err.Error()}
		return rec, nil
	}
	// 白名单把守：模型（或规则）提议的每个键都必须在册、改后值在界内。
	reasons := c.applyToRegistry(prop.Knobs)
	if len(reasons) > 0 {
		rec.Verdict = "rejected"
		rec.Reasons = reasons
		return rec, nil
	}
	rec.Proposal = &prop

	// —— 4. 评估：冻结指纹上跑候选 ——
	candExec := c.NewExec(c.Reg.Snapshot())
	runner := evalfcore.NewRunner(c.RunStore, c.Fingerprints, candExec, nil)
	state, err := runner.Start(ctx, "cand-"+cycleID, items)
	if err != nil {
		// 候选跑飞：旋钮退回原值，周期记 rejected
		_ = c.Reg.Restore(rec.KnobsBefore)
		rec.Verdict = "rejected"
		rec.Reasons = []string{fmt.Sprintf("candidate run failed: %v", err)}
		return rec, nil
	}
	if c.Budget > 0 && stateSummaryTokens(state) > c.Budget {
		_ = c.Reg.Restore(rec.KnobsBefore)
		rec.Verdict = "rejected"
		rec.Reasons = []string{fmt.Sprintf("candidate tokens %d over budget %d", stateSummaryTokens(state), c.Budget)}
		return rec, nil
	}
	cand := evalfcore.Summarize(state)
	rec.Candidate = &cand

	ok, guardReasons := c.Guard.Check(Baseline{
		EvidenceHitRate: cand.EvidenceHitRate,
		CitationsOKRate: cand.CitationsOKRate,
	})
	if !ok {
		// 4a. 不过护栏：退回原值，不上线
		_ = c.Reg.Restore(rec.KnobsBefore)
		rec.Verdict = "rejected"
		rec.Reasons = guardReasons
		return rec, nil
	}

	// —— 5. 提升：登记新值（逆 = KnobsBefore 已存），随后复核 ——
	rec.KnobsAfter = c.Reg.Snapshot()
	rec.Verdict = "promoted"

	// 复核：用新值再跑一次，掉线即回滚
	verifyExec := c.NewExec(c.Reg.Snapshot())
	verifyRunner := evalfcore.NewRunner(c.RunStore, c.Fingerprints, verifyExec, nil)
	vstate, verr := verifyRunner.Start(ctx, "verify-"+cycleID, items)
	if verr != nil {
		_ = c.Reg.Restore(rec.KnobsBefore)
		rec.Verdict = "rolled-back"
		rec.Reasons = []string{fmt.Sprintf("verify run failed: %v", verr)}
		rec.KnobsAfter = c.Reg.Snapshot()
		return rec, nil
	}
	vsum := evalfcore.Summarize(vstate)
	vok, vreasons := c.Guard.Check(Baseline{EvidenceHitRate: vsum.EvidenceHitRate, CitationsOKRate: vsum.CitationsOKRate})
	if !vok {
		_ = c.Reg.Restore(rec.KnobsBefore)
		rec.Verdict = "rolled-back"
		rec.Reasons = append(vreasons, "verify regression after promotion: rolled back")
		rec.KnobsAfter = c.Reg.Snapshot()
		return rec, nil
	}
	return rec, nil
}

// applyToRegistry 把提议写进旋钮，返回拒绝理由（空 = 全上）。
// 越界提议不夹取（静默夹取=静默改意图，cumulus 的 fail-open 教训），
// 整组拒绝：要么全上，要么全不上。
func (c *Cycle) applyToRegistry(knobs map[string]float64) []string {
	var reasons []string
	// 先全量校验，再应用（避免应用一半失败）
	for name, v := range knobs {
		k, ok := c.Reg.Get(name)
		if !ok {
			reasons = append(reasons, fmt.Sprintf("knob %q not in whitelist", name))
			continue
		}
		if v < k.Min || v > k.Max {
			reasons = append(reasons, fmt.Sprintf("knob %q: %v outside [%v,%v]", name, v, k.Min, k.Max))
		}
	}
	if len(reasons) > 0 {
		return reasons
	}
	for name, v := range knobs {
		k, _ := c.Reg.Get(name)
		_ = k.Set(v)
	}
	return nil
}

func stateSummaryTokens(s evalfcore.RunState) int {
	sum := evalfcore.Summarize(s)
	return sum.TotalPromptTokens + sum.TotalCompletionTokens
}

// CycleStore 学习档案（与 RunStore 同一 KV 域）。
type CycleStore interface {
	SaveCycle(ctx context.Context, rec CycleRecord) error
	LoadCycle(ctx context.Context, id string) (CycleRecord, error)
}

// ErrCycleNotFound 档案不存在。
var ErrCycleNotFound = errors.New("learncore: cycle not found")

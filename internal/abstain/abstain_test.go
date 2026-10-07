package abstain

import "testing"

// 空手检索（无窗、跳过）→ p_fail 高 → refuse（早弃权：DEEP 在
// 这类题上是纯燃烧，cumulus 实测 ≈96s/19k tokens 换必然的拒答）
func TestEarlyAbstainOnEmptyRetrieval(t *testing.T) {
	h := Default()
	p, act := h.Decide(Features{QueryLen: 8, Candidates: 0, Kept: 0, Skipped: true})
	if act != "refuse" {
		t.Fatalf("无窗且跳过必须早弃权，got act=%s p=%.2f", act, p)
	}
}

// 有窗有置信 → 不动作（有证据就别瞎预测）
func TestNoActionWithEvidence(t *testing.T) {
	h := Default()
	p, act := h.Decide(Features{QueryLen: 8, Candidates: 12, Kept: 3, TopScore: 6, Confidence: 0.8})
	if act != "" {
		t.Fatalf("有证据必须无意见，got act=%s p=%.2f", act, p)
	}
}

// 未盖事实堆高 p_fail（多事实问句的事实缺口是危险信号）
func TestMissingFactsRaisePFail(t *testing.T) {
	h := Default()
	base, _ := h.Decide(Features{QueryLen: 8, Kept: 3, TopScore: 6, Confidence: 0.6})
	gap, _ := h.Decide(Features{QueryLen: 8, Kept: 3, TopScore: 6, Confidence: 0.6, MissingFacts: 2})
	if gap <= base {
		t.Fatalf("未盖事实必须推高 p_fail：%.2f → %.2f", base, gap)
	}
}

// 一个低分样本也升级不拒（DEEP 救回过题，不能一刀切）
func TestOneLowSampleStillEscalatesNotRefuses(t *testing.T) {
	h := Default()
	p, act := h.Decide(Features{QueryLen: 20, Candidates: 5, Kept: 1, TopScore: 2, Confidence: 0.2, MissingFacts: 1})
	if act == "refuse" {
		t.Fatalf("有低分样本时不该早弃权（可升级），p=%.2f act=%s", p, act)
	}
}

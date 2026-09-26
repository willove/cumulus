package deep

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/source"
)

// Per-round pessimistic exit (u_d, Jev-Mem §3.3 的单出口)：EarlyAbove 打开时，
// 连续若干轮 p_fail 高位且不改善的语料必须在两三个文件后收手，而不是烧完
// MaxLoops + CorrectBudget + WidenBudget 去换一个必然的拒答（真机：≈96s /
// ≈19k tokens）。机制级测试直接打 runDeep：FAST 边界的一次性早弃权管住
// 全语料口径的常规情况，这个出口兜住边界视角过时后的路径（窄复用升级、
// admission 重排等）。
func TestUtilityStopHaltsHopelessAdmission(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	var sampled, widenCalls int
	e.OnFile = func(string, float64, int) { sampled++ }
	e.Widen = func(context.Context, string, map[string]bool, int, map[string]bool) ([]source.Source, error) {
		widenCalls++
		return nil, nil
	}
	e.Abstain = abstain.Default() // EarlyAbove = 0.80, operator opt-in shape

	srcs := make([]source.Source, 0, 5)
	for i := 0; i < 5; i++ {
		srcs = append(srcs, source.New(
			"无关"+string(rune('A'+i)), "md", "", strings.ToLower(string(rune('a'+i))), "zh",
			strings.Repeat("无关内容。", 60), nil))
	}

	ans, _, _, wid, sc, _, _, reason, err := e.runDeep(ctx, "连接池最大连接数是多少", srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "utility" {
		t.Fatalf("stop reason = %q, want utility", reason)
	}
	if sampled != 3 { // 2 rounds to judge + 1 confirming non-improvement
		t.Fatalf("files sampled = %d, want 3 (not the whole corpus)", sampled)
	}
	if widenCalls != 0 || wid != 0 {
		t.Fatalf("a utility stop must skip widening: calls=%d widened=%d", widenCalls, wid)
	}
	if sc {
		t.Fatal("a utility stop must skip self-correction")
	}
	if !ans.Skipped {
		t.Fatalf("the kept set is empty — answer must be skipped: %+v", ans)
	}
}

// 对照组：EarlyAbove 关闭（默认部署）时行为完全不变——整个候选集照走，
// 自纠错照跑，stop reason 为空。
func TestUtilityStopDisabledWalksAllCandidates(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	var sampled int
	e.OnFile = func(string, float64, int) { sampled++ }
	h := abstain.Default()
	h.EarlyAbove = 0 // opt-out: the default wiring (CLUS_EARLY_ABSTAIN unset)
	e.Abstain = h

	srcs := make([]source.Source, 0, 5)
	for i := 0; i < 5; i++ {
		srcs = append(srcs, source.New(
			"无关"+string(rune('A'+i)), "md", "", strings.ToLower(string(rune('a'+i))), "zh",
			strings.Repeat("无关内容。", 60), nil))
	}

	// Budget-wide-enough that the loop is bounded only by the candidate
	// list: the assertion is about the utility-stop path, not about the
	// loop budgets (whose defaults this test must not be coupled to).
	e.MaxLoops = len(srcs)
	e.CorrectBudget = len(srcs)
	e.WidenBudget = len(srcs)

	_, _, _, _, sc, _, _, reason, err := e.runDeep(ctx, "连接池最大连接数是多少", srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("stop reason = %q, want empty (candidates exhausted naturally)", reason)
	}
	if sampled != 5 {
		t.Fatalf("files sampled = %d, want all 5", sampled)
	}
	if !sc {
		t.Fatal("self-correction must still run when the exit is disabled")
	}
}

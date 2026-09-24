package deep

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// 3.1 早弃权：FAST 完全没有可用证据（0 样本 + skipped）且 p_fail 达线时，
// DEEP 一次都不跑。用 Widen 钩子计数证明 runDeep 未被进入。
func TestEarlyAbstainSkipsDeep(t *testing.T) {
	ctx := context.Background()
	kbE := kb.New(fast.New(mcs.KeywordScorer{Keywords: []string{"不存在词"}}),
		cluster.NewMemory(), cluster.Local{N: 64})
	e := New(kbE, NewMemoryConflict())
	e.Abstain = abstain.Default() // EarlyAbove enabled by the caller
	deepCalls := 0
	e.Widen = func(context.Context, string, map[string]bool, int, map[string]bool) ([]source.Source, error) {
		deepCalls++
		return nil, nil
	}
	src := source.New("手册", "md", "", "m", "zh",
		strings.Repeat("无关内容。", 200), nil)

	res, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if !res.AbstainEarly {
		t.Fatalf("early refuse expected: p=%.3f act=%q", res.AbstainP, res.AbstainAction)
	}
	if !res.Answer.Refused || !res.Answer.Skipped {
		t.Fatalf("early refuse must mark the answer refused: %+v", res.Answer)
	}
	if res.Escalated || res.Loops != 0 || deepCalls != 0 {
		t.Fatalf("DEEP must not run: escalated=%v loops=%d widenCalls=%d", res.Escalated, res.Loops, deepCalls)
	}
	if res.Answer.Summary == "" {
		t.Fatal("refusal must carry a summary")
	}
}

// 对照组：同一个 engine 关掉 EarlyAbove 时必须照旧升级（不改变既有行为）。
func TestEarlyAbstainDisabledFallsThroughToDeep(t *testing.T) {
	ctx := context.Background()
	kbE := kb.New(fast.New(mcs.KeywordScorer{Keywords: []string{"不存在词"}}),
		cluster.NewMemory(), cluster.Local{N: 64})
	e := New(kbE, NewMemoryConflict())
	h := abstain.Default()
	h.EarlyAbove = 0 // opt-out
	e.Abstain = h
	src := source.New("手册", "md", "", "m", "zh",
		strings.Repeat("无关内容。", 200), nil)

	res, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if res.AbstainEarly {
		t.Fatal("EarlyAbove=0 must never early-refuse")
	}
	if !res.Escalated {
		t.Fatal("historical escalate behavior must be preserved")
	}
}

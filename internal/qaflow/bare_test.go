package qaflow

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/flow"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/retrieval"
)

// 哑臂（Bare）是**零改写、零管理**：事实分解与上下文驱逐都不装。
// 这是 v0.2 §三.7 的"最笨基线"，也是"管线有没有增值"的唯一参照物——
// 没有它，任何改进数字都无法归因。
func TestBareRunnerSkipsManagement(t *testing.T) {
	idx := retrieval.Build(conflDocs())
	full := Runner("服务端口是多少", BM25Evidence(idx, 3, 60), offlineStub, opts())
	bare := Runner("服务端口是多少", BM25Evidence(idx, 3, 60), offlineStub, Options{
		CorpusVersion: "c1", ConfigVersion: "cfg1", StrategyVersion: "s1", BeliefVersion: "b1", Bare: true,
	})
	if bare.Flow != "qa-bare" {
		t.Fatalf("bare runner must be its own flow label, got %q", bare.Flow)
	}
	if full.Stages == nil || bare.Stages == nil {
		t.Fatal("both runners must carry stages")
	}
	bareNames := names(bare.Stages)
	for _, banned := range []string{"facts", "evict"} {
		if containsName(bareNames, banned) {
			t.Fatalf("bare runner must not carry %q stage: %v", banned, bareNames)
		}
	}
	if !containsName(bareNames, "evidence-supply") || !containsName(bareNames, "synthesize") {
		t.Fatalf("bare runner must still answer: %v", bareNames)
	}
	// 哑臂必须比全管线短，且不含事实二次判定（escalate 后的那次）
	if len(bare.Stages) >= len(full.Stages) {
		t.Fatalf("bare (%d stages) must be smaller than full (%d stages)", len(bare.Stages), len(full.Stages))
	}
}

// Bare 时可选件一律被忽略（哑臂的定义就是不用它们），且这件事可见：
// 提交视图的策略版本带 +bare 后缀。
func TestBareRunnerIgnoresOptionalPlugins(t *testing.T) {
	idx := retrieval.Build(conflDocs())
	c := context.New("default")
	o := Options{
		CorpusVersion: "c1", ConfigVersion: "cfg1", StrategyVersion: "s1", BeliefVersion: "b1",
		Bare:  true,
		Reuse: knowledge.NewReuseStore(),
	}
	o.Session = "s"
	if err := Runner("服务端口是多少", BM25Evidence(idx, 3, 60), offlineStub, o).Run(c); err != nil {
		t.Fatalf("bare run with plugins configured: %v", err)
	}
	views := c.Views()
	if len(views) != 1 {
		t.Fatalf("want 1 committed view, got %d", len(views))
	}
	if !strings.HasSuffix(views[0].StrategyVersion, "+bare") {
		t.Fatalf("bare mode must be visible in the committed view, got %q", views[0].StrategyVersion)
	}
	// 复用件被忽略 = 流程里没有复用记账 stage
	if containsName(names(Runner("x", BM25Evidence(idx, 3, 60), offlineStub, o).Stages), "reuse-record") {
		t.Fatal("bare runner must not carry reuse stages")
	}
}

func names(stages []flow.Stage) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		out = append(out, s.Name())
	}
	return out
}

func containsName(list []string, want string) bool {
	for _, n := range list {
		if n == want {
			return true
		}
	}
	return false
}

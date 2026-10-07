package qaflow

import (
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/retrieval"
)

// 回归：配了复用的流程，合成与记账两个 stage 必须在场。
// 这个 bug 真实存在过：复用分支按硬编码下标重建 stage 列表，Evict/
// Escalate 插入后合成与记账被静默丢掉——配复用的流程全部无声跳过
// 合成。HTTP 面首测（答案为空、usage 全零）才暴露。按名字定位后，
// 这个测试保证"复用配置不许改变 stage 集合的其余部分"。
func TestReuseConfigDoesNotDropStages(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100。"},
	})
	c := context.New("default")
	if err := Runner("连接池最大连接数是多少", BM25Evidence(idx, 3, 160), offlineStub, Options{
		CorpusVersion: "t", ConfigVersion: "t", StrategyVersion: "t", BeliefVersion: "t",
		Reuse: knowledge.NewReuseStore(), Session: "s-1",
	}).Run(c); err != nil {
		t.Fatal(err)
	}
	ans, ok := context.Get(c, KeyAnswer)
	if !ok || ans.Text == "" {
		t.Fatalf("reuse-configured flow must still synthesize: ok=%v %+v", ok, ans)
	}
	_, uok := context.Get(c, KeyUsage)
	if !uok {
		t.Fatal("reuse-configured flow must still account (KeyUsage present)")
	}
}

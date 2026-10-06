package qaflow

import (
	"testing"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/retrieval"
)

// 集成：金标首轮被词频压住（噪声三篇都含 连接池+端口），覆盖度不达标，
// 循环展开第二轮把 ops-1 取进窗口。这就是"一枪答不了就多取几轮"。
func TestBM25DeepRecoversGoldInRound2(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484；变更需值班经理审批并记录在案，回滚方案同步归档。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
		{ID: "sec-1", Body: "审计周期：审计每季度执行一次，审计报告归档保存三年备查。"},
		{ID: "noise-1", Body: "连接池巡检：连接池每季度检查一次连接情况。"},
	})
	retrieve := BM25DeepEvidence(idx, 60, DeepOptions{MaxRounds: 3, CoverageTarget: 1.0})
	c := context.New("default")
	// 四族事实的问句：首轮 k=3 只能覆盖三族，第四族逼出第二轮。
	// 每族事实各由一篇独持词文档承载（连接数/部署端口/收入/审计周期），
	// 首轮 top-3 必然漏一族——这不是调参调出来的，是 k=3  structurally
	// 装不下四族。
	if err := Runner("连接数是多少，服务端口是多少，收入怎么样，审计周期多长", retrieve, offlineStub, Options{
		CorpusVersion: "test", ConfigVersion: "test", StrategyVersion: "test", BeliefVersion: "test",
	}).Run(c); err != nil {
		t.Fatal(err)
	}
	tel, ok := context.Get(c, KeyDeep)
	if !ok {
		t.Fatal("deep telemetry must be sealed")
	}
	if tel.Rounds < 2 || tel.StopReason != "target" {
		t.Fatalf("must run a second round and stop at target: %+v", tel)
	}
	// 覆盖度必须爬到目标（每轮取进新事实）
	if n := len(tel.Coverage); n < 2 || tel.Coverage[n-1] < 1.0 {
		t.Fatalf("coverage must climb to target: %v", tel.Coverage)
	}
}

// 遥测必须进禁闭声明（EvidenceStage 写了 KeyDeep）——漏声明的回归钉死
func TestDeepKeyIsDeclared(t *testing.T) {
	st := EvidenceStage{}
	writes := map[string]bool{}
	for _, w := range st.Writes() {
		writes[w] = true
	}
	if !writes[KeyDeep.String()] {
		t.Fatalf("evidence stage must declare the deep telemetry key: %v", st.Writes())
	}
}

// MaxRounds=1 时行为与单轮检索一致（同函数承载两条路，可回退）
func TestDeepWithSingleRoundMatchesSingleShot(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置。"},
		{ID: "ops-1", Body: "部署手册：服务端口默认 8484。"},
	})
	q := "连接池最大连接数是多少"
	one, _ := BM25DeepEvidence(idx, 60, DeepOptions{MaxRounds: 1})(context.New("a"), Rewrite{Original: q})
	plain, _ := BM25Evidence(idx, 3, 60)(context.New("b"), Rewrite{Original: q})
	if len(one) != len(plain) {
		t.Fatalf("single-round deep must equal single-shot: %d vs %d", len(one), len(plain))
	}
	for i := range one {
		if one[i].SourceID != plain[i].SourceID {
			t.Fatalf("order must match at %d: %s vs %s", i, one[i].SourceID, plain[i].SourceID)
		}
	}
}

// countingEmbedder 数 embed 调用次数。深循环的 CPU 纪律：循环内一次
// 都不许 embed（每页 BM25），只许收敛后对最终窗口比一次。真实语料上
// 违反这条 = 300 题几万次 CPU 推理，机器被打满（实测事故）。
type countingEmbedder struct {
	calls int
	inner *fakeEmbedder
}

func (c *countingEmbedder) Dims() int { return c.inner.dims }

func (c *countingEmbedder) Embed(ctx gocontext.Context, texts []string) ([][]float32, error) {
	c.calls++
	return c.inner.Embed(ctx, texts)
}

func TestDeepEmbedsOnlyOnceAfterLoop(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484；变更需值班经理审批并记录在案，回滚方案同步归档。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
		{ID: "sec-1", Body: "审计周期：审计每季度执行一次，审计报告归档保存三年备查。"},
		{ID: "noise-1", Body: "连接池巡检：连接池每季度检查一次连接情况。"},
	})
	ce := &countingEmbedder{inner: &fakeEmbedder{vocab: []string{"连接", "端口"}, dims: 2}}
	c := context.New("default")
	if err := context.Set[embed.Embedder](c, KeyEmbedder, ce); err != nil {
		t.Fatal(err)
	}
	retrieve := BM25DeepEvidence(idx, 60, DeepOptions{MaxRounds: 3, CoverageTarget: 1.0})
	if err := Runner("连接数是多少，服务端口是多少，收入怎么样，审计周期多长", retrieve, offlineStub, Options{
		CorpusVersion: "test", ConfigVersion: "test", StrategyVersion: "test", BeliefVersion: "test",
	}).Run(c); err != nil {
		t.Fatal(err)
	}
	tel, _ := context.Get(c, KeyDeep)
	if tel.Rounds < 2 {
		t.Fatalf("precondition: loop must run multiple rounds, got %+v", tel)
	}
	if ce.calls != 1 {
		t.Fatalf("deep loop must embed exactly once (after convergence), got %d calls for %d rounds", ce.calls, tel.Rounds)
	}
}

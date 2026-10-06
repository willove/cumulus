package qaflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/flow"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/retrieval"
)

// offlineStub 是测试用的确定性合成：断言 = 窗口坐标，答案取第一条。
// 与 synth.Offline 同契约；测试里本地定义，免得测试依赖 synth 成环
// （synth 依赖 qaflow，测试再依赖回去就成环）。
func offlineStub(question string, windows []EvidenceWindow) (Answer, Usage, error) {
	if len(windows) == 0 {
		return Answer{}, Usage{}, errors.New("offlineStub: no windows")
	}
	ans := Answer{}
	for _, w := range windows {
		ans.Citations = append(ans.Citations, w.SourceID+"#"+w.Span)
	}
	ans.Text = ans.Citations[0]
	return ans, Usage{CostKnown: false}, nil
}

func stubRetrieve(_ *context.Context, r Rewrite) ([]EvidenceWindow, error) {
	return []EvidenceWindow{{SourceID: "doc-1", Span: "第3条", Score: 0.9, Substrate: "text"}}, nil
}

func opts() Options {
	return Options{CorpusVersion: "c1", ConfigVersion: "cfg1", StrategyVersion: "s1", BeliefVersion: "b1"}
}

// rogueStage 故意写声明之外的 key，验证禁闭。
type rogueStage struct{}

func (rogueStage) Name() string     { return "rogue" }
func (rogueStage) Reads() []string  { return nil }
func (rogueStage) Writes() []string { return nil }
func (rogueStage) Run(c *context.Context) error {
	return context.Set(c, KeyAnswer, Answer{})
}
func (rogueStage) Verify(c *context.Context) error { return nil }

func runnerWith(stages ...flow.Stage) *flow.Runner {
	return &flow.Runner{Flow: "test", Stages: stages}
}

// 跑通一次全流程：六步全过，提交视图落账。
func TestRunnerHappyPath(t *testing.T) {
	c := context.New("tenant_a")
	r := Runner("连接池最大连接数是多少", stubRetrieve, offlineStub, opts())
	if err := r.Run(c); err != nil {
		t.Fatalf("run: %v", err)
	}
	views := c.Views()
	if len(views) != 1 {
		t.Fatalf("want 1 committed view, got %d", len(views))
	}
	v := views[0]
	if v.Flow != "qa" || v.Realm != "tenant_a" || v.CorpusVersion != "c1" {
		t.Fatalf("bad view: %+v", v)
	}
	ans, ok := context.Get(c, KeyAnswer)
	if !ok || ans.Refused {
		t.Fatalf("expected a cited stub answer, got %+v ok=%v", ans, ok)
	}
	if len(ans.Citations) != 1 || !strings.HasPrefix(ans.Citations[0], "doc-1#") {
		t.Fatalf("citation must resolve to the window, got %v", ans.Citations)
	}
}

// 没有证据窗口时，路由必须拒答而不是硬答（诚实的不知道）。
func TestRouteRefusesWithoutEvidence(t *testing.T) {
	c := context.New("default")
	empty := func(*context.Context, Rewrite) ([]EvidenceWindow, error) { return nil, nil }
	if err := Runner("任意问题", empty, offlineStub, opts()).Run(c); err != nil {
		t.Fatalf("run: %v", err)
	}
	d, _ := context.Get(c, KeyRoute)
	if d.Action != "refuse" {
		t.Fatalf("want refuse, got %q", d.Action)
	}
}

// 禁闭：stage 写声明之外的 key，必须失败并反卷。
func TestConfinementRejectsUndeclaredWrite(t *testing.T) {
	c := context.New("default")
	rogue := rogueStage{}
	r := runnerWith(rogue)
	err := r.Run(c)
	if err == nil {
		t.Fatal("expected confinement error")
	}
	if !strings.Contains(err.Error(), "undeclared key") {
		t.Fatalf("want confinement error, got: %v", err)
	}
}

// 反卷：中途失败后，本流程写过的 key 必须消失，之前的注册不动。
func TestUnwindRollsBackFlowRegistrations(t *testing.T) {
	c := context.New("default")
	// 流程之前就存在的注册（模拟摄取阶段）
	if err := context.Set(c, KeyRewrite, Rewrite{Original: "pre-existing"}); err != nil {
		t.Fatal(err)
	}
	mark := c.Mark()
	// 流程内的注册随后被撤销
	if err := context.Set(c, KeyUsage, Usage{PromptTokens: 5, CostKnown: true}); err != nil {
		t.Fatal(err)
	}
	notes := c.UnwindTo(mark)
	if len(notes) != 1 {
		t.Fatalf("want 1 unwound, got %d (%v)", len(notes), notes)
	}
	if _, ok := context.Get(c, KeyUsage); ok {
		t.Fatal("usage key must be gone after unwind")
	}
	if r, ok := context.Get(c, KeyRewrite); !ok || r.Original != "pre-existing" {
		t.Fatal("pre-flow registration must survive")
	}
	if len(c.Views()) != 0 {
		t.Fatal("failed flow must not commit a view")
	}
}

// 注册没有逆必须被拒（不变量 2）。
func TestRegisterWithoutReleaseRejected(t *testing.T) {
	c := context.New("default")
	err := c.Register(context.Registration{Key: "x.y", Note: "no release"})
	if err == nil {
		t.Fatal("expected rejection")
	}
}

// 窗口缺 span：引用不可回溯，Verify 必须拦下。
func TestEvidenceVerifyRejectsUnresolvableWindow(t *testing.T) {
	c := context.New("default")
	bad := func(*context.Context, Rewrite) ([]EvidenceWindow, error) {
		return []EvidenceWindow{{SourceID: "doc-1", Span: ""}}, nil
	}
	err := Runner("q", bad, offlineStub, opts()).Run(c)
	if err == nil || !strings.Contains(err.Error(), "span") {
		t.Fatalf("want span error, got: %v", err)
	}
}

// 真 BM25 接进 evidence stage：全流程跑通，且引用必须能回溯到原文坐标。
func TestBM25EndToEnd(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
	})
	c := context.New("tenant_a")
	r := Runner("连接池最大连接数是多少", BM25Evidence(idx, 3, 60), offlineStub, opts())
	if err := r.Run(c); err != nil {
		t.Fatalf("run: %v", err)
	}
	ws, ok := context.Get(c, KeyWindows)
	if !ok || len(ws) == 0 {
		t.Fatal("bm25 must produce evidence windows")
	}
	if ws[0].SourceID != "law-1" {
		t.Fatalf("law doc must rank first, got %s", ws[0].SourceID)
	}
	// 每个窗口的坐标必须能在原文里还原出非空片段
	for _, w := range ws {
		d, ok := idx.Doc(w.SourceID)
		if !ok {
			t.Fatalf("window references unknown doc %s", w.SourceID)
		}
		text, err := retrieval.ResolveSpan(d.Body, w.Span)
		if err != nil || text == "" {
			t.Fatalf("window span must resolve: doc=%s span=%s err=%v", w.SourceID, w.Span, err)
		}
	}
	ans, _ := context.Get(c, KeyAnswer)
	if len(ans.Citations) != 1 || !strings.HasPrefix(ans.Citations[0], "law-1#") {
		t.Fatalf("citation must resolve to the law window, got %v", ans.Citations)
	}
}

// 无命中查询必须走拒答，不许硬答（诚实的不知道）。
func TestBM25NoHitRefuses(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{{ID: "a", Body: "连接池配置说明。"}})
	c := context.New("default")
	if err := Runner("量子引力飞船怎么造", BM25Evidence(idx, 3, 60), offlineStub, opts()).Run(c); err != nil {
		t.Fatalf("run: %v", err)
	}
	d, _ := context.Get(c, KeyRoute)
	a, _ := context.Get(c, KeyAnswer)
	if d.Action != "refuse" || !a.Refused {
		t.Fatalf("want refuse, got route=%s refused=%v", d.Action, a.Refused)
	}
}

// 复用按会话记录 + 命中：同一问题再问，直接取上轮窗口，本轮不再检索。
// 判据是检索调用次数（冷过一次），不是"答案对不对"——复用管的是
// 不走回头路（belief 全局声望版已被真实语料证伪退役，原设计按查询/
// 按会话的正确形态从这里开始）。
func TestReuseShortCircuitsSecondAsk(t *testing.T) {
	// 复用测试不需要干扰语料（复用的是上轮窗口本身），单文档语料即可
	idx := retrieval.Build([]retrieval.Document{
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
		{ID: "cost-a", Body: "成本结构与分摊方法：成本按部门分摊。"},
	})
	store := knowledge.NewReuseStore()

	calls := 0
	retrieve := func(c *context.Context, r Rewrite) ([]EvidenceWindow, error) {
		calls++
		return BM25Evidence(idx, 3, 60)(c, r)
	}
	run := func(q string) *context.Context {
		c := context.New("default")
		if err := Runner(q, retrieve, offlineStub, Options{
			CorpusVersion: "test", ConfigVersion: "test", StrategyVersion: "test", BeliefVersion: "test",
			Reuse: store, Session: "s-1",
		}).Run(c); err != nil {
			t.Fatal(err)
		}
		return c
	}

	c1 := run("成本结构怎么样")
	if calls != 1 {
		t.Fatalf("first ask must retrieve, calls=%d", calls)
	}
	ws1, _ := context.Get(c1, KeyWindows)

	c2 := run("成本结构怎么样")
	if calls != 1 {
		t.Fatalf("second ask must NOT retrieve, calls=%d", calls)
	}
	rs, _ := context.Get(c2, KeyReuseState)
	if !rs.Hit {
		t.Fatalf("second ask must be a reuse hit: %+v", rs)
	}
	ws2, _ := context.Get(c2, KeyWindows)
	if len(ws2) != len(ws1) {
		t.Fatalf("reused windows must match, got %v vs %v", ws2, ws1)
	}

	run("连接池最大连接数是多少")
	if calls != 2 {
		t.Fatalf("different question must retrieve, calls=%d", calls)
	}
}

// 拒答不记：拒答的经验没有复用价值。
func TestReuseSkipsRefusedAnswer(t *testing.T) {
	store := knowledge.NewReuseStore()
	retrieve := func(c *context.Context, r Rewrite) ([]EvidenceWindow, error) {
		return nil, nil
	}
	c := context.New("default")
	if err := Runner("量子引力飞船怎么造", retrieve, offlineStub, Options{
		CorpusVersion: "test", ConfigVersion: "test", StrategyVersion: "test", BeliefVersion: "test",
		Reuse: store, Session: "s-1",
	}).Run(c); err != nil {
		t.Fatal(err)
	}
	if n := store.Len("s-1"); n != 0 {
		t.Fatalf("refused answer must not be recorded, got %d entries", n)
	}
}

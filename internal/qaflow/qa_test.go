package qaflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/flow"
	"github.com/willove/cumulus/internal/knowledge/belief"
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

// 信念重排：观测过、没产出的文档下沉，金标从 BM25 的窗外捞回前三。
func TestBeliefBoostRescuesGoldFromOutsideTopK(t *testing.T) {
	corpus := []retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "cost-a", Body: "成本结构与分摊方法：成本按部门分摊，成本结构按季度复盘，成本口径见附则。"},
		{ID: "cost-b", Body: "成本结构与定价：成本结构决定底线，成本结构变动需重新定价，成本归集周期一月。"},
		{ID: "cost-c", Body: "成本结构与预算：成本结构分解到项目，成本结构偏差超百分之五需说明，成本台账按月."},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
	}
	idx := retrieval.Build(corpus)

	// 无信念：前三名是三个干扰文档（BM25 词频决定的真排序）
	c := context.New("default")
	if err := Runner("成本结构怎么样", BM25Evidence(idx, 3, 60), offlineStub, opts()).Run(c); err != nil {
		t.Fatal(err)
	}
	ws, _ := context.Get(c, KeyWindows)
	if len(ws) != 3 {
		t.Fatalf("want 3 windows, got %d", len(ws))
	}
	for _, w := range ws {
		if w.SourceID == "fin-1" {
			t.Fatal("without belief, gold must be pushed out of top-3 (this is the failure the corpus encodes)")
		}
	}

	// 观测：三个干扰文档零产出，fin-1 有产出
	b := belief.New(nil, 0.5)
	b.Observe("cost-a", 0)
	b.Observe("cost-b", 0)
	b.Observe("cost-c", 0)
	b.Observe("fin-1", 1)

	// 绑信念重跑：fin-1 回到前三
	c2 := context.New("default")
	if err := BindBelief(c2, b); err != nil {
		t.Fatal(err)
	}
	if err := Runner("成本结构怎么样", BM25Evidence(idx, 3, 60), offlineStub, opts()).Run(c2); err != nil {
		t.Fatal(err)
	}
	ws2, _ := context.Get(c2, KeyWindows)
	found := false
	for _, w := range ws2 {
		if w.SourceID == "fin-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("belief must rescue gold into top-3, got %v", ws2)
	}
}

// 分类器驱动：信念绑上→组件激活；信念被撤销→组件停用。
// “依赖没了还在跑”在这个结构里无法表达。
func TestBeliefBoosterFollowsClassifier(t *testing.T) {
	c := context.New("default")
	booster := &BeliefBooster{}
	c.RegisterComponent(booster)

	// 没绑信念：组件未激活，缺什么列得出来
	if booster.Active() {
		t.Fatal("must not activate before belief is bound")
	}
	states := c.Components()
	if len(states) != 1 || len(states[0].Missing) != 1 || states[0].Missing[0] != KeyBelief.String() {
		t.Fatalf("missing dep must be visible: %+v", states)
	}

	// 绑上：激活
	b := belief.New(nil, 0.5)
	if err := BindBelief(c, b); err != nil {
		t.Fatal(err)
	}
	if !booster.Active() {
		t.Fatal("bind must activate the booster")
	}

	// 撤销到未绑定：停用
	mark := c.Mark()
	if err := BindBelief(c, b); err != nil {
		t.Fatal(err)
	}
	if !booster.Active() {
		t.Fatal("rebind must keep active (neutral)")
	}
	_ = c.UnwindTo(mark)
	if !booster.Active() {
		t.Fatal("restore-to-present must keep active")
	}
	_ = c.UnwindTo(0)
	if booster.Active() {
		t.Fatal("unbind must deactivate the booster")
	}
}

package qaflow

import (
	"math/rand"
	"reflect"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
)

// 合流测试：Cordis 定理 80（反复增删替换组件，终态等同于从一开始就按
// 最终方案装配）的可执行化（v0.2 §三.6）。
//
// 三断言，固定种子、失败可重放：
//  1. **无悬挂**：任意时刻没有"活着的组件缺依赖"；被卸下的注册提供的
//     key 不再出现在绑定集里（分类器把停用做成了事实，不只是标签）；
//  2. **逆干净**：随机装卸后回到 mark，绑定键集与装卸前完全一致，
//     且注册的逆确实按 LIFO 跑过（资源被释放，不是被遗忘）；
//  3. **行为等价**：一条脏历史（中间反复装卸）终态为 S 的上下文，跑出
//     的答案/引用/窗口/路由与"一开始就按 S 静态装配"的上下文逐字段相同。
//
// 边界（诚实记）：本测试覆盖 context 注册与可选组件这一层——stage 列表
// 目前在装配期定死（Runner 构造时），还没有运行期装卸 stage 的 API。
// 把 stage 也做成注册之后，这三条断言可以原样扩到 stage 层。

// conflDoc 是合流测试用的固定语料（两篇，够区分 BM25 序）。
func conflDocs() []retrieval.Document {
	return []retrieval.Document{
		{ID: "ops", Body: "部署手册：服务端口默认 8484，先改配置再重启服务。"},
		{ID: "db", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列。"},
	}
}

// conflOps 是一串随机装卸操作。固定种子的序列可重放（失败能复现）。
type conflOp int

const (
	opBindEmbedder conflOp = iota
	opSetAnalysis
	opRegisterEffect
	opUnwind
	opCheckOnly
)

const conflOpKinds = 5

// conflApply 把一条操作作用到 context 上，返回它是否改变了"终态"。
// 是否终结由调用方按 opUnwind 处理。
func conflApply(t *testing.T, c *context.Context, op conflOp, mark int, released *int) {
	t.Helper()
	switch op {
	case opBindEmbedder:
		if err := BindEmbedder(c, &fakeEmbedder{vocab: []string{"端口", "连接"}, dims: 2}); err != nil {
			t.Fatalf("bind embedder: %v", err)
		}
	case opSetAnalysis:
		if err := context.Set(c, KeyAnalysis, query.Analysis{
			Intent:     "search",
			Primary:    map[string]float64{"端口": 2.0},
			TotalTerms: 2,
		}); err != nil {
			t.Fatalf("set analysis: %v", err)
		}
	case opRegisterEffect:
		if err := c.Register(context.Registration{
			Key: "test.effect", Note: "confluence test effect",
			Release: func() { *released++ },
		}); err != nil {
			t.Fatalf("register effect: %v", err)
		}
	case opUnwind:
		c.UnwindTo(mark)
	case opCheckOnly:
		// 什么都不做：只为了让"每一步之后都断言"这句话成立
	}
	assertNoDangling(t, c)
}

// assertNoDangling 是断言 1：分类器不可能产出"活着但缺依赖"的组件；
// 反向也成立——依赖没了的组件必须已经不活。
func assertNoDangling(t *testing.T, c *context.Context) {
	t.Helper()
	for _, cs := range c.Components() {
		if cs.Active && len(cs.Missing) > 0 {
			t.Fatalf("dangling: component %s is active with missing deps %v", cs.Name, cs.Missing)
		}
	}
	// 提供者缺席时，它提供的 key 不许还在绑定集里，组件也不许还是活的
	// ——"停了"要是事实，不是标签。
	if !conflHasKey(c, KeyEmbedder.String()) {
		for _, cs := range c.Components() {
			if cs.Name == (SemanticRerank{}).Name() && cs.Active {
				t.Fatal("semantic-rerank is active while embed.embedder is unbound")
			}
		}
	}
}

func conflHasKey(c *context.Context, key string) bool {
	for _, k := range c.Keys() {
		if k == key {
			return true
		}
	}
	return false
}

// conflFinalKeys 提取一条操作序列的终态（最后一次 unwind 之后的绑定意图）。
func conflFinalKeys(ops []conflOp) []conflOp {
	var final []conflOp
	for _, op := range ops {
		if op == opUnwind {
			final = nil
			continue
		}
		final = append(final, op)
	}
	return final
}

// conflApplyFinal 按终态静态装配（"从一开始就按最终方案装配"那一侧）。
func conflApplyFinal(t *testing.T, c *context.Context, final []conflOp, released *int) {
	t.Helper()
	for _, op := range final {
		conflApply(t, c, op, 0, released)
	}
}

// conflRun 跑一次问答，返回可逐字段比对的观察量。
type conflRun struct {
	answer    string
	citations []string
	action    string
	windows   int
	rerank    RerankState
	coverage  float64
}

func conflRunOnce(t *testing.T, c *context.Context, idx *retrieval.Index) conflRun {
	t.Helper()
	if err := Runner("服务端口是多少", BM25Evidence(idx, 3, 60), offlineStub, opts()).Run(c); err != nil {
		t.Fatalf("run: %v", err)
	}
	a, _ := context.Get(c, KeyAnswer)
	r, _ := context.Get(c, KeyRoute)
	ws, _ := context.Get(c, KeyWindows)
	rr, _ := context.Get(c, KeyRerank)
	cov, _ := context.Get(c, KeyCoverage)
	return conflRun{answer: a.Text, citations: a.Citations, action: r.Action, windows: len(ws), rerank: rr, coverage: cov.Value}
}

func TestConfluenceRandomMountUnmount(t *testing.T) {
	idx := retrieval.Build(conflDocs())
	for seed := int64(0); seed < 12; seed++ {
		rng := rand.New(rand.NewSource(seed))
		ops := make([]conflOp, 0, 12)
		for i := 0; i < 12; i++ {
			ops = append(ops, conflOp(rng.Intn(conflOpKinds)))
		}
		final := conflFinalKeys(ops)

		// ---- 第一遍：装卸 + 无悬挂 + 逆干净 ----
		c := context.New("confluence")
		c.RegisterComponent(&SemanticRerank{})
		mark := c.Mark()
		keys0 := c.Keys()
		released := 0
		for _, op := range ops {
			conflApply(t, c, op, mark, &released)
		}
		c.UnwindTo(mark)
		if !reflect.DeepEqual(c.Keys(), keys0) {
			t.Fatalf("seed %d: unwind is not clean: keys %v → %v", seed, keys0, c.Keys())
		}
		// 每次注册的逆都要跑到，且全程只跑一次——注册了没人撤就是"逆不
		// 成立"（Cordis 把见证列为作者义务的那条）。
		if want := conflCount(ops, opRegisterEffect); released != want {
			t.Fatalf("seed %d: %d registrations but %d releases (unwind must replay every inverse exactly once)", seed, want, released)
		}
		assertNoDangling(t, c)

		// ---- 第二遍：同一种子重放，得到一条有历史的上下文（终态 = final）----
		dirty := context.New("confluence")
		dirty.RegisterComponent(&SemanticRerank{})
		dirtyMark := dirty.Mark()
		rel2 := 0
		for _, op := range ops {
			conflApply(t, dirty, op, dirtyMark, &rel2)
		}
		// ---- 第三遍：静态装配（从一开始就按 final 装）----
		clean := context.New("confluence")
		clean.RegisterComponent(&SemanticRerank{})
		rel3 := 0
		conflApplyFinal(t, clean, final, &rel3)

		got := conflRunOnce(t, dirty, idx)
		want := conflRunOnce(t, clean, idx)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: dirty history ≠ static assembly\n dirty=%+v\n clean=%+v", seed, got, want)
		}
	}
}

func conflCount(ops []conflOp, kind conflOp) int {
	n := 0
	for _, op := range ops {
		if op == kind {
			n++
		}
	}
	return n
}

// 同一问题的两种装配（带复用件 vs 不带）在"复用库为空"时必须等价：
// 可选件的存在不该改变行为——这是"可选组件不静默改语义"的可执行版。
func TestConfluenceReuseWithEmptyStore(t *testing.T) {
	idx := retrieval.Build(conflDocs())
	plain := context.New("confluence")
	got := conflRunOnce(t, plain, idx)

	withReuse := context.New("confluence")
	optsReuse := opts()
	optsReuse.Reuse = knowledge.NewReuseStore()
	optsReuse.Session = "s-confluence"
	if err := Runner("服务端口是多少", BM25Evidence(idx, 3, 60), offlineStub, optsReuse).Run(withReuse); err != nil {
		t.Fatalf("run with reuse: %v", err)
	}
	a, _ := context.Get(withReuse, KeyAnswer)
	r, _ := context.Get(withReuse, KeyRoute)
	ws, _ := context.Get(withReuse, KeyWindows)
	rr, _ := context.Get(withReuse, KeyRerank)
	cov, _ := context.Get(withReuse, KeyCoverage)
	want := conflRun{answer: a.Text, citations: a.Citations, action: r.Action, windows: len(ws), rerank: rr, coverage: cov.Value}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty reuse store changed behaviour:\n plain=%+v\n reuse=%+v", got, want)
	}
}

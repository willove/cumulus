package main

import (
	"fmt"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/knowledge/belief"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
)

// bm25Executor 是 evalfcore 的执行面：对冻结语料建一次索引，
// 每题走 qaflow 全流程（真检索、真路由、真验证）。
//
// 答案取抽取式基线：第一个可回溯窗口的原文片段。qaflow 的合成步还是
// TODO 桩（未接 LLM），所以这里自己从证据抽——规则臂因此跑得出真数字；
// 等 LLM 合成接上，这一行换回读 qaflow.KeyAnswer 的文本。
//
// 隔离靠索引只读：executor 不写业务集合；gold（金标）在签名里不存在——
// 执行面看不到答案，闭卷与防泄漏是结构性的。
type bm25Executor struct {
	idx    *retrieval.Index
	knobs  map[string]float64 // evidence.topk / evidence.width
	belief *belief.Belief     // 可空：候选区信念（绑了才用）
}

func (e *bm25Executor) topk() int {
	if v, ok := e.knobs["evidence.topk"]; ok && v >= 1 {
		return int(v)
	}
	return 3
}

func (e *bm25Executor) width() int {
	if v, ok := e.knobs["evidence.width"]; ok && v >= 1 {
		return int(v)
	}
	return 60
}

func (e *bm25Executor) Answer(_ gocontext.Context, question string) (evalfcore.ItemOutcome, error) {
	c := context.New("eval-sandbox")
	if e.belief != nil {
		if err := qaflow.BindBelief(c, e.belief); err != nil {
			return evalfcore.ItemOutcome{}, err
		}
	}
	r := qaflow.Runner(question, qaflow.BM25Evidence(e.idx, e.topk(), e.width()), qaflow.Options{
		CorpusVersion:   "frozen",
		ConfigVersion:   "eval",
		StrategyVersion: "v0.1",
		BeliefVersion:   "none",
	})
	if err := r.Run(c); err != nil {
		return evalfcore.ItemOutcome{}, fmt.Errorf("qaflow: %w", err)
	}
	route, _ := context.Get(c, qaflow.KeyRoute)
	answer, _ := context.Get(c, qaflow.KeyAnswer)
	windows, _ := context.Get(c, qaflow.KeyWindows)

	out := evalfcore.ItemOutcome{
		Refused:     answer.Refused,
		RouteAction: route.Action,
		Windows:     len(windows),
	}
	// 抽取式答案：第一个可回溯窗口的原文片段；其余窗口全部计引用。
	for _, w := range windows {
		d, ok := e.idx.Doc(w.SourceID)
		if !ok {
			continue
		}
		text, err := retrieval.ResolveSpan(d.Body, w.Span)
		if err != nil {
			out.Cited = append(out.Cited, evalfcore.Citation{DocID: w.SourceID, Span: w.Span, Resolved: false})
			continue
		}
		if out.Answer == "" {
			out.Answer = text
		}
		out.Cited = append(out.Cited, evalfcore.Citation{DocID: w.SourceID, Span: w.Span, Resolved: true})
	}
	return out, nil
}

// runEval 跑一次评测 episode 并打印摘要。语料与题集内联（selftest 性质）；
// 接上 store LoadFromStore 后改从库里读。
func defaultKnobs() map[string]float64 {
	return map[string]float64{"evidence.topk": 3, "evidence.width": 60}
}

func runEval(ctx gocontext.Context) error {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
	})
	ds, err := evalfcore.NewDataset([]evalfcore.Item{
		{ID: "q1", Question: "连接池最大连接数是多少", Answer: "100", GoldIDs: []string{"law-1"}},
		{ID: "q2", Question: "默认端口是多少", Answer: "8484", GoldIDs: []string{"ops-1"}},
		{ID: "q3", Question: "成本结构怎么样", Answer: "优化", GoldIDs: []string{"fin-1"}},
	})
	if err != nil {
		return err
	}
	st, err := store.Open("", true)
	if err != nil {
		return err
	}
	fp := evalfcore.Fingerprints{
		ItemsSHA:  ds.ItemsSHA,
		CorpusSHA: "corpus-selftest",
		ConfigSHA: evalfcore.Config{Arms: []string{"rule"}, Model: "offline-stub"}.SHA(),
	}
	runner := evalfcore.NewRunner(evalfcore.NewKVStore(st), fp, &bm25Executor{idx: idx}, nil)
	state, err := runner.Start(ctx, "run-selftest", ds.Items)
	if err != nil {
		return err
	}
	fmt.Printf("dataset %s items=%d\n", ds.ID[:12], len(ds.Items))
	fmt.Printf("fingerprints items=%s corpus=%s config=%s\n", fp.ItemsSHA[:12], fp.CorpusSHA, fp.ConfigSHA[:12])
	for _, r := range state.Results {
		f := "ok"
		if r.Failure != "" {
			f = r.Failure
		}
		fmt.Printf("  %s rule=%.0f evidence=%v cites=%d/%d failure=%s\n",
			r.ItemID, r.RuleScore, r.EvidenceHit, r.CitationsResolved, r.CitationsTotal, f)
	}
	fmt.Println("summary:", evalfcore.Summarize(state))
	return nil
}

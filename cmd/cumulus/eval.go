package main

import (
	"fmt"
	"os"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/knowledge/belief"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
)

// bm25Executor 是 evalfcore 的执行面：对冻结语料建一次索引，
// 每题走 qaflow 全流程（真检索、真路由、真合成、真记账）。
//
// 隔离靠索引只读：executor 不写业务集合；gold（金标）在签名里不存在——
// 执行面看不到答案，闭卷与防泄漏是结构性的。
//
// 合成面用抽取式基线：答案 = 第一个可回溯窗口的原文片段。与
// internal/synth 的 LLM 版同契约；抽取式的意义在规则臂——它读的是
// 语料原话，不是生成。生产把这里换成 synth.LLM 即可，别处不动。
type bm25Executor struct {
	idx     *retrieval.Index
	knobs   map[string]float64 // evidence.topk / evidence.width
	belief  *belief.Belief     // 可空：候选区信念（绑了才用）
	synthFn qaflow.SynthFunc   // 合成面：offline 或 llm
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

// synth 是抽取式合成（qaflow.SynthFunc 契约）：每个窗口一条断言，
// 答案取第一个窗口的原文。
func (e *bm25Executor) synth() qaflow.SynthFunc {
	return func(_ string, ws []qaflow.EvidenceWindow) (qaflow.Answer, qaflow.Usage, error) {
		if len(ws) == 0 {
			return qaflow.Answer{}, qaflow.Usage{}, fmt.Errorf("extractive synth: no windows")
		}
		ans := qaflow.Answer{}
		for _, w := range ws {
			if ans.Text == "" {
				ans.Text = w.Text
			}
			ans.Citations = append(ans.Citations, w.SourceID+"#"+w.Span)
		}
		return ans, qaflow.Usage{CostKnown: false}, nil
	}
}

func (e *bm25Executor) Answer(_ gocontext.Context, question string) (evalfcore.ItemOutcome, error) {
	c := context.New("eval-sandbox")
	if e.belief != nil {
		if err := qaflow.BindBelief(c, e.belief); err != nil {
			return evalfcore.ItemOutcome{}, err
		}
	}
	synthFn := e.synthFn
	if synthFn == nil {
		synthFn = e.synth()
	}
	r := qaflow.Runner(question, qaflow.BM25Evidence(e.idx, e.topk(), e.width()), synthFn, qaflow.Options{
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
	usage, _ := context.Get(c, qaflow.KeyUsage)

	out := evalfcore.ItemOutcome{
		Answer:           answer.Text,
		Refused:          answer.Refused,
		RouteAction:      route.Action,
		Windows:          len(windows),
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		CostKnown:        usage.CostKnown,
	}
	for _, w := range windows {
		out.Cited = append(out.Cited, evalfcore.Citation{DocID: w.SourceID, Span: w.Span, Resolved: true})
	}
	return out, nil
}

func defaultKnobs() map[string]float64 {
	return map[string]float64{"evidence.topk": 3, "evidence.width": 60}
}

func runEval(ctx gocontext.Context) error {
	idx := retrieval.Build(evalCorpus())
	ds, err := evalfcore.NewDataset(evalItems())
	if err != nil {
		return err
	}
	st, err := store.Open("", true)
	if err != nil {
		return err
	}
	synthFn, synthLabel, err := pickSynth(os.Getenv("CUMULUS_SYNTH"))
	if err != nil {
		return err
	}
	j, err := judgeFromEnv(os.Getenv("CUMULUS_JUDGE"))
	if err != nil {
		return err
	}
	fmt.Printf("synth: %s  judge: %s\n", synthLabel, judgeLabel(os.Getenv("CUMULUS_JUDGE")))
	fp := evalfcore.Fingerprints{
		ItemsSHA:  ds.ItemsSHA,
		CorpusSHA: "corpus-selftest",
		ConfigSHA: evalfcore.Config{Arms: []string{"rule"}, Model: "offline-stub"}.SHA(),
	}
	runner := evalfcore.NewRunner(evalfcore.NewKVStore(st), fp, &bm25Executor{idx: idx, knobs: defaultKnobs(), synthFn: synthFn}, j)
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

func evalCorpus() []retrieval.Document {
	return []retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
	}
}

func evalItems() []evalfcore.Item {
	return []evalfcore.Item{
		{ID: "q1", Question: "连接池最大连接数是多少", Answer: "100", GoldIDs: []string{"law-1"}},
		{ID: "q2", Question: "默认端口是多少", Answer: "8484", GoldIDs: []string{"ops-1"}},
		{ID: "q3", Question: "成本结构怎么样", Answer: "优化", GoldIDs: []string{"fin-1"}},
	}
}

func judgeLabel(which string) string {
	if which == "llm" {
		return "llm"
	}
	return "none (N/A)"
}

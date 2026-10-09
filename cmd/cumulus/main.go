// Command cumulus 是 CLI 入口。v0.1 只有一个自检命令：
// 跑骨架问答流程，打印提交视图，验证不变量的执行处是活的。
package main

import (
	gocontext "context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/ctxmgmt"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cumulus:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	loadDotEnv(".env")
	if len(args) == 0 {
		return fmt.Errorf("usage: cumulus selftest [-realm R] | cumulus eval | cumulus learn")
	}
	switch args[0] {
	case "selftest":
		return runSelftest(args[1:])
	case "eval":
		return runEval(gocontext.Background())
	case "learn":
		return runLearn(gocontext.Background())
	case "serve":
		return runServe(args[1:])
	case "signals":
		return runSignals(args[1:])
	default:
		return fmt.Errorf("unknown command %q; usage: cumulus selftest | cumulus eval | cumulus learn | cumulus serve | cumulus signals", args[0])
	}
}

func runSelftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	realm := fs.String("realm", "default", "isolation realm (namespace)")
	synthFlag := fs.String("synth", "offline", "synthesis backend: offline | llm")
	embedFlag := fs.String("embed", "off", "embedding backend: off | minilm")
	if err := fs.Parse(args); err != nil {
		return err
	}
	synthFn, _, synthLabel, err := pickSynth(*synthFlag)
	if err != nil {
		return err
	}

	c := context.New(context.Realm(*realm))

	// 可选组件走分类器：登记后由 context 按依赖驱动启停。语义重排只
	// 要求 embedder 绑着（-embed minilm 时激活，否则可见地停用）。
	reranker := &qaflow.SemanticRerank{}
	c.RegisterComponent(reranker)
	if *embedFlag == "minilm" {
		embFn, _, err := pickEmbed(*embedFlag)
		if err != nil {
			return err
		}
		if err := qaflow.BindEmbedder(c, embFn()); err != nil {
			return err
		}
	}

	// 复用件（会话内）：第一问记、第二问取
	reuse := knowledge.NewReuseStore()

	// 真语料、真检索：倒排索引 + BM25（cumulus 验证过的那套）。
	// 语料此刻由 selftest 内联给出；接上 store LoadFromStore 后改从库里读。
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484；变更需值班经理审批并记录在案，回滚方案同步归档。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
		// 深循环演示用：三个词面沾边但不覆盖"部署端口"的干扰文档——
		// 单轮 top-3 被它们占满，覆盖度不达标，循环必须展开第二轮
		{ID: "noise-1", Body: "连接池巡检记录：连接池每季度检查一次，记录在案。"},
		{ID: "noise-2", Body: "连接池应急演练：连接池告警时先扩容再排查。"},
		{ID: "noise-3", Body: "连接池操作基线：连接池日常操作按基线执行。"},
	})
	selftestIdx = idx

	// 升级贵路：充足性判了 escalate 才跑（BioHarness 级联）
	escalateBackend := qaflow.BM25DeepEvidence(idx, 160, qaflow.DefaultDeep())
	r := qaflow.Runner("连接池最大连接数是多少", qaflow.BM25Evidence(idx, 3, 60), synthFn, qaflow.Options{
		CorpusVersion:   "selftest",
		ConfigVersion:   "selftest",
		StrategyVersion: "v0.1",
		BeliefVersion:   "none",
		Reuse:           reuse,
		Session:         "selftest",
		Escalate:        escalateBackend,
	})
	if err := r.Run(c); err != nil {
		return err
	}

	fmt.Printf("synth: %s\n", synthLabel)
	printAsk("first ask (cold)  ", c)

	// —— 深循环演示：同一问句，证据不够就多取几轮 ——
	cDeep := context.New(context.Realm(*realm))
	if err := qaflow.Runner("连接池的部署端口是多少", qaflow.BM25DeepEvidence(idx, 60, qaflow.DeepOptions{
		MaxRounds: 3, CoverageTarget: 1.0,
	}), synthFn, qaflow.Options{
		CorpusVersion: "selftest", ConfigVersion: "selftest", StrategyVersion: "v0.1", BeliefVersion: "none",
		Reuse: reuse, Session: "selftest",
		Escalate: escalateBackend,
		// 紧预算：深循环攒了 5 个窗口，预算只留 3 个——驱逐真实发生，
		// 账目打出来（合成前最后一道上下文管理）
		CtxBudget: ctxmgmt.Budget{MaxWindows: 3, PerSourceMax: 2, DedupCosine: 0.92},
	}).Run(cDeep); err != nil {
		return err
	}
	if tel, ok := context.Get(cDeep, qaflow.KeyDeep); ok {
		fmt.Printf("deep: rounds=%d sampled=%d dead-ends=%d coverage=%v stop=%s\n",
			tel.Rounds, tel.SampledDocs, tel.DeadEnds, tel.Coverage, tel.StopReason)
		if ci, ok := context.Get(cDeep, qaflow.KeyCoverage); ok {
			fmt.Printf("deep: coverage=%.3f out-of-corpus terms (not in denominator): %v\n", ci.Value, ci.OOV)
		}
	}
	if ws, ok := context.Get(cDeep, qaflow.KeyWindows); ok {
		for _, w := range ws {
			fmt.Printf("deep window: %s %s score=%.2f\n", w.SourceID, w.Span, w.Score)
		}
	}
	if ev, ok := context.Get(cDeep, qaflow.KeyEviction); ok {
		for _, m := range ev.Merged {
			fmt.Printf("evict: merged %s#%s into %s#%s (cosine=%.3f)\n",
				m.Merged.SourceID, m.Merged.Span, m.Kept.SourceID, m.Kept.Span, m.Cosine)
		}
		for _, d := range ev.Dropped {
			fmt.Printf("evict: dropped %s#%s (%s)\n", d.Window.SourceID, d.Window.Span, d.Reason)
		}
	}

	// —— 级联演示：快路够就用快的；不够才升级（BioHarness）——
	cEsc := context.New(context.Realm(*realm))
	if err := qaflow.Runner("连接池的连接数和部署端口分别是多少", qaflow.BM25Evidence(idx, 1, 160), synthFn, qaflow.Options{
		CorpusVersion: "selftest", ConfigVersion: "selftest", StrategyVersion: "v0.1", BeliefVersion: "none",
		Reuse: reuse, Session: "selftest",
		Escalate: escalateBackend,
	}).Run(cEsc); err != nil {
		return err
	}
	fmt.Println("--- cascade (fast + escalate)")
	if rd, ok := context.Get(cEsc, qaflow.KeyRoute); ok {
		fmt.Printf("route: %s — %s\n", rd.Action, rd.Reason)
	}
	if es, ok := context.Get(cEsc, qaflow.KeyEscalation); ok {
		fmt.Printf("escalate: triggered=%v executed=%v %s→%s\n", es.Triggered, es.Executed, es.Before, es.After)
	}
	if tel, ok := context.Get(cEsc, qaflow.KeyDeep); ok {
		fmt.Printf("escalation deep: rounds=%d coverage=%v stop=%s\n", tel.Rounds, tel.Coverage, tel.StopReason)
	}

	// 同一会话再问一次：复用命中，本轮不检索（"越问越快"的执行处）
	c2 := context.New(context.Realm(*realm))
	c2.RegisterComponent(&qaflow.SemanticRerank{})
	if err := qaflow.Runner("连接池最大连接数是多少", qaflow.BM25Evidence(idx, 3, 60), synthFn, qaflow.Options{
		CorpusVersion: "selftest", ConfigVersion: "selftest", StrategyVersion: "v0.1", BeliefVersion: "none",
		Reuse: reuse, Session: "selftest",
	}).Run(c2); err != nil {
		return err
	}
	printAsk("second ask (reuse)", c2)
	fmt.Printf("reuse store: %d entries after two asks\n", reuse.Len("selftest"))
	fmt.Println("selftest ok")
	return nil
}

// printAsk 打印一次问答的可观测事实：组件状态、复用决定、答案、提交
// 视图、窗口。两次提问共用一台打印机——并排看才有对比。
func printAsk(label string, c *context.Context) {
	fmt.Printf("--- %s\n", label)
	for _, s := range c.Components() {
		state := "inactive"
		if s.Active {
			state = "active"
		}
		line := fmt.Sprintf("component: %-16s %s", s.Name, state)
		if len(s.Missing) > 0 {
			line += fmt.Sprintf(" missing=%v", s.Missing)
		}
		if s.LastError != "" {
			line += fmt.Sprintf(" error=%s", s.LastError)
		}
		fmt.Println(line)
	}
	if rs, ok := context.Get(c, qaflow.KeyReuseState); ok {
		fmt.Printf("reuse: hit=%v %s\n", rs.Hit, rs.Reason)
	}
	if rd, ok := context.Get(c, qaflow.KeyRoute); ok {
		fmt.Printf("route: %s — %s\n", rd.Action, rd.Reason)
	}
	if es, ok := context.Get(c, qaflow.KeyEscalation); ok {
		fmt.Printf("escalate: triggered=%v executed=%v %s→%s %s\n", es.Triggered, es.Executed, es.Before, es.After, es.Reason)
	}
	if a, ok := context.Get(c, qaflow.KeyAnswer); ok {
		state := "answered"
		if a.Refused {
			state = "refused"
		}
		fmt.Printf("answer: %s text=%q citations=%v\n", state, a.Text, a.Citations)
	}
	for _, v := range c.Views() {
		fmt.Printf("committed: flow=%s realm=%s corpus=%s strategy=%s at=%s\n",
			v.Flow, v.Realm, v.CorpusVersion, v.StrategyVersion, v.At.Format("15:04:05"))
		// 路由档位与校准程序是**发生额**：跑完才知道，所以由 ViewHook 填。
		// 不打印它就没人会发现它没填（不变量 4 的执行处要看得见）。
		fmt.Printf("  calibration: tier=%s program=%s threshold=%.3f version=%s\n",
			v.Calibration.Tier, v.Calibration.Program, v.Calibration.Threshold, v.Calibration.ThresholdVersion)
	}
	if ws, ok := context.Get(c, qaflow.KeyWindows); ok {
		for _, w := range ws {
			d, _ := idxDoc(w.SourceID)
			text, err := retrieval.ResolveSpan(d, w.Span)
			if err != nil {
				fmt.Printf("window: %s score=%.2f span=%s (unresolvable: %v)\n", w.SourceID, w.Score, w.Span, err)
				continue
			}
			fmt.Printf("window: %s score=%.2f span=%s text=%q\n", w.SourceID, w.Score, w.Span, text)
		}
	}
}

// idxDoc 是 selftest 内联语料的查表（包级变量在 runSelftest 里建）。
var selftestIdx *retrieval.Index

func idxDoc(id string) (string, error) {
	if selftestIdx == nil {
		return "", fmt.Errorf("no index")
	}
	d, ok := selftestIdx.Doc(id)
	if !ok {
		return "", fmt.Errorf("unknown doc %s", id)
	}
	return d.Body, nil
}

// runServe 起 HTTP 面：语料从 store 装（空了就从 -corpus 导入），零件
// 按 flag 装配，/v1/qa 一次问答返回完整 committed view。
func pruneEvery() time.Duration {
	v := strings.TrimSpace(os.Getenv("CUMULUS_PRUNE_EVERY"))
	if v == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

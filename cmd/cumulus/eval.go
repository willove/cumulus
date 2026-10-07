package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/evaldata"
	"github.com/willove/cumulus/internal/evalfcore"
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
	idx           *retrieval.Index
	knobs         map[string]float64  // evidence.topk / evidence.width
	embedder      embed.Embedder      // 可空：向量面（绑了才重排/才亮语义尺）
	synthFn       qaflow.SynthFunc    // 合成面：offline 或 llm
	groundingFlag bool                // semantic grounding scale (true=on)
	deep          *qaflow.DeepOptions // 非空 = 首程就走深循环（多轮取证）
	escalate      bool                // true = 快路首程 + 判 escalate 才升级（级联）
}

// escalateBackend 是级联的贵路（深循环）。
func (e *bm25Executor) escalateBackend() func(*context.Context, qaflow.Rewrite) ([]qaflow.EvidenceWindow, error) {
	return qaflow.BM25DeepEvidence(e.idx, e.width(), qaflow.DeepOptions{MaxRounds: 3, CoverageTarget: 1.0})
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
func (e *bm25Executor) grounding() float64 {
	if e.groundingFlag {
		return defaultGroundingFloor
	}
	return 0
}

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
	if e.embedder != nil {
		if err := qaflow.BindEmbedder(c, e.embedder); err != nil {
			return evalfcore.ItemOutcome{}, err
		}
	}
	synthFn := e.synthFn
	if synthFn == nil {
		synthFn = e.synth()
	}
	retrieve := qaflow.BM25Evidence(e.idx, e.topk(), e.width())
	if e.deep != nil {
		retrieve = qaflow.BM25DeepEvidence(e.idx, e.width(), *e.deep)
	}
	var escalateFn func(*context.Context, qaflow.Rewrite) ([]qaflow.EvidenceWindow, error)
	if e.escalate {
		escalateFn = e.escalateBackend()
	}
	r := qaflow.Runner(question, retrieve, synthFn, qaflow.Options{
		CorpusVersion:   "frozen",
		ConfigVersion:   "eval",
		StrategyVersion: "v0.1",
		BeliefVersion:   "none",
		GroundingFloor:  e.grounding(),
		Escalate:        escalateFn,
	})
	if err := r.Run(c); err != nil {
		return evalfcore.ItemOutcome{}, fmt.Errorf("qaflow: %w", err)
	}
	route, _ := context.Get(c, qaflow.KeyRoute)
	answer, _ := context.Get(c, qaflow.KeyAnswer)
	windows, _ := context.Get(c, qaflow.KeyWindows)
	usage, _ := context.Get(c, qaflow.KeyUsage)

	rerank, _ := context.Get(c, qaflow.KeyRerank)
	out := evalfcore.ItemOutcome{
		Answer:           answer.Text,
		RerankApplied:    rerank.Applied,
		RerankReason:     rerank.Reason,
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

// defaultGroundingFloor 语义接地地板的默认值（可过 CUMULUS_GROUNDING 调）。
const defaultGroundingFloor = 0.5

// knobsFromEnv 用环境变量覆盖旋钮（实验入口：CUMULUS_TOPK /
// CUMULUS_WIDTH）。旋钮本来就该被实验驱动——覆盖只是把注册表的口
// 开到命令行，不新增旋钮语义。
func knobsFromEnv(knobs map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range knobs {
		out[k] = v
	}
	if v := os.Getenv("CUMULUS_TOPK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			out["evidence.topk"] = float64(n)
		}
	}
	if v := os.Getenv("CUMULUS_WIDTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			out["evidence.width"] = float64(n)
		}
	}
	return out
}

// defaultKnobs 默认旋钮。width=160 不是拍的：真实运行（cn-law-rag，
// MiniMax 真合成）发现 60 字宽把法条拦腰截断（“为了保护专利权人的合法权”
// 就断了），模型对碎片证据全部正确拒答——judge 一度只有 6%。法条平均
// 长度决定宽度下限；换语料要重测（这正是旋钮该干的事）。
func defaultKnobs() map[string]float64 {
	return map[string]float64{"evidence.topk": 3, "evidence.width": 160}
}

func runEval(ctx gocontext.Context) error {
	var (
		corpus    []retrieval.Document
		rawItems  []evalfcore.Item
		corpusSHA string
		itemsSHA  string
		err       error
	)
	corpus = evalCorpus()
	rawItems = evalItems()
	corpusSHA = "inline"
	itemsSHA = "inline"
	if os.Getenv("CUMULUS_REALDATA") == "cnlaw" {
		path := os.Getenv("CNLAW_DIR")
		if path == "" {
			path = filepath.Join(os.Getenv("HOME"), "datasets/cn-law-rag/finetune_dataset.jsonl")
		}
		sample := 300
		if v := os.Getenv("CUMULUS_SAMPLE"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &sample); err != nil {
				return fmt.Errorf("CUMULUS_SAMPLE not a number: %q", v)
			}
		}
		set, err := evaldata.LoadCNLaw(path, sample)
		if err != nil {
			return err
		}
		corpus, rawItems = set.Docs, set.Items
		corpusSHA, itemsSHA = set.CorpusSHA, set.ItemsSHA
		fmt.Printf("realdata: %s sample=%d corpus=%d docs\n", path, len(rawItems), len(corpus))
	}
	dset, err := evalfcore.NewDataset(rawItems)
	ds = dset
	if err != nil {
		return err
	}

	idx := retrieval.Build(corpus)
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
	embedFn, embedLabel, err := pickEmbed(os.Getenv("CUMULUS_EMBED"))
	if err != nil {
		return err
	}
	grounding := 0.0
	if v := os.Getenv("CUMULUS_GROUNDING"); v != "" {
		if _, err := fmt.Sscanf(v, "%f", &grounding); err != nil {
			return fmt.Errorf("CUMULUS_GROUNDING not a number: %q", v)
		}
	}
	fmt.Printf("synth: %s  judge: %s  embed: %s\n", synthLabel, judgeLabel(os.Getenv("CUMULUS_JUDGE")), embedLabel)
	fp := evalfcore.Fingerprints{
		ItemsSHA:  itemsSHA,
		CorpusSHA: corpusSHA,
		ConfigSHA: evalfcore.Config{Arms: []string{"rule"}, Model: "offline-stub"}.SHA(),
	}
	ab := os.Getenv("CUMULUS_AB") == "1"
	// execFor 组装执行面：embedder/信念都是可选件，由参数决定装不装
	execFor := func(withEmbed bool, knobs map[string]float64, deep *qaflow.DeepOptions, escalate bool) *bm25Executor {
		ex := &bm25Executor{idx: idx, knobs: knobs, synthFn: synthFn, groundingFlag: grounding > 0, deep: deep, escalate: escalate}
		if withEmbed && embedFn != nil {
			ex.embedder = embedFn()
		}
		return ex
	}
	runOne := func(runID, arm string, withEmbed bool, knobs map[string]float64, deep *qaflow.DeepOptions, escalate bool) (evalfcore.RunState, error) {
		runner := evalfcore.NewRunner(evalfcore.NewKVStore(st), fp, execFor(withEmbed, knobs, deep, escalate), j)
		state, err := runner.Start(ctx, runID, ds.Items)
		state.Arm = arm
		if err != nil {
			return state, err
		}
		return state, nil
	}
	fmt.Printf("dataset %s items=%d\n", ds.ID[:12], len(ds.Items))
	fmt.Printf("fingerprints items=%s corpus=%s config=%s\n", fp.ItemsSHA[:12], fp.CorpusSHA, fp.ConfigSHA[:12])

	if ab {
		// 三臂对照（同一指纹；臂是实验内维度，不进指纹）：
		//   bm25        k=3 单轮（基线）
		//   bm25-k9     k=9 单轮（预算对齐：与深循环的窗口上限同预算）
		//   deep        k=3 起步的深循环（覆盖度驱动多轮，预算同上）
		// 三者窗口预算一致才可比——不然"窗口多所以命中高"是预算差异不是
		// 部件差异。（belief 全局声望臂已退役；按会话复用由 selftest 覆盖。）
		deepOpts := &qaflow.DeepOptions{MaxRounds: 3, CoverageTarget: 1.0}
		k9 := knobsFromEnv(defaultKnobs())
		k9["evidence.topk"] = 9
		deepEnabled := os.Getenv("CUMULUS_DEEP") == "1"
		a, err := runOne("run-ab-bm25", "bm25", false, knobsFromEnv(defaultKnobs()), nil, false)
		if err != nil {
			return err
		}
		arms := []evalfcore.RunState{a}
		labels := []string{"bm25   "}
		if deepEnabled {
			k9Run, err := runOne("run-ab-k9", "bm25-k9", false, k9, nil, false)
			if err != nil {
				return err
			}
			deepRun, err := runOne("run-ab-deep", "deep", false, knobsFromEnv(defaultKnobs()), deepOpts, false)
			if err != nil {
				return err
			}
			cascRun, err := runOne("run-ab-cascade", "cascade", false, knobsFromEnv(defaultKnobs()), nil, true)
			if err != nil {
				return err
			}
			arms = append(arms, k9Run, deepRun, cascRun)
			labels = append(labels, "bm25-k9", "deep    ", "cascade")
		} else {
			b, err := runOne("run-ab-rerank", "bm25+rerank", true, defaultKnobs(), nil, false)
			if err != nil {
				return err
			}
			arms = append(arms, b)
			labels = append(labels, "bm25+rerank")
		}
		for i, s := range arms {
			printRun(labels[i], s)
		}
		for i := 1; i < len(arms); i++ {
			pair := struct {
				label string
				x, y  evalfcore.RunState
			}{labels[i] + "-bm25", arms[i], a}
			diff, reasons := evalfcore.Compare(pair.x, pair.y)
			if len(reasons) > 0 {
				fmt.Printf("diff %s: incomparable %v\n", pair.label, reasons)
				continue
			}
			lost, gained := flips(pair.y, pair.x)
			fmt.Printf("diff %-12s evidence=%+.3f citations=%+.3f latency=%+.0fms flips(lost/gained)=%d/%d\n",
				pair.label, diff["evidence_hit"], diff["citations_ok"], diff["avg_latency_ms"], lost, gained)
		}
		return nil
	}

	state, err := runOne("run-selftest", "default", os.Getenv("CUMULUS_EMBED") == "minilm", knobsFromEnv(defaultKnobs()), nil, false)
	if err != nil {
		return err
	}
	printRun("default      ", state)
	return nil
}

// flips 逐题对比基线与变体：lost = 基线命中而变体丢失，gained 反之。
// 总量持平但翻转不为零，说明部件在重新分配风险，不是单纯改善或恶化。
func flips(base, variant evalfcore.RunState) (lost, gained int) {
	bh := make(map[string]bool)
	for _, r := range base.Results {
		bh[r.ItemID] = r.EvidenceHit
	}
	for _, r := range variant.Results {
		if bh[r.ItemID] && !r.EvidenceHit {
			lost++
		}
		if !bh[r.ItemID] && r.EvidenceHit {
			gained++
		}
	}
	return lost, gained
}

var ds evalfcore.Dataset

func printRun(arm string, state evalfcore.RunState) {
	verbose := os.Getenv("CUMULUS_EVAL_VERBOSE") == "1"
	golds := map[string]string{}
	if len(ds.Items) > 0 {
		for _, it := range ds.Items {
			golds[it.ID] = it.Answer
		}
	}
	for _, r := range state.Results {
		if verbose {
			j := "N/A"
			if r.JudgeOK != nil {
				j = "yes"
				if !*r.JudgeOK {
					j = "NO"
				}
			}
			raw := ""
			if r.JudgeRaw != "" {
				raw = " raw=%q" + ""
				raw = fmt.Sprintf(raw, truncateRunes(r.JudgeRaw, 30))
			}
			fmt.Printf("    %s gold=%q answer=%q judge=%s%s\n", r.ItemID, truncateRunes(golds[r.ItemID], 40), truncateRunes(r.Answer, 60), j, raw)
		}
		f := "ok"
		if r.Failure != "" {
			f = r.Failure
		}
		rr := ""
		if r.RerankReason != "" {
			rr = " (" + r.RerankReason + ")"
		}
		fmt.Printf("  [%s] %s rule=%.0f evidence=%v cites=%d/%d rerank=%v%s failure=%s\n",
			arm, r.ItemID, r.RuleScore, r.EvidenceHit, r.CitationsResolved, r.CitationsTotal, r.RerankApplied, rr, f)
	}
	fmt.Printf("[%s] %s\n", arm, evalfcore.Summarize(state))
}

func evalCorpus() []retrieval.Document {
	return []retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
		{ID: "cost-a", Body: "成本结构与分摊方法：成本按部门分摊，成本结构按季度复盘，成本口径见附则。"},
		{ID: "cost-b", Body: "成本结构与定价：成本结构决定底线，成本结构变动需重新定价，成本归集周期一月。"},
		{ID: "cost-c", Body: "成本结构与预算：成本结构分解到项目，成本结构偏差超百分之五需说明，成本台账按月."},
	}
}

func evalItems() []evalfcore.Item {
	return []evalfcore.Item{
		{ID: "q1", Question: "连接池最大连接数是多少", Answer: "100", GoldIDs: []string{"law-1"}},
		{ID: "q2", Question: "默认端口是多少", Answer: "8484", GoldIDs: []string{"ops-1"}},
		{ID: "q3", Question: "成本结构怎么样", Answer: "优化", GoldIDs: []string{"fin-1"}},
	}
}

// evalCorpusNote: q3 是构造的真失败——三个干扰文档在 BM25 词频上压过
// 金标 fin-1（topk=3 时金标在窗外）。

func judgeLabel(which string) string {
	if which == "llm" {
		return "llm"
	}
	return "none (N/A)"
}

// truncateRunes 截断到 n 个字符（eval 明细打印用）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

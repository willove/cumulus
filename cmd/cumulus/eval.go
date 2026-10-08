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
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/failure"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/query"
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
	bare          bool                // true = 哑臂：零改写、零管理（v0.2 §三.7）
	route         qaflow.RouteConfig  // 路由阈值与校准来源
}

// envInt 读一个整数环境变量（未设置或非法 → ok=false，用默认值）。
// escalateBackend 是级联的贵路（深循环）。
func (e *bm25Executor) escalateBackend() func(*context.Context, qaflow.Rewrite) ([]qaflow.EvidenceWindow, error) {
	return qaflow.BM25DeepEvidence(e.idx, e.width(), qaflow.DefaultDeep())
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
	return func(_ string, ws []qaflow.EvidenceWindow, _ facts.Report) (qaflow.Answer, qaflow.Usage, error) {
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
	retrieve := e.retrievalFor()
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
		Bare:            e.bare,
		Route:           e.route,
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
		Confidence:       route.Signals.Confidence,
		RouteTier:        route.Signals.Tier,
		Coverage:         route.Signals.Coverage,
		Margin:           route.Signals.Margin,
	}
	// 带符号的词面支持：答案的内容词有多少落在窗口原文里（post-answer
	// 信号——只能在答案出来之后算，因此进不了路由，但进得了校准比较，
	// 以及之后"验证后再决定升级/拒答"的环节）。
	out.Support, out.SupportN = qaflow.AnswerSupport(answer.Text, windows)
	// 底物信号交给归因（不在评审器的签名里加东西，执行面顺手报）：
	// 窗里有没有数、查询的实体语料里有没有。
	texts := make([]string, 0, len(windows))
	for _, w := range windows {
		texts = append(texts, w.Text)
	}
	out.EvidenceHasNumeric = failure.HasNumeric(texts...)
	// 底物信号：库与问句**词面零共享**（内容词一个都不在语料里）。这里做
	// 一次离线判据、不写进 context——评测臂的检索口径保持纯 BM25，不因为
	// 多了一个信号就换底物。
	// 判据只认"零共享"，不认 OOV 占比：后者是词表桥的触发器（桥本来就
	// 该常开），当失败标签用会误标普通口语问句（真跑踩过）。
	out.QueryOutOfCorpus = query.NoSharedContent(question, e.idx)
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

// defaultKnobs 默认旋钮。**k9/w400 是实测定的，不是拍的**（DomainRAG 五类任务
// + cn-law，全部零 token 复现）：
//
//	页宽 3→9（找到更多）：multidoc evidence 27.1→70.8%、time_sensitive
//	  70.8→93.8%、faithful 81.6→91.8%；cn-law 74.0→88.3%。五个任务无一
//	  回退（basic/structured 同步小涨）。
//	窗宽 160→400（拿到能答的片段）：量的是"金标答案词项落在取回窗口原文里
//	  的比例"——faithful 46.9→65.3%、time_sensitive 78.5→89.2%、basic
//	  56.7→65.6%、structured 92.6→98.9%；端到端规则臂同向（time_sensitive
//	  61.5→73.8）。multidoc 提升有限——答案跨多文档，是任务性质不是切分 bug。
//
// 两个都是确定性旋钮，代价只是提示词变长（每题多几百 token），对个人知识库值。
// width 的下限仍由法条长度决定（60 字宽会把法条拦腰截断，模型对碎片证据正确
// 拒答——judge 曾只有 6%）；换语料要重测，这正是旋钮该干的事。
func defaultKnobs() map[string]float64 {
	return map[string]float64{"evidence.topk": 9, "evidence.width": 400}
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
	// 内联小样也给真指纹：占位串会在 [:12] 截断处炸（且"同样内容同指纹"
	// 的纪律对演示路径同样成立）。
	corpusSHA = evaldata.HashDocs(corpus)
	itemsSHA = evaldata.HashItems(rawItems)
	if os.Getenv("CUMULUS_REALDATA") == "cnlaw" {
		path := os.Getenv("CNLAW_DIR")
		if path == "" {
			path = filepath.Join(os.Getenv("HOME"), "datasets/cn-law-rag/finetune_dataset.jsonl")
		}
		// cnlaw 自带采样参数：这里给 -1（全量；0 是"取 0 条"），统一采样
		// 交给下面的公共块——两处采样会让"先切分再采样"的顺序无法保证。
		set, err := evaldata.LoadCNLaw(path, -1)
		if err != nil {
			return err
		}
		corpus, rawItems = set.Docs, set.Items
		corpusSHA, itemsSHA = set.CorpusSHA, set.ItemsSHA
		fmt.Printf("realdata: %s sample=%d corpus=%d docs\n", path, len(rawItems), len(corpus))
	}
	// local：任意来源裁剪好的小语料（ModelScope 拉的 CMRC、自裁领域语料、
	// 按块切的校准集）。目录里放 corpus.jsonl + items.jsonl，见
	// scripts/prep_cmrc.py；也可用 CUMULUS_LOCAL_CORPUS / CUMULUS_LOCAL_ITEMS
	// 分别指定两条文件。
	if os.Getenv("CUMULUS_REALDATA") == "local" {
		dir := os.Getenv("CUMULUS_LOCAL_DIR")
		if dir == "" {
			return fmt.Errorf("CUMULUS_REALDATA=local needs CUMULUS_LOCAL_DIR (dir with corpus.jsonl + items.jsonl)")
		}
		corpusPath, itemsPath := evaldata.DefaultLocalPaths(dir)
		if v := os.Getenv("CUMULUS_LOCAL_CORPUS"); v != "" {
			corpusPath = v
		}
		if v := os.Getenv("CUMULUS_LOCAL_ITEMS"); v != "" {
			itemsPath = v
		}
		set, warnings, err := evaldata.LoadJSONL(corpusPath, itemsPath)
		if err != nil {
			return err
		}
		corpus, rawItems = set.Docs, set.Items
		corpusSHA, itemsSHA = set.CorpusSHA, set.ItemsSHA
		fmt.Printf("local: corpus=%s items=%s docs=%d items=%d\n", corpusPath, itemsPath, len(corpus), len(rawItems))
		// 数据诊断必须看得见：金标不在语料里是"该拒答"的合法构造，但要
		// 显式选择，不能默默跑（否则把数据错当成检索失败）。
		for _, w := range warnings {
			fmt.Printf("  warn: %s\n", w)
		}
	}
	rawItems, err = applySplitAndSample(rawItems)
	if err != nil {
		return err
	}

	// 协议警告要看得见（超长金标、无金标 docid）：不拦运行，但会改变
	// 哪些指标可读——规则臂恒判 0 不是检索失败。
	for _, w := range evalfcore.WarnItems(rawItems) {
		fmt.Printf("  warn: %s\n", w)
	}

	dset, err := evalfcore.NewDataset(rawItems)
	ds = dset
	if err != nil {
		return err
	}

	idx := retrieval.Build(corpus)
	if c := coordFromEnv(); c > 0 {
		idx.Coord = c
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
	// 路由阈值与校准来源（v0.2 §2.1：阈值不手调）。
	// CUMULUS_TAU0=<数> 显式换到 CAUC 程序（τ₀ = 强臂校准准确率）；
	// CUMULUS_TAU0=auto 只打印建议值，不改本轮——应用它是下一轮的事，
	// 因为"强臂准确率"要跑完本轮才知道（不为了一个数把评测跑两遍）。
	routeCfg := qaflow.RouteConfig{}
	tau0Auto := false
	if v := os.Getenv("CUMULUS_TAU0"); v != "" {
		if v == "auto" {
			tau0Auto = true
		} else {
			tau, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("CUMULUS_TAU0 not a number or auto: %q", v)
			}
			if tau < 0 || tau > 1 {
				return fmt.Errorf("CUMULUS_TAU0 out of [0,1]: %v", tau)
			}
			routeCfg = qaflow.RouteConfig{
				UpgradeBase:      qaflow.Tau0FromDeepAccuracy(tau),
				Program:          qaflow.ProgramCAUC,
				ThresholdVersion: fmt.Sprintf("tau0=%.3f", tau),
			}
		}
	}
	// execSpec 是一次执行面的装配参数（臂注册用）。
	type execSpec struct {
		withEmbed bool
		knobs     map[string]float64
		deep      *qaflow.DeepOptions
		escalate  bool
		bare      bool
	}
	execFor := func(sp execSpec) *bm25Executor {
		ex := &bm25Executor{idx: idx, knobs: sp.knobs, synthFn: synthFn, groundingFlag: grounding > 0,
			deep: sp.deep, escalate: sp.escalate, bare: sp.bare, route: routeCfg}
		if sp.withEmbed && embedFn != nil {
			ex.embedder = embedFn()
		}
		return ex
	}
	runOne := func(runID, arm string, sp execSpec) (evalfcore.RunState, error) {
		runner := evalfcore.NewRunner(evalfcore.NewArchive(st), fp, execFor(sp), j)
		state, err := runner.Start(ctx, runID, ds.Items)
		state.Arm = arm
		if err != nil {
			return state, err
		}
		return state, nil
	}
	fmt.Printf("dataset %s items=%d\n", shortSHA(ds.ID), len(ds.Items))
	fmt.Printf("fingerprints items=%s corpus=%s config=%s\n", shortSHA(fp.ItemsSHA), shortSHA(fp.CorpusSHA), shortSHA(fp.ConfigSHA))
	effTau := routeCfg.UpgradeBase
	if effTau <= 0 {
		effTau = 0.5 // RouteStage 的同款默认（零值 = 手工基线）
	}
	fmt.Printf("route: program=%s tau0=%.3f\n", routeCfg.ProgramName(), effTau)
	if tau0Auto {
		fmt.Println("route: CUMULUS_TAU0=auto —— 本轮不改阈值，建议值在评测末尾给出")
	}

	if ab {
		// 对照组臂注册（v0.2 §三.3/§三.7）：臂是可挂卸的注册，且**必须
		// 含一个哑臂**（纯 BM25 top-3、零改写、零管理）——没有它，
		// "全管线命中多少"无法归因：是管线在增值，还是在补自己造的洞。
		//    bm25-bare  哑基线（哑臂，Dumb=true）
		//    bm25       全管线快路 k=3
		//    bm25-k9    k=9 单轮（预算对齐：与深循环窗口上限同预算）
		//    deep       k=3 起步的深循环（覆盖度驱动多轮，预算同上）
		//    cascade    快路 + 判 escalate 才升级（级联）
		// 除哑臂外窗口预算一致才可比——不然"窗口多所以命中高"是预算差异
		// 不是部件差异。（belief 全局声望臂已退役；按会话复用由 selftest 覆盖。）
		deepOpts := deepFromEnv()
		k9 := knobsFromEnv(defaultKnobs())
		k9["evidence.topk"] = 9
		deepEnabled := os.Getenv("CUMULUS_DEEP") == "1"
		armRun := func(runID string, sp execSpec) func(gocontext.Context) (evalfcore.RunState, error) {
			return func(gocontext.Context) (evalfcore.RunState, error) { return runOne(runID, runID, sp) }
		}
		arms := []evalfcore.Arm{
			{ID: "bm25-bare", Dumb: true, Note: "纯 BM25 top-3、零改写、零管理（哑基线）",
				Run: armRun("run-ab-bm25-bare", execSpec{knobs: knobsFromEnv(defaultKnobs()), bare: true})},
			{ID: "bm25", Note: "全管线快路 k=3",
				Run: armRun("run-ab-bm25", execSpec{knobs: knobsFromEnv(defaultKnobs())})},
		}
		if deepEnabled {
			arms = append(arms,
				evalfcore.Arm{ID: "bm25-k9", Note: "k=9 单轮（预算对齐）",
					Run: armRun("run-ab-k9", execSpec{knobs: k9})},
				evalfcore.Arm{ID: "deep", Note: "覆盖度驱动深循环",
					Run: armRun("run-ab-deep", execSpec{knobs: knobsFromEnv(defaultKnobs()), deep: &deepOpts})},
				evalfcore.Arm{ID: "cascade", Note: "快路 + escalate 才升级",
					Run: armRun("run-ab-cascade", execSpec{knobs: knobsFromEnv(defaultKnobs()), escalate: true})},
			)
		} else {
			arms = append(arms, evalfcore.Arm{ID: "bm25+rerank", Note: "语义重排（需 embedder）",
				Run: armRun("run-ab-rerank", execSpec{withEmbed: true, knobs: defaultKnobs()})})
		}
		// 丢掉 Run 的闭包再交给注册表：注册表只认 ID/Dumb/Note/Run，
		// 便于测试直接构造（ValidateArms 的判据不依赖真实执行面）。
		for _, a := range arms {
			fmt.Printf("arm %-12s dumb=%-5v %s\n", a.ID, a.Dumb, a.Note)
		}
		states, err := evalfcore.RunArms(ctx, arms)
		if err != nil {
			return err
		}
		var base evalfcore.RunState
		for i, s := range states {
			printRun(arms[i].ID, s)
			if arms[i].Dumb {
				base = s
			}
		}
		// 差值与哑臂比：这才是"管线有没有增值"的正确问法
		for i, s := range states {
			if arms[i].Dumb {
				continue
			}
			diff, reasons := evalfcore.Compare(s, base)
			label := arms[i].ID + "-bm25-bare"
			if len(reasons) > 0 {
				fmt.Printf("diff %s: incomparable %v\n", label, reasons)
				continue
			}
			lost, gained := flips(base, s)
			fmt.Printf("diff %-20s evidence=%+.3f citations=%+.3f latency=%+.0fms flips(lost/gained)=%d/%d\n",
				label, diff["evidence_hit"], diff["citations_ok"], diff["avg_latency_ms"], lost, gained)
		}
		// 校准读数（v0.2 §2.1）：跑最强的臂，报 τ₀ 候选与分桶可靠性。
		// 单调性不成立时不许把置信度当风险代理——这里明说。
		strongest := states[0]
		for i, s := range states {
			if arms[i].ID == "deep" || len(states) == 1 {
				strongest = s
			}
		}
		fmt.Println(evalfcore.CalibrationReport(strongest.Arm, strongest))
		printCalib(strongest)
		printLockbox(strongest)
		printCalibContest(strongest, alphaFromEnv())
		if tau0Auto {
			tau, oracle := evalfcore.SuggestedTau0(strongest)
			fmt.Printf("tau0 suggestion: CUMULUS_TAU0=%.3f (from %s, next run applies it)\n", tau, oracle)
		}
		return nil
	}

	state, err := runOne("run-selftest", "default", execSpec{withEmbed: os.Getenv("CUMULUS_EMBED") == "minilm", knobs: knobsFromEnv(defaultKnobs())})
	if err != nil {
		return err
	}
	printRun("default      ", state)
	// 校准读数也可单臂看（CUMULUS_CALIB=1），但 τ₀ 的**正确取法是强臂**：
	// CAUC 的 τ₀ = 强模型（这里对应 DEEP 臂）的校准准确率——单臂跑出来的
	// 是当前这一臂的读数，只能当参考，不能当 τ₀ 直接套。
	if tau0Auto || os.Getenv("CUMULUS_CALIB") == "1" {
		fmt.Println(evalfcore.CalibrationReport(state.Arm, state))
		printCalib(state)
		printLockbox(state)
		printCalibContest(state, alphaFromEnv())
	}
	if tau0Auto {
		tau, oracle := evalfcore.SuggestedTau0(state)
		fmt.Printf("tau0 suggestion: CUMULUS_TAU0=%.3f（来自本臂 %s/%s）——CAUC 的 τ₀ 应取**强臂**读数：跑 CUMULUS_AB=1 CUMULUS_DEEP=1 取 deep 臂那一行\n",
			tau, state.Arm, oracle)
	}
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
	switch which {
	case "llm":
		return "llm(等义)"
	case "points":
		return "points(分点覆盖)"
	default:
		return "none (N/A)"
	}
}

// truncateRunes 截断到 n 个字符（eval 明细打印用）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// shortSHA 安全截断指纹（12 位）。指纹长度随来源不同（内容寻址 16 位、
// 演示用的短串），硬切 [:12] 会在短串上 panic——真跑踩过。
func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

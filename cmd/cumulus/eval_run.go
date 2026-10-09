package main

import (
	gocontext "context"
	"fmt"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
	"os"
	"strconv"
)

// eval_run.go —— 评测的**执行面**（跑多臂、读环境旋钮、打印读数）。
//
// （拆文件的理由：eval.go 原本同时装"执行器装配"与"跑与读数"两件事——前者是被测
// 代码的接线，后者是实验编排。混在一起时，改一个旋钮要确认它有没有误改执行器。）

func runEval(ctx gocontext.Context) error {
	var (
		corpus    []retrieval.Document
		rawItems  []evalfcore.Item
		corpusSHA string
		itemsSHA  string
		err       error
	)
	corpus, rawItems, corpusSHA, itemsSHA, err = resolveDataset()
	if err != nil {
		return err
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
	// 存储：默认内存（隔离、跑完即弃）；给 CUMULUS_STORE_DIR 就落盘——
	// **研究工作流需要它**（跑一次、反复分析；标注批次也从档案里出）。
	st, err := store.Open(os.Getenv("CUMULUS_STORE_DIR"), os.Getenv("CUMULUS_STORE_DIR") == "")
	if err != nil {
		return err
	}
	synthFn, streamFn, synthLabel, err := pickSynth(os.Getenv("CUMULUS_SYNTH"))
	// 桥的两个消融开关（量的用途，不是产品配置）：
	//   CUMULUS_BRIDGE=0     关桥（退回朴素贵路）
	//   CUMULUS_BRIDGE_GUARD=0 关护栏（采信桥，不管它把检索带偏）
	// 三臂对照才能说清"护栏有没有用"：关桥 / 开桥无护栏 / 开桥带护栏。
	// 桥要用 LLM；没有配置就缺席（缺席是合法状态：鸿沟时退化朴素贵路，遥测可见）
	var llmForBridge *llm.OpenAICompleter
	if os.Getenv("CUMULUS_BRIDGE") != "0" {
		if c, err := llmFromEnvImpl(); err == nil {
			llmForBridge = c
		} else {
			fmt.Printf("bridge: 无 LLM，桥缺席（%v）\n", err)
		}
	}
	// 桥的装配状态要看得见（真跑教训：四轮对照全读"未走桥"，第一反应是语料/
	// 开关错了，实际是**桥压根没装**而提示只在出错时打）
	if llmForBridge != nil {
		fmt.Printf("bridge: 已装配（model=%s）\n", llmForBridge.Model)
	} else if os.Getenv("CUMULUS_BRIDGE") == "0" {
		fmt.Println("bridge: 已关（CUMULUS_BRIDGE=0）")
	} else {
		fmt.Println("bridge: 缺席（无 LLM 配置）")
	}
	// 评测侧目前不发事件流（它要的是逐题指标，不是帧序列），但流式能力
	// 仍显式接上：**能力声明在接线处，评测要不要用是另一件事**。v1 传 nil
	// 给 executor，合成照旧走整条（答案与流式路径逐字段一致，见 synth 流式的纪律 3）。
	_ = streamFn
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
		decide    bool
		gate      bool
	}
	execFor := func(sp execSpec) *bm25Executor {
		ex := &bm25Executor{idx: idx, knobs: sp.knobs, synthFn: synthFn, groundingFlag: grounding > 0,
			deep: sp.deep, escalate: sp.escalate, bare: sp.bare, decide: sp.decide, route: routeCfg}
		if sp.gate {
			ex.gate = qaflow.DecideFromEnv("answerable")
		}
		bindBridge(ex, idx, llmForBridge, sp.knobs)
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
				Run: armRun("run-ab-bm25-bare", execSpec{knobs: knobsFromEnv(defaultKnobs()), bare: true, decide: os.Getenv("CUMULUS_DECIDE") == "1", gate: os.Getenv("CUMULUS_GATE") == "1"})},
			{ID: "bm25", Note: "全管线快路 k=3",
				Run: armRun("run-ab-bm25", execSpec{knobs: knobsFromEnv(defaultKnobs()), decide: os.Getenv("CUMULUS_DECIDE") == "1", gate: os.Getenv("CUMULUS_GATE") == "1"})},
		}
		if deepEnabled {
			arms = append(arms,
				evalfcore.Arm{ID: "bm25-k9", Note: "k=9 单轮（预算对齐）",
					Run: armRun("run-ab-k9", execSpec{knobs: k9})},
				evalfcore.Arm{ID: "deep", Note: "覆盖度驱动深循环",
					Run: armRun("run-ab-deep", execSpec{knobs: knobsFromEnv(defaultKnobs()), deep: &deepOpts, decide: os.Getenv("CUMULUS_DECIDE") == "1", gate: os.Getenv("CUMULUS_GATE") == "1"})},
				evalfcore.Arm{ID: "cascade", Note: "快路 + escalate 才升级",
					Run: armRun("run-ab-cascade", execSpec{knobs: knobsFromEnv(defaultKnobs()), escalate: true, decide: os.Getenv("CUMULUS_DECIDE") == "1", gate: os.Getenv("CUMULUS_GATE") == "1"})},
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

	state, err := runOne("run-selftest", "default", execSpec{withEmbed: os.Getenv("CUMULUS_EMBED") == "minilm", knobs: knobsFromEnv(defaultKnobs()), decide: os.Getenv("CUMULUS_DECIDE") == "1", gate: os.Getenv("CUMULUS_GATE") == "1"})
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
	fmt.Printf("[%s] %s %s %s %s\n", arm, evalfcore.Summarize(state), gateStats(state), shouldRetrieveStats(state), bridgeStats(state))
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

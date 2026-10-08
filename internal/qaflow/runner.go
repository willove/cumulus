package qaflow

// 本文件是问答流程的**装配面**：stage 列表怎么组、可选件按名字插在
// 哪里、提交视图记什么，以及哑臂（零改写零管理）的另一个装法。
// stage 各自的契约在 qa.go 与同目录各 stage 文件里——装配与契约分开，
// 这一层的改动才不牵连 stage 语义（file-size 门的本意：按职责拆）。

import (
	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/ctxmgmt"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/flow"
	"github.com/willove/cumulus/internal/harness"
	"github.com/willove/cumulus/internal/knowledge"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
)

// Runner 组装问答流程的 stage。学习步按开关取舍：不启用就整个不进流程，
// 不留空壳注册（可选组件的启用必须是显式的）。
func Runner(query string, retrieve func(*context.Context, Rewrite) ([]EvidenceWindow, error), synth SynthFunc, opts Options) *flow.Runner {
	if opts.Bare {
		return bareRunner(query, retrieve, synth, opts)
	}
	stages := []flow.Stage{
		RewriteStage{Query: query, Hypothetical: opts.Hypothetical, Idx: opts.RewriteIdx, Analyze: opts.Analyzer, Prior: opts.Prior},
		EvidenceStage{Retrieve: retrieve},
		FactsStage{Scorer: opts.FactScorer}, // 事实覆盖 + 一致性门（恒注册：这是"答得全不全"的
		// 判据，不是可选件；注不注册由上面的 list 说话，不设空壳）
		EvictStage{Budget: opts.CtxBudget.WithDefaults()},
		RouteStage{Config: opts.Route},
		EscalateStage{Retrieve: opts.Escalate, Expand: opts.Expander, Analyze: opts.Analyzer, Weighted: opts.WeightedRetrieve},
		SynthesizeStage{Query: query, Synth: synth, GroundingFloor: opts.GroundingFloor, Stream: opts.StreamSynth, Emit: opts.Emitter, RunID: opts.RunID},
		AccountStage{},
	}
	// 第二次事实判定：escalate 在驱逐**之后**又换过一轮窗，第一次判定
	// （驱逐前）的 supports 指向的窗可能已被替换。合成前按**最终窗集**再
	// 判一次——报告与合成面看到的事实一致（否则响应里"盖"了，合成看到
	// 的窗集里却没有那个窗）。
	rerun := make([]flow.Stage, 0, len(stages)+1)
	for _, st := range stages {
		rerun = append(rerun, st)
		if st.Name() == (EscalateStage{}).Name() {
			// 插**同一个** FactsStage 实例（带判官）——空壳会把 Scorer 丢
			// 掉：二次判定的存在意义就是按最终窗集重算，没判官的二次判
			// 定会把第一次救回的事实又打回未盖（真跑教训）
			rerun = append(rerun, FactsStage{Scorer: opts.FactScorer})
		}
	}
	stages = rerun
	// 合成前闸门只在**绑了决策面**时注册（可选件的启用必须是显式的，
	// 见文件头"不留空壳注册"）。缺席时 stage 列表逐字段不变。
	if opts.Decision != nil {
		withGate := make([]flow.Stage, 0, len(stages)+1)
		for _, st := range stages {
			if st.Name() == (RouteStage{}).Name() {
				withGate = append(withGate, GateStage{Decision: opts.Decision, Query: query})
			}
			withGate = append(withGate, st)
		}
		stages = withGate
	}
	if opts.Abstain != nil {
		withAbstain := make([]flow.Stage, 0, len(stages)+1)
		for _, st := range stages {
			withAbstain = append(withAbstain, st)
			if st.Name() == (RouteStage{}).Name() {
				withAbstain = append(withAbstain, AbstainStage{Head: opts.Abstain})
			}
		}
		stages = withAbstain
	}
	if opts.Reuse != nil {
		// 复用查（evidence 前）与复用记（account 后）成对出现：
		// 只查不记，第二次永远冷；只记不查，记了白记。
		// **按名字定位插入，不用下标**：这个列表吃过下标硬编码的亏——
		// Evict/Escalate 插入后按旧下标重建，合成与记账两个 stage 被
		// 静默丢掉（配了复用的流程全在无声跳过合成），HTTP 面首测才
		// 暴露。按名字插，以后再加 stage 也不会错位。
		expanded := make([]flow.Stage, 0, len(stages)+1)
		for _, st := range stages {
			if st.Name() == (EvidenceStage{}).Name() {
				expanded = append(expanded, ReuseStage{Session: opts.Session, Store: opts.Reuse, Query: query})
			}
			expanded = append(expanded, st)
		}
		stages = expanded
	}
	if opts.LearnEnabled {
		stages = append(stages, LearnStage{Enabled: true})
	}
	if opts.Abstain != nil {
		withAbstain := make([]flow.Stage, 0, len(stages)+1)
		for _, st := range stages {
			withAbstain = append(withAbstain, st)
			if st.Name() == (RouteStage{}).Name() {
				withAbstain = append(withAbstain, AbstainStage{Head: opts.Abstain})
			}
		}
		stages = withAbstain
	}
	if opts.Reuse != nil {
		stages = append(stages, ReuseRecordStage{Session: opts.Session, Store: opts.Reuse, Query: query})
	}
	r := &flow.Runner{
		Flow:   "qa",
		Stages: stages,
		View: context.CommittedView{
			CorpusVersion:   opts.CorpusVersion,
			ConfigVersion:   opts.ConfigVersion,
			StrategyVersion: opts.StrategyVersion,
			BeliefVersion:   opts.BeliefVersion,
		},
		ViewHook: calibrationHook,
	}
	// 观测闭包：进度分母是**本次实际注册的阶段数**（装了哪些可选件，
	// 步数就不一样）——写死一个常数会让进度条说谎。
	r.Trace = traceFunc(opts.Emitter, opts.RunID, len(stages))
	return r
}

// calibrationHook 把路由实际生效的档位、校准程序与阈值填进提交视图。
// 在 Commit 之前由 flow.Runner 调用：视图记的是**发生额**（本轮真的
// 用了哪个档、哪条线），不是调用方的声明值——只有 stage 跑完才存在。
func calibrationHook(c *context.Context, v *context.CommittedView) {
	d, ok := context.Get(c, KeyRoute)
	if !ok {
		return
	}
	v.Calibration = context.Calibration{
		Tier:             d.Signals.Tier,
		Program:          d.Signals.CalibrationProgram,
		Threshold:        d.Signals.Threshold,
		ThresholdVersion: d.Signals.ThresholdVersion,
	}
}

// bareRunner 组装**哑臂**（v0.2 §三.7 的"最笨基线"）：意图澄清（不注入
// 假设、不做查询分析）→ 证据供给（纯 BM25）→ 路由 → 合成 → 记账。
// 事实分解与上下文驱逐全不装——"精致"的部分一件不留。
//
// 为什么要有它：精致臂不赢哑臂不许上线。"全管线 51.7% 对纯 BM25 85%"
// 那类数字，没有真哑臂就没法归因：是管线在增值，还是在添乱？哑臂把
// 这个问题变成可测的差。
//
// Bare 时下列开关被**忽略**（哑臂的定义就是不用它们）：Hypothetical /
// RewriteIdx / Analyzer / Prior / FactScorer / CtxBudget / Escalate /
// Abstain / Reuse / LearnEnabled。这不是静默降级——Bare 本身就是显式
// 放弃这些件，提交视图的 StrategyVersion 带 "+bare" 后缀，响应里看得到。
func bareRunner(query string, retrieve func(*context.Context, Rewrite) ([]EvidenceWindow, error), synth SynthFunc, opts Options) *flow.Runner {
	return &flow.Runner{
		Flow: "qa-bare",
		Stages: []flow.Stage{
			RewriteStage{Query: query},
			EvidenceStage{Retrieve: retrieve},
			RouteStage{Config: opts.Route},
			SynthesizeStage{Query: query, Synth: synth, GroundingFloor: opts.GroundingFloor, Stream: opts.StreamSynth, Emit: opts.Emitter, RunID: opts.RunID},
			AccountStage{},
		},
		View: context.CommittedView{
			CorpusVersion:   opts.CorpusVersion,
			ConfigVersion:   opts.ConfigVersion,
			StrategyVersion: opts.StrategyVersion + "+bare",
			BeliefVersion:   opts.BeliefVersion,
		},
		ViewHook: calibrationHook,
	}
}

// Options 是问答流程的环境版本与开关。四版本进提交视图，
// 缺一项就不可比（流程文法 §五.4）。
type Options struct {
	CorpusVersion   string
	ConfigVersion   string
	StrategyVersion string
	BeliefVersion   string
	LearnEnabled    bool
	// GroundingFloor 语义接地地板：>0 且 embedder 绑定时启用语义尺。
	GroundingFloor float64
	// Reuse 会话复用件：非 nil 时流程在 evidence 前查复用、account 后记录。
	// 同一问题（归一化）再问直接取上轮窗口——"同类问题越问越快"的执行处。
	Reuse   *knowledge.ReuseStore
	Session string
	// Hypothetical 注入改写文本（HyDE 式；空 = 不改写）。漂移闸审核它。
	Hypothetical string
	// RewriteIdx 漂移闸用的索引（判语料内/外词）；nil = 闸不启动。
	RewriteIdx *retrieval.Index
	// Analyzer 查询分析（IDF 加权关键词级）；nil = 全词等权（旧路径）。
	Analyzer func(q string) query.Analysis
	// Expander 词汇鸿沟桥（LLM 关键词扩展）；nil = 鸿沟时不扩展，照原
	// 查询升级检索。
	Expander query.Expander
	// WeightedRetrieve 按词权取数（鸿沟扩展后的加权重取）；nil = 扩展
	// 无执行处，退化普通贵路。
	WeightedRetrieve func(weights map[string]float64) ([]EvidenceWindow, error)
	// Abstain 零 LLM 失败预测头（早弃权/强升级）；nil = 不启用。
	Abstain *abstain.Head
	// Prior 开文档级多信号重排（cumulus prior 移植：lexical 无长度归一
	// + 标题 + 条文结构）。治 BM25 的短文档偏爱——答案在长法律里被短
	// 解释压住的那类。cumulus 同款 opt-in（UsePrior），默认关，验完再
	// 定去留。
	// FactScorer 事实覆盖的模型判官（词面判据的兜底升级：认不出改写的
	// 那类未盖事实让模型判一次）。nil = 只有词面判据。
	FactScorer facts.Scorer
	Prior      bool
	// Escalate 是升级（FAST→DEEP）的贵路取数函数：路由判 escalate 时
	// 跑它再判一次（BioHarness 级联）。nil = 升级无执行处（死标签，
	// 遥测里可见）。
	Escalate func(*context.Context, Rewrite) ([]EvidenceWindow, error)
	// Decision 是**合成前闸门**（§三·八）：问"这些窗口里到底有没有答案"，
	// 没有就不合成、改为拒答。nil = 这一层没接决策面（**合法状态**，
	// 此时行为与不启用闸门逐字段相同——TestGateAbsentIsNoop 钉死）。
	//
	// 为什么放合成前而不是合成后：事后判分时答案已经出笼；而"窗口里根本
	// 没有答案却合出了很像样的答案"（真跑见过：9 条窗口全讲潜伏期长短，
	// 答案写的是抗病毒治疗建议）只能在合成前拦。
	Decision *DecisionDecider
	// Emitter 是**对外事件流**（§architecture 输出面）。nil = 不发任何事件，
	// 流程与响应逐字段不变（harness 契约 1：观测面缺席不许改变行为）。
	Emitter *harness.Emitter
	// RunID 进每一帧事件（外部按它对账/回放）。空 = 不带（一次性问答无编号）。
	RunID string
	// StreamSynth 是**可选**的流式合成能力（能力在接线处声明，见 StreamSynthFunc
	// 的注释：方法值会把方法丢掉，断言找不回来）。nil = 整条路径。
	StreamSynth StreamSynthFunc
	// CtxBudget 合成前的上下文预算（按源配额/语义近重合并/窗口预算）。
	// 零值 = 默认预算（MaxWindows 8 / PerSource 2 / Dedup 0.92——
	// 0.92 是 Volt 论文的合并阈值，不是我们拍的）。
	CtxBudget ctxmgmt.Budget
	// Route 是路由的阈值与校准来源。零值 = 手工基线（0.5 + 每事实 0.05）；
	// 换 CAUC 程序时由调用方给 τ₀（见 qaflow.Tau0FromDeepAccuracy）。
	Route RouteConfig
	// Bare 是哑臂模式（v0.2 §三.7）：零改写、零管理，只留 证据→路由→
	// 合成→记账。评测的哑对照臂用它；Bare 时上列可选件一律被忽略
	// （StrategyVersion 自动带 "+bare" 后缀，忽略是可见的）。
	Bare bool
}

// traceFunc 返回挂到 flow.Runner 上的观测钩子。emitter 为 nil 时返回 nil
// （flow 那边 nil = 不观测）——**可选面缺席不留空壳**（见文件头约定）。
func traceFunc(em *harness.Emitter, runID string, total int) flow.TraceFunc {
	if em == nil {
		return nil
	}
	return NewTrace(em, runID, total).Stage
}

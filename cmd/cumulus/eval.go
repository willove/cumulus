package main

import (
	"fmt"
	"os"
	"strconv"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/failure"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
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
	knobs         map[string]float64      // evidence.topk / evidence.width
	embedder      embed.Embedder          // 可空：向量面（绑了才重排/才亮语义尺）
	synthFn       qaflow.SynthFunc        // 合成面：offline 或 llm
	groundingFlag bool                    // semantic grounding scale (true=on)
	deep          *qaflow.DeepOptions     // 非空 = 首程就走深循环（多轮取证）
	escalate      bool                    // true = 快路首程 + 判 escalate 才升级（级联）
	bare          bool                    // true = 哑臂：零改写、零管理（v0.2 §三.7）
	decide        bool                    // CUMULUS_DECIDE=1：合成后跑答案级验证
	gate          *qaflow.DecisionDecider // 非 nil：接合成前闸门（CUMULUS_GATE=1）
	route         qaflow.RouteConfig      // 路由阈值与校准来源
	// 桥与查询分析（评测要能单独量桥的收益/代价，所以三臂都能跑）
	bridge           bool                                                                        // 接 LLM 词汇桥（CUMULUS_BRIDGE=0 消融）
	analyzer         func(string) query.Analysis                                                 // 查询分析（桥的触发判据）
	expander         query.Expander                                                              // 词汇桥
	weightedRetrieve func(*context.Context, map[string]float64) ([]qaflow.EvidenceWindow, error) // 加权重取
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

func (e *bm25Executor) Answer(ctx gocontext.Context, question string) (evalfcore.ItemOutcome, error) {
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
	// 升级执行处：**单臂也要装**。
	//
	// 真跑踩到的坑：桥住在 escalateRetrieve 里（桥要靠贵路这一层上场），而
	// escalateFn 原来只在级联臂装配 —— 于是单臂"升级触发了 10 次、桥一次没走"。
	// 桥的接线与"要不要测级联"是两件事，混在一起时前者被后者悄悄关掉了。
	var escalateFn func(*context.Context, qaflow.Rewrite) ([]qaflow.EvidenceWindow, error)
	if e.escalate {
		escalateFn = e.escalateBackend()
	} else {
		// 默认贵路：深循环（同 serve 的"升级贵路无条件装配"口径）
		escalateFn = qaflow.BM25DeepEvidence(e.idx, e.width(), qaflow.DefaultDeep())
	}
	r := qaflow.Runner(question, retrieve, synthFn, qaflow.Options{
		CorpusVersion:    "frozen",
		ConfigVersion:    "eval",
		StrategyVersion:  "v0.1",
		BeliefVersion:    "none",
		GroundingFloor:   e.grounding(),
		Escalate:         escalateFn,
		Decision:         e.gate,
		Analyzer:         e.analyzer,
		Expander:         e.expander,
		WeightedRetrieve: e.weightedRetrieve,
		Bare:             e.bare,
		Route:            e.route,
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
	// 词汇桥结局进逐题结果（桥失手是质量问题，读数里要看得见）
	if esc, ok := context.Get(c, qaflow.KeyEscalation); ok {
		out.Bridge = esc.Bridge
		if esc.Triggered && out.Bridge == "" {
			// 触发了升级却没有桥结局 = 桥没上场（可观测：否则读数只说"未走桥"，
			// 说不清是没触发、还是触发了但没条件）
			out.Bridge = "no-bridge-on-escalate"
		}
	}
	// 闸门/决策留痕进每题输出："有没有真决策"必须看得见（没记录与"决策说不行"
	// 在读数里是两回事）。
	if rec, ok := qaflow.DecisionRecordOf(c); ok {
		out.DecisionApplied, out.DecisionReason, out.DecisionNoul = rec.Applied, rec.Reason, rec.Noul
		out.DecisionKind = rec.Kind
		if reason, ok := context.Get(c, qaflow.KeyRefusalReason); ok && reason == "decision-gate" {
			out.GateBlocked = true
		}
	}
	// 决策模型的答案级验证（CUMULUS_DECIDE=1）：合成之后问"答案有没有依据"。
	// 与检索侧信号正交，且与合成器不同家族——本项目唯一可作独立评估的候选。
	if e.decide {
		if vc, err := decideFromEnv(); err == nil {
			noul, choice, conf, reason := verifyWithDecision(vc).Verify(ctx, question, windows, answer.Text)
			if reason != "" {
				fmt.Printf("  decide: %s\n", reason) // 失败如实留痕，不静默当 0
			} else {
				out.VerifyNoul, out.VerifyChoice, out.VerifyConf = noul, choice, conf
			}
		} else {
			fmt.Printf("  decide: %v（不验，信号留空）\n", err)
		}
	}
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

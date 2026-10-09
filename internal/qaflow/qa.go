// Package qaflow 是流程一：一次问答的六个 stage（docs/flow-grammar.md §二）。
//
// stage 划分、契约、验证都是执行处：检索（BM25 + 可选语义重排）、路由
// （便宜信号）、合成（SynthFunc 契约，离线/LLM 两实现）、记账（发生额
// 同一来源）、学习（可选步）。lexical 与语义两把接地尺都在 Verify。
package qaflow

// qa.go —— 问答链的**共享词汇**（挂点、窗口、答案、用量、合成面契约）。
//
// stage 实现各自成文件（stage_*.go），这里不放流程步骤：改一个 step 不该与改另一个
// step 撞在同一处，也文法（flow-grammar §三）与源码无法一一对应。

import (
	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/prior"
	"github.com/willove/cumulus/internal/query"
)

// 本包登记的 key。命名规则 "<域>.<名>"，全进程唯一。
var (
	KeyRewrite = context.NewKey[Rewrite]("qa.rewrite")
	// KeyAnalysis 是查询分析的挂点：意图 + IDF 加权主关键词级 + 语料外
	// 词（词汇鸿沟信号）。覆盖度、加权检索、鸿沟扩展共用这一份事实。
	KeyAnalysis = context.NewKey[query.Analysis]("qa.query.analysis")
	// KeyPriorOn 是文档级多信号重排的开关（Options.Prior 的传播）。
	KeyPriorOn = context.NewKey[bool]("qa.prior.on")
	// KeyPrior 是 prior 的逐文档信号（开了才有；响应露出，取舍可查）。
	KeyPrior = context.NewKey[[]prior.FileScore]("qa.prior.signals")
	// KeyFacts / KeyFactReport 是事实分解与事实覆盖（cumulus 的准确定
	// 义：事实点全盖）。查询侧拆事实，证据侧逐条判盖到没有。
	KeyFacts      = context.NewKey[[]facts.Fact]("qa.facts")
	KeyFactReport = context.NewKey[facts.Report]("qa.fact.report")
	// KeyAbstain 是零 LLM 失败预测头的裁决。
	KeyAbstain = context.NewKey[abstain.Verdict]("qa.abstain")
	// KeyConflicts 是证据一致性门的冲突列表（同事实两窗给不同的数）。
	KeyConflicts = context.NewKey[[]facts.Conflict]("qa.evidence.conflicts")
	KeyWindows   = context.NewKey[[]EvidenceWindow]("qa.evidence.windows")
	KeyRoute     = context.NewKey[RouteDecision]("qa.route")
	KeyAnswer    = context.NewKey[Answer]("qa.answer")
	KeyUsage     = context.NewKey[Usage]("qa.usage")
	// KeySynthUsage 是合成步记下的用量。记账步只读它——账目和发生额
	// 是同一个来源，不许两处各写一份。
	KeySynthUsage = context.NewKey[Usage]("qa.synthesis.usage")
)

// Rewrite 是 stage 1 的产物：原问 + 假设文档双视图。
type Rewrite struct {
	Original      string
	Hypothetical  string   // 改写文本（HyDE 式假设摘要）；空 = 用原问检索
	DriftRejected bool     // 改写被漂移闸拦下（内容词丢了原问的）
	DroppedTerms  []string // 被拦下的改写丢了哪些内容词
}

// Effective 是检索实际使用的文本：改写通过漂移闸才用，否则原问。
// 覆盖度始终按 Original 算——改写管召回，原问管接地。
func (r Rewrite) Effective() string {
	if r.Hypothetical != "" && !r.DriftRejected {
		return r.Hypothetical
	}
	return r.Original
}

// EvidenceWindow 是一个证据窗口。SourceID + Span 非空是引用可回溯的
// 最低契约，Verify 强制；Text 是窗口原文——合成面读它，模型看不到
// 原文就等于没有证据（第一次真调用学到的：坐标不是内容）。
type EvidenceWindow struct {
	SourceID  string
	Title     string // 文档身份（法律名/文档题）——合成提示词用它标注窗口
	Span      string
	Text      string
	Score     float64
	Substrate string // text / structured / cross-doc，来自底物注册
}

// RouteDecision 是 stage 3 的路由结论。信号与阈值都留痕，供回放
// “当时为什么升级/拒答”。
type RouteDecision struct {
	Action   string // fast / escalate / refuse
	Signals  RouteSignals
	Grounded bool
	Reason   string
}

// Answer 是 stage 4 的产物。Citations 必须能映射回窗口 id。
type Answer struct {
	Text          string
	Citations     []string // window 标识（SourceID#Span）
	Refused       bool
	RefusalReason string // 拒答理由（模型给的解释）。Text 保持空——
	// 理由不当答案，不许进判官与规则臂（真运行学到的：模型拒答天然
	// 带解释，硬要清空等于把有用信息扔掉，混进 Text 又成了夹带）
}

// Usage 是 stage 5 的记账。上游不报 usage 时 CostKnown=false，
// 按流程文法不许继续发起付费调用。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CostKnown        bool
}

// ---------- stage 1: 意图澄清 ----------

// RewriteStage 读会话、写改写。Verify 强制“改写可逆回原问”：
// 防假设漂移把用户的意图带走。

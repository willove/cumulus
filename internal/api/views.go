package api

// 本文件是 HTTP 面的 **DTO 层**：请求/响应与视图结构。
// 契约由这些结构体反射生成（cmd/contract-gen → docs/contract/），
// 所以它们单独一处、只做形状，不掺 handler 逻辑——形状与行为分开，
// 改形状必过 contract-gen 门，改行为不动形状。

import (
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/qaflow"
)

// QARequest 是 POST /v1/qa 的请求。
type QARequest struct {
	Question string `json:"question"`
	Session  string `json:"session"` // 空 = 不问复用（一次性问答）
}

// QAResponse 是一次问答的完整记录（committed view 的 HTTP 形状）。
type QAResponse struct {
	Question  string                  `json:"question"`
	Answer    string                  `json:"answer"`
	Refused   bool                    `json:"refused"`
	Reason    string                  `json:"refusal_reason,omitempty"`
	Citations []string                `json:"citations"`
	Analysis  AnalysisView            `json:"analysis"`
	Prior     []PriorView             `json:"prior,omitempty"`     // 空 = 未开多信号重排
	Facts     []FactView              `json:"facts,omitempty"`     // 事实分解与逐条覆盖
	Conflicts []ConflictView          `json:"conflicts,omitempty"` // 证据一致性门：同事实不同值
	Abstain   *AbstainView            `json:"abstain,omitempty"`   // 零 LLM 失败预测头裁决
	Route     RouteView               `json:"route"`
	Escalate  qaflow.EscalationRecord `json:"escalation"`
	Reuse     qaflow.ReuseState       `json:"reuse"`
	Coverage  CoverageView            `json:"coverage"`
	Eviction  EvictionView            `json:"eviction"`
	Rerank    qaflow.RerankState      `json:"rerank"`
	Windows   []WindowView            `json:"windows"`
	Usage     UsageView               `json:"usage"`
	// Classification 是窗口分级（GaRAGe 四类）的读数：**可解释性**用，
	// 不参与检索与路由。三段分开是因为"没开""开了失败""开了成功"要能分开。
	Classification *ClassificationView `json:"classification,omitempty"`
	// Committed 是本次迁移的提交视图（不变量 4 的执行处）：这次答案
	// 针对哪版语料/配置/策略，以及路由实际生效的档位与校准程序。
	// 缺了它，"答案为什么变了"只能靠猜——语料在长（个人库），版本必须
	// 跟着答案走。
	Committed context.CommittedView `json:"committed"`
}

// AnalysisView 是查询侧理解的快照：意图、IDF 加权主关键词级（着重/降权
// 的取舍看得见）、语料外词（词汇鸿沟信号）。查询侧做了什么，答案旁边
// 直接可查。
type AnalysisView struct {
	Intent  string             `json:"intent"`
	Primary map[string]float64 `json:"primary"` // 语词 → 权重（2.0 着重/1.0 平权）
	OOV     []string           `json:"out_of_corpus_terms,omitempty"`
	Score   float64            `json:"score"` // 主级总权重（稀薄度代理）
}

// RouteView 是路由判定（含信号——可审计）。
type RouteView struct {
	Action  string              `json:"action"`
	Reason  string              `json:"reason"`
	Signals qaflow.RouteSignals `json:"signals"`
}

// CoverageView 是覆盖度 + 语料外词。
type CoverageView struct {
	Value float64  `json:"value"`
	OOV   []string `json:"out_of_corpus_terms,omitempty"`
}

// EvictionView 是驱逐账。
type EvictionView struct {
	Merged  int `json:"merged"`
	Dropped int `json:"dropped"`
}

// WindowView 是一个证据窗口。
// PriorView 是一篇文档的多信号置信（cumulus prior 移植的可视化：
// lexical 无长度归一 / 标题 / 条文结构，融合后置顶归一）。
type PriorView struct {
	DocID   string             `json:"doc_id"`
	Score   float64            `json:"score"`
	Signals map[string]float64 `json:"signals"`
	Title   string             `json:"title,omitempty"`
}

// FactView 是一条事实的覆盖判定与支撑证据（可溯证据路径：每个窗支撑
// 哪条事实，逐窗可查——SUBQRAG 的 graph memory 在我们这里的露出）。
type FactView struct {
	ID       string        `json:"id"`
	Query    string        `json:"query"`
	Covered  bool          `json:"covered"`
	NearMiss float64       `json:"near_miss,omitempty"`
	Supports []SupportView `json:"supports,omitempty"`
	Judge    string        `json:"judge,omitempty"` // 判官裁决（rescued/no-support/error:…）
}

// SupportView 是支撑某条事实的一个窗口。
type SupportView struct {
	SourceID string  `json:"source_id"`
	Title    string  `json:"title,omitempty"`
	Span     string  `json:"span"`
	Score    float64 `json:"score"`
}

// ConflictView 是同事实两窗给不同的数的冲突（证据一致性门产出）。
type ConflictView struct {
	FactID    string   `json:"fact_id"`
	Values    []string `json:"values"`
	SourceIDs []string `json:"source_ids"`
}

// AbstainView 是失败预测头的裁决（p_fail 与动作）。
type AbstainView struct {
	PFail  float64 `json:"p_fail"`
	Action string  `json:"action"`
	Reason string  `json:"reason,omitempty"`
}

type WindowView struct {
	SourceID string  `json:"source_id"`
	Title    string  `json:"title"` // 文档身份（法律名）——前端要显示"这是哪份文档的第几条"
	Span     string  `json:"span"`
	Text     string  `json:"text"`
	Score    float64 `json:"score"`
}

// HealthResponse 是 GET /v1/health 的响应（类型化——契约从代码生成，
// map[string]any 生成不出契约）。
type HealthResponse struct {
	Status     string `json:"status"`
	CorpusDocs int    `json:"corpus_docs"`
	Realm      string `json:"realm"`
	// CorpusVersion 是语料内容摘要（条数 + sha 前 12 位）——健康面报
	// 的是"我在答哪一版语料"，不是"我活着"。
	CorpusVersion string `json:"corpus_version"`
}

// SignalRequest 是 POST /v1/signal 的请求体（前端钩子用）。目前唯一
// 的实际发送方是引用点击（cite）；接收端不锁死类型，服务端推导的四
// 族之外的新族从这里进（加族前必须有真实使用在要它，不预先建）。
type SignalRequest struct {
	Session  string `json:"session"`
	Kind     string `json:"kind"`
	Target   string `json:"target"`   // cite 的 "doc#span"
	Question string `json:"question"` // 可选：被点的答案对应的问句
}

// StatusResponse 是 GET /v1/status 的响应：可选组件的启停。cumulus 的
// MCS 静默不触发，缺的就是这一面。
type StatusResponse struct {
	Synthesis bool `json:"synthesis"`
	Embedder  bool `json:"embedder"`
	Reuse     bool `json:"reuse"`
	Escalate  bool `json:"escalate"`
}

// UsageView 是用量账。CostKnown=false 表示上游不报（成本未知不是 0）。
type UsageView struct {
	PromptTokens     int  `json:"prompt_tokens"`
	CompletionTokens int  `json:"completion_tokens"`
	CostKnown        bool `json:"cost_known"`
}

// ClassificationView 是窗口分级的读数。
type ClassificationView struct {
	Enabled bool           `json:"enabled"`           // 分类器装了没
	Applied bool           `json:"applied"`           // 这一问真的分级了没
	Outcome string         `json:"outcome,omitempty"` // 失败原因（有则说明装了但没成）
	Counts  map[string]int `json:"counts,omitempty"`  // 各类各几条（rank → 类别 → 计数）
}

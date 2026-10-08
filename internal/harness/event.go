// Package harness 是**对外输出面**：问答过程的全部可观测事实，以**事件流**的形式
// 交给外部（UI、别的 agent、CLI、离线回放）。
//
// 它存在的理由：harness 的职责就是"让人看见系统在干什么"。旧 cumulus 有过一套
// 输出流（OpenAI chat 帧 + `cumulus` 扩展帧：stage/file/reasoning/citations/done），
// 能力搬到了新骨架，**这个"面"没搬过来**——现在只有一次性 JSON 响应，过程全部
// 压在跑完之后。本包把它补回来，并按新骨架的纪律重做。
//
// 四条契约（都有测试钉死，不靠人记）：
//
//  1. **缺席不改行为**：没挂 sink 时 Emit 是空操作，流程与响应逐字段不变。
//     可选增强不许变成依赖——一次外部服务缺席不许改变问答结果。
//  2. **顺序即真相**：Seq 单调、AtMS 相对起点；事件顺序 = 流程顺序，
//     回放能重建时间线（stage 在窗口之前、引用在答案之后）。
//  3. **降级必须可见**：sink 写失败**不阻断**问答，但计数并可查
//     （Emitter.Health）——"没接上"与"接上了但丢了事件"必须分得开。
//  4. **事件即数据**：每个事件都能 JSON 往返不变，可单独落库重放。
//
// 分层：本包只依赖 kernel（typed key / 提交视图），**不许依赖 pipeline（qaflow）
// 或 apps（api）**——否则它就不是"出口"，而是又一个内部耦合点（boundary 门禁
// 会拦住）。代价是事件载荷用**朴素结构体**表达，不复用 qaflow 的类型；这是有意的
// 解耦成本。
package harness

import (
	"fmt"
	"strings"
	"time"
)

// Kind 是事件类型。词表继承旧 cumulus 的输出流（stage/file/reasoning/content/
// citations/done/error），新增 **related**（关联文档：取回但未被引用的证据）。
type Kind string

// 事件类型取值。
const (
	KindStarted   Kind = "started"   // 会话开始（问题、run id、提交视图）
	KindStage     Kind = "stage"     // 阶段时间线（开始/结束 + 耗时）
	KindFile      Kind = "file"      // 逐窗口打分：哪篇、几分、在哪一段 ← 引用文章/检索日志
	KindReasoning Kind = "reasoning" // 思考片段（合成器的推理）
	KindContent   Kind = "content"   // 答案片段（可整段替换）
	KindCitations Kind = "citations" // 最终引用（docid + 坐标 + 原文，可回溯）
	KindRelated   Kind = "related"   // 关联文档（取回未引用 / 同文档其他片段 / 知识邻居）
	KindError     Kind = "error"     // 错误（仍然礼貌收尾）
	KindDone      Kind = "done"      // 收尾：答案 + 决策 + 计量 + 提交视图
)

// StagePhase 是阶段事件的状态。
type StagePhase string

// 阶段相位。
const (
	PhaseStart StagePhase = "start"
	PhaseDone  StagePhase = "done"
)

// StageInfo 是阶段事件载荷。
type StageInfo struct {
	Name       string     `json:"name"`
	Phase      StagePhase `json:"phase"`
	DurationMS int64      `json:"duration_ms,omitempty"`
}

// FileInfo 是**逐窗口**事件载荷：这是"引用文章"的流式形态——检索到一个窗口就
// 发一个，带文档身份、得分、坐标与预览。旧 cumulus 叫 `stage=file`，沿用同一形状
// （前端的老代码几乎可以直接吃）。
type FileInfo struct {
	Rank    int     `json:"rank"` // 本次取数里的名次（1 起）
	DocID   string  `json:"doc_id"`
	Title   string  `json:"title,omitempty"`
	Score   float64 `json:"score"`
	Span    string  `json:"span,omitempty"`    // 窗口坐标（可回溯）
	Preview string  `json:"preview,omitempty"` // 原文片段（截断）
	Cited   bool    `json:"cited,omitempty"`   // 最终是否被引用（done 时可回填）
}

// TextInfo 是文本片段载荷（思考/答案）。Replace=true 表示**整段替换**而非追加
// （合成器改写答案时用）。
type TextInfo struct {
	Text    string `json:"text"`
	Replace bool   `json:"replace,omitempty"`
	// Channel 区分思考与答案（reasoning 事件恒为 thinking，content 事件为 answer）。
	Channel string `json:"channel,omitempty"`
}

// Citation 是最终引用：坐标 + 原文，**可回溯**（这是"证据可核"的执行处）。
type Citation struct {
	DocID    string `json:"doc_id"`
	Title    string `json:"title,omitempty"`
	Span     string `json:"span,omitempty"`
	Text     string `json:"text,omitempty"`
	Resolved bool   `json:"resolved"` // 坐标能否还原
}

// RelatedInfo 是**关联文档**：与问题相关但**没被引用**的证据。它回答"系统还看到
// 什么"——检索面比答案面宽，这是 RAG 与搜索最大的差别。
type RelatedInfo struct {
	DocID   string  `json:"doc_id"`
	Title   string  `json:"title,omitempty"`
	Score   float64 `json:"score,omitempty"`
	Span    string  `json:"span,omitempty"`
	Preview string  `json:"preview,omitempty"`
	// Why 说明它为什么算关联（retrieved-uncited / same-doc-other-span / knowledge-neighbour）
	Why string `json:"why,omitempty"`
}

// DoneInfo 是收尾载荷：答案 + 决策 + 计量 + **提交视图**（这次答案针对哪版语料/
// 配置/策略，以及路由实际生效的档位与校准程序）。提交视图在这里比在静态响应里
// 更重要——流式消费者往往不回查 /v1/qa。
type DoneInfo struct {
	Answer           string  `json:"answer"`
	Refused          bool    `json:"refused,omitempty"`
	RefusalReason    string  `json:"refusal_reason,omitempty"`
	RouteAction      string  `json:"route_action,omitempty"`
	RouteTier        string  `json:"route_tier,omitempty"`
	Threshold        float64 `json:"threshold,omitempty"`
	Coverage         float64 `json:"coverage,omitempty"`
	LatencyMS        int64   `json:"latency_ms,omitempty"`
	PromptTokens     int     `json:"prompt_tokens,omitempty"`
	CompletionTokens int     `json:"completion_tokens,omitempty"`
	CostKnown        bool    `json:"cost_known,omitempty"`
	// Decision 是决策层留痕（§三·八）：有没有真决策、哪条判据、为什么。
	DecisionKind    string `json:"decision_kind,omitempty"`
	DecisionApplied bool   `json:"decision_applied,omitempty"`
	DecisionReason  string `json:"decision_reason,omitempty"`
	// Committed 是提交视图的四版本 + 校准（逗号分隔的 k=v，够外部展示与对账）。
	Committed string `json:"committed,omitempty"`
	// Counts 是本次事件计数（阶段数/窗口数/引用数/关联数），给进度条收尾用。
	Counts map[string]int `json:"counts,omitempty"`
}

// Event 是一次输出事件。载荷用可选字段（老前端与通用客户端都习惯这种形状），
// 但**每个 Kind 必填的字段由构造器校验**——事件流里出现半截事件比不出事件更坏。
type Event struct {
	Seq   int    `json:"seq"`
	Kind  Kind   `json:"kind"`
	AtMS  int64  `json:"at_ms"`
	RunID string `json:"run_id,omitempty"`

	Question string `json:"question,omitempty"` // started

	Stage     *StageInfo    `json:"stage,omitempty"`
	File      *FileInfo     `json:"file,omitempty"`
	Reasoning *TextInfo     `json:"reasoning,omitempty"`
	Content   *TextInfo     `json:"content,omitempty"`
	Citations []Citation    `json:"citations,omitempty"`
	Related   []RelatedInfo `json:"related,omitempty"`
	Done      *DoneInfo     `json:"done,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// 构造器：每个都保证必填字段齐、且留痕可校验。返回 error 而不是 panic——
// 事件构造失败不该带走问答流程（契约 1）。
func Started(runID, question string) (Event, error) {
	if strings.TrimSpace(question) == "" {
		return Event{}, fmt.Errorf("harness: started 缺 question")
	}
	return Event{Kind: KindStarted, RunID: runID, Question: question}, nil
}

func Stage(runID, name string, phase StagePhase, durationMS int64) (Event, error) {
	if name == "" {
		return Event{}, fmt.Errorf("harness: stage 缺 name")
	}
	if phase != PhaseStart && phase != PhaseDone {
		return Event{}, fmt.Errorf("harness: stage 非法相位 %q", phase)
	}
	return Event{Kind: KindStage, RunID: runID, Stage: &StageInfo{Name: name, Phase: phase, DurationMS: durationMS}}, nil
}

func File(runID string, f FileInfo) (Event, error) {
	if f.DocID == "" {
		return Event{}, fmt.Errorf("harness: file 缺 doc_id")
	}
	if f.Rank <= 0 {
		return Event{}, fmt.Errorf("harness: file 缺 rank")
	}
	return Event{Kind: KindFile, RunID: runID, File: &f}, nil
}

func Reasoning(runID, text string) (Event, error) {
	if text == "" {
		return Event{}, fmt.Errorf("harness: reasoning 缺文本")
	}
	return Event{Kind: KindReasoning, RunID: runID, Reasoning: &TextInfo{Text: text, Channel: "thinking"}}, nil
}

func Content(runID, text string, replace bool) (Event, error) {
	if text == "" {
		return Event{}, fmt.Errorf("harness: content 缺文本")
	}
	return Event{Kind: KindContent, RunID: runID, Content: &TextInfo{Text: text, Replace: replace, Channel: "answer"}}, nil
}

func Citations(runID string, cs []Citation) (Event, error) {
	for _, c := range cs {
		if c.DocID == "" {
			return Event{}, fmt.Errorf("harness: citation 缺 doc_id")
		}
	}
	return Event{Kind: KindCitations, RunID: runID, Citations: cs}, nil
}

func Related(runID string, rs []RelatedInfo) (Event, error) {
	for _, r := range rs {
		if r.DocID == "" {
			return Event{}, fmt.Errorf("harness: related 缺 doc_id")
		}
	}
	return Event{Kind: KindRelated, RunID: runID, Related: rs}, nil
}

func Failed(runID string, err error) (Event, error) {
	if err == nil {
		return Event{}, fmt.Errorf("harness: error 事件缺错误")
	}
	return Event{Kind: KindError, RunID: runID, Error: err.Error()}, nil
}

func Done(runID string, d DoneInfo) (Event, error) {
	if strings.TrimSpace(d.Answer) == "" && !d.Refused {
		return Event{}, fmt.Errorf("harness: done 既没有答案也没标记拒答")
	}
	return Event{Kind: KindDone, RunID: runID, Done: &d}, nil
}

// Validate 检查事件自洽（必填字段齐、序号非负）。**收流侧也要查**：外部消费
// 一个半截事件时，应该能立刻看出来，而不是解析到一半才发现。
func (e Event) Validate() error {
	switch e.Kind {
	case KindStarted:
		if e.Question == "" {
			return fmt.Errorf("started 缺 question")
		}
	case KindStage:
		if e.Stage == nil || e.Stage.Name == "" {
			return fmt.Errorf("stage 缺载荷")
		}
	case KindFile:
		if e.File == nil || e.File.DocID == "" || e.File.Rank <= 0 {
			return fmt.Errorf("file 缺载荷（doc_id/rank 必填）")
		}
	case KindReasoning:
		if e.Reasoning == nil || e.Reasoning.Text == "" {
			return fmt.Errorf("reasoning 缺载荷")
		}
	case KindContent:
		if e.Content == nil || e.Content.Text == "" {
			return fmt.Errorf("content 缺载荷")
		}
	case KindCitations:
		for _, c := range e.Citations {
			if c.DocID == "" {
				return fmt.Errorf("citation 缺 doc_id")
			}
		}
	case KindRelated:
		for _, r := range e.Related {
			if r.DocID == "" {
				return fmt.Errorf("related 缺 doc_id")
			}
		}
	case KindError:
		if e.Error == "" {
			return fmt.Errorf("error 缺错误文本")
		}
	case KindDone:
		if e.Done == nil {
			return fmt.Errorf("done 缺载荷")
		}
		if strings.TrimSpace(e.Done.Answer) == "" && !e.Done.Refused {
			return fmt.Errorf("done 既没有答案也没标记拒答")
		}
	default:
		return fmt.Errorf("未知事件类型 %q", e.Kind)
	}
	if e.Seq < 0 {
		return fmt.Errorf("事件序号非负（%d）", e.Seq)
	}
	return nil
}

// sinceMS 是相对起点的毫秒（AtMS 的来源）。
func sinceMS(start time.Time) int64 { return time.Since(start).Milliseconds() }

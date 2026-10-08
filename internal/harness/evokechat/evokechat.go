// Package evokechat 把 harness 事件翻译成 **@wil-works/evoke-chat** 的引擎调用。
//
// 它是**协议适配**，不是界面：这个包不认识 Vue、不认识 CSS，只输出"宿主该调用
// 哪些引擎方法、带什么参数"。宿主（我们的应用、或任何别的消费方）照着执行即可。
//
// 为什么值得单独一层：evoke-chat 的契约（docs/chat/ai-contract.md）要求
//
//	appendContent / appendThinkContent 分开、阶段进度走 assistant/progress
//	瞬时事件（"别把阶段进度拼进 think 文本"）、事件信封 {type, seq, time, data}
//	且 seq 连续、瞬时事件不推进游标也不造成缺口。
//
// 这些规则**每个后端都要各自踩一遍**；写在这里=写一次、测一次。
//
// 零依赖：只用 harness 的事件类型与标准库。
package evokechat

import (
	"fmt"

	"github.com/willove/cumulus/internal/harness"
)

// Op 是宿主要执行的一次引擎调用（对应 evoke-chat 的 API）。
type Op string

// 调用类型。
const (
	OpCreateAssistant Op = "create-assistant-message"
	OpAppendContent   Op = "append-content"
	OpAppendThink     Op = "append-think"
	OpProgress        Op = "progress"
	OpCitations       Op = "citations"
	OpToolCall        Op = "tool-call"
	OpComplete        Op = "complete"
	OpError           Op = "error"
	OpSetContext      Op = "set-context"
)

// Call 是一次调用。Args 的键与 evoke-chat 的同名 API 参数一致。
type Call struct {
	Op        Op
	Args      map[string]any
	Transient bool // 瞬时事件：不推进游标、不造成缺口（evoke-chat 的硬规则）
}

// State 是翻译所需的位置状态（消息 id、是否已开场）。
//
// 为什么需要状态：evoke-chat 要求**先 createAssistantMessage 再追加**，
// 而事件流里第一条不一定是 started（例如只截取了一段流）。翻译器不猜，
// 缺前置就报错（Options.Strict）或跳过（默认宽松）。
type State struct {
	MessageID string
	Opened    bool
}

// Options 是翻译口径。
type Options struct {
	// Strict：缺前置/未知事件时返回错误；默认 false = 跳过并记录。
	Strict bool
	// FilesAsToolCalls：把 file 事件翻成**工具调用卡**（"检索"作为一次工具
	// 调用，结果是命中的文件列表），而不是进度行。
	//
	// 两种都合法，进货不同：
	//   - 进度行：轻，留在时间轴上，适合"扫一眼进展"；
	//   - 工具卡：重（可折叠、可展开原文），适合"回看这次检索到了什么"。
	// 默认 false（轻），因为契约明确说**结构化进度别堆进 think**——工具卡
	// 是结构化呈现，不是文本。
	FilesAsToolCalls bool
}

// Result 是一次翻译的产物。
type Result struct {
	Calls     []Call
	Skipped   int   // 被跳过的帧（宽松模式下）
	FirstErr  error // 跳过的原因（宽松模式：记下来，不吞）
	Transient bool  // 本帧是否瞬时（不推进游标）
}

// Translate 把一帧事件翻成调用序列。
func Translate(ev harness.Event, st *State, opt Options) Result {
	var res Result
	add := func(op Op, args map[string]any, transient bool) {
		res.Calls = append(res.Calls, Call{Op: op, Args: args, Transient: transient})
	}
	res.Transient = transientOf(ev)

	// 兜底开场：任何内容类事件在没开场时都必须先建消息（截断的流也能显示）。
	needOpen := ev.Kind == harness.KindStarted ||
		ev.Kind == harness.KindReasoning || ev.Kind == harness.KindContent ||
		ev.Kind == harness.KindCitations || ev.Kind == harness.KindRelated ||
		ev.Kind == harness.KindDone
	if needOpen && !st.Opened {
		if ev.Kind == harness.KindStarted && ev.Question != "" && st.MessageID == "" {
			// 有 run/seq 可用时用它当消息 id（回放时幂等）
			if ev.RunID != "" {
				st.MessageID = ev.RunID
			}
		}
		add(OpCreateAssistant, map[string]any{
			"messageId": st.MessageID,
			"meta": map[string]any{
				"seq":   ev.Seq,
				"atMs":  ev.AtMS,
				"runId": ev.RunID,
			},
		}, true)
		st.Opened = true
	}

	switch ev.Kind {
	case harness.KindStarted:
		// 开场在上面处理了；问题本身进 meta 供 UI 显示
		if len(res.Calls) > 0 {
			if meta, ok := res.Calls[0].Args["meta"].(map[string]any); ok {
				meta["question"] = ev.Question
			}
		}
	case harness.KindStage:
		if ev.Stage == nil {
			return skip(res, ev, opt, "stage 帧缺载荷")
		}
		add(OpProgress, map[string]any{
			"messageId": st.MessageID,
			"label":     progressLabel(ev.Stage),
			"detail":    ev.Stage.Detail,
			"elapsedMs": ev.Stage.DurationMS,
			"percent":   ev.Stage.Percent,
			"stage":     ev.Stage.Name,
			"phase":     string(ev.Stage.Phase),
		}, true)
	case harness.KindFile:
		if ev.File == nil {
			return skip(res, ev, opt, "file 帧缺载荷")
		}
		if opt.FilesAsToolCalls {
			add(OpToolCall, map[string]any{
				"messageId": st.MessageID,
				"name":      "knowledge_retrieval",
				"label":     "检索证据",
				"args": map[string]any{
					"rank":  ev.File.Rank,
					"docId": ev.File.DocID,
					"score": ev.File.Score,
					"span":  ev.File.Span,
				},
				"result": ev.File.Preview,
			}, true)
			return res
		}
		add(OpProgress, map[string]any{
			"messageId": st.MessageID,
			"label":     fmt.Sprintf("命中 %s", ev.File.Title),
			"detail":    fmt.Sprintf("#%d 得分 %.2f", ev.File.Rank, ev.File.Score),
			"stage":     "evidence-supply",
			"phase":     "hit",
		}, true)
	case harness.KindReasoning:
		if ev.Reasoning == nil || ev.Reasoning.Text == "" {
			return skip(res, ev, opt, "reasoning 帧缺文本")
		}
		add(OpAppendThink, map[string]any{"messageId": st.MessageID, "text": ev.Reasoning.Text}, false)
	case harness.KindContent:
		if ev.Content == nil || ev.Content.Text == "" {
			return skip(res, ev, opt, "content 帧缺文本")
		}
		if ev.Content.Replace {
			add(OpComplete, map[string]any{"messageId": st.MessageID, "content": ev.Content.Text}, false)
			break
		}
		add(OpAppendContent, map[string]any{"messageId": st.MessageID, "text": ev.Content.Text}, false)
	case harness.KindCitations:
		if len(ev.Citations) == 0 {
			return skip(res, ev, opt, "citations 帧没有引用")
		}
		add(OpCitations, map[string]any{
			"messageId": st.MessageID,
			"refs":      mapRefs(ev.Citations),
		}, false)
	case harness.KindRelated:
		if len(ev.Related) == 0 {
			return skip(res, ev, opt, "related 帧为空")
		}
		// 关联文档是**引用面板的补充**，不是引用：交给宿主决定挂哪
		// （面板里多一栏"还看到但没引用"）。这里走 progress 之外的独立 op，
		// 免得消费方把它混进引用列表（那就变成"引用了但没引"了）。
		add("related", map[string]any{
			"messageId": st.MessageID,
			"items":     mapRelated(ev.Related),
		}, true)
	case harness.KindError:
		add(OpError, map[string]any{"messageId": st.MessageID, "text": ev.Error}, false)
	case harness.KindDone:
		d := ev.Done
		args := map[string]any{"messageId": st.MessageID}
		if d != nil {
			if d.Refused {
				args["refused"] = true
				args["reason"] = d.RefusalReason
			}
			if u, ok := d.Counts["delivered"]; ok {
				args["events"] = u
			}
			if d.Committed != "" {
				args["committed"] = d.Committed
			}
		}
		add(OpComplete, args, false)
		if d != nil && (d.PromptTokens > 0 || d.CompletionTokens > 0) {
			// 上下文占用：**拿不到窗口容量就不画环**（契约硬规则：used 与
			// capacity 缺一不渲染）。我们不知道模型的窗口，所以只给 used，
			// 让宿主决定填不填 capacity。
			add(OpSetContext, map[string]any{
				"used": d.PromptTokens + d.CompletionTokens,
			}, true)
		}
	default:
		return skip(res, ev, opt, "未知事件类型")
	}
	return res
}

// Envelope 是 evoke-chat 的**会话日志事件信封** `{type, seq, time, data}`。
//
// 为什么单独一个函数：信封是**传输层**的事（游标、补页、断线恢复），
// 与事件内容无关。seq 必须连续、缺口会被缓冲并回调 onGap——所以我们的
// 序号必须**一条不落**地映射过去（这正是 harness 契约 2 的用处）。
func Envelope(ev harness.Event) map[string]any {
	return map[string]any{
		"type": envelopeType(ev.Kind),
		"seq":  ev.Seq,
		"time": ev.AtMS,
		"data": payloadOf(ev),
	}
}

// transientOf 判事件是否**瞬时**（不推进游标、不造成缺口）。
//
// 分档的依据是"这条信息丢了会不会让历史不可重放"：
//   - 瞬时：started / stage（进度是当下的快照，历史里重放它没意义，
//     而且它到达得最密——每条都推进游标会让补页协议变脆）；
//   - 持久：reasoning / content / citations / related / done / error
//     （这些构成"这一轮说了什么"，丢了就是历史缺了一块）。
func transientOf(ev harness.Event) bool {
	switch ev.Kind {
	case harness.KindStarted, harness.KindStage:
		return true
	default:
		return false
	}
}

func envelopeType(kind harness.Kind) string {
	switch kind {
	case harness.KindStage:
		return "assistant/progress"
	case harness.KindReasoning:
		return "assistant/thinking"
	case harness.KindContent:
		return "assistant/content"
	case harness.KindCitations:
		return "assistant/citations"
	case harness.KindRelated:
		return "assistant/related"
	case harness.KindDone:
		return "assistant/done"
	case harness.KindError:
		return "assistant/error"
	case harness.KindStarted:
		return "assistant/started"
	case harness.KindFile:
		return "assistant/progress" // file 默认走进度行；工具卡模式下由宿主改写
	default:
		return "assistant/unknown"
	}
}

// payloadOf 是事件内容（信封的 data）。**只放这一类事件自己的字段**——
// 别把整个事件塞进去，消费方会开始依赖不属于它的字段。
func payloadOf(ev harness.Event) map[string]any {
	switch ev.Kind {
	case harness.KindStarted:
		return map[string]any{"runId": ev.RunID, "question": ev.Question}
	case harness.KindStage:
		if ev.Stage == nil {
			return map[string]any{}
		}
		return map[string]any{
			"stage": ev.Stage.Name, "phase": string(ev.Stage.Phase),
			"label": ev.Stage.Label, "detail": ev.Stage.Detail,
			"elapsedMs": ev.Stage.DurationMS, "percent": ev.Stage.Percent,
		}
	case harness.KindFile:
		if ev.File == nil {
			return map[string]any{}
		}
		return map[string]any{
			"rank": ev.File.Rank, "docId": ev.File.DocID, "title": ev.File.Title,
			"score": ev.File.Score, "span": ev.File.Span, "preview": ev.File.Preview,
		}
	case harness.KindReasoning:
		if ev.Reasoning == nil {
			return map[string]any{}
		}
		return map[string]any{"text": ev.Reasoning.Text}
	case harness.KindContent:
		if ev.Content == nil {
			return map[string]any{}
		}
		return map[string]any{"text": ev.Content.Text, "replace": ev.Content.Replace}
	case harness.KindCitations:
		return map[string]any{"refs": mapRefs(ev.Citations)}
	case harness.KindRelated:
		return map[string]any{"items": mapRelated(ev.Related)}
	case harness.KindError:
		return map[string]any{"text": ev.Error}
	case harness.KindDone:
		if ev.Done == nil {
			return map[string]any{}
		}
		return map[string]any{
			"answer": ev.Done.Answer, "refused": ev.Done.Refused,
			"reason": ev.Done.RefusalReason, "committed": ev.Done.Committed,
			"counts": ev.Done.Counts,
		}
	default:
		return map[string]any{}
	}
}

// mapRefs 把引用翻成 evoke-chat 的形状（title/text/sourceId/url）。
func mapRefs(cs []harness.Citation) []map[string]any {
	out := make([]map[string]any, 0, len(cs))
	for _, c := range cs {
		title := c.Title
		if title == "" {
			title = c.DocID
		}
		out = append(out, map[string]any{
			"sourceId": c.DocID,
			"title":    title,
			"text":     c.Text,
			"url":      "/v1/doc/" + c.DocID,
			"resolved": c.Resolved,
		})
	}
	return out
}

func mapRelated(rs []harness.RelatedInfo) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		title := r.Title
		if title == "" {
			title = r.DocID
		}
		out = append(out, map[string]any{
			"sourceId": r.DocID, "title": title, "why": r.Why,
			"score": r.Score, "preview": r.Preview, "url": "/v1/doc/" + r.DocID,
		})
	}
	return out
}

// progressLabel 优先用阶段自带的中文标签；没有就退回阶段名（**不许编**）。
func progressLabel(s *harness.StageInfo) string {
	if s.Label != "" {
		return s.Label
	}
	return harness.StageLabel(s.Name)
}

func skip(res Result, ev harness.Event, opt Options, why string) Result {
	if opt.Strict {
		res.FirstErr = fmt.Errorf("evokechat: %s 帧（seq=%d kind=%s）无法翻译：%s", ev.Kind, ev.Seq, ev.Kind, why)
		return res
	}
	res.Skipped++
	if res.FirstErr == nil {
		res.FirstErr = fmt.Errorf("evokechat: %s 帧（seq=%d）已跳过：%s", ev.Kind, ev.Seq, why)
	}
	return res
}

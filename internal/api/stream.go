package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/harness"
	"github.com/willove/cumulus/internal/qaflow"
)

// handleQAStream 是 `/v1/qa` 的**流式同款**：同一次问答，过程以事件流的形式
// 实时送出（阶段进度、逐窗口检索日志、引用、关联文档、最终答案与提交视图）。
//
// 为什么不改 `/v1/qa` 而是新增一个端点：既有客户端（脚本、其他服务）读的是
// 一次性 JSON，**改形状等于破坏兼容**。流式是**可选增强**——同一个契约
// （harness 契约 1）在 HTTP 层的再现：不挂它，`/v1/qa` 逐字节不变。
//
// 帧形状继承旧 cumulus（`event: <kind>` + `data: <json>`），老前端可直接吃。
func (s *Server) handleQAStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req QARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		writeErr(w, http.StatusBadRequest, "question is required")
		return
	}
	flusher, _ := w.(http.Flusher)
	if flusher == nil {
		// 不能流的环境就明说，不给一个"看起来在流其实全缓冲"的端点。
		writeErr(w, http.StatusInternalServerError, "streaming unsupported by this server")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx 默认会缓冲 SSE，必须显式关
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	runID := streamRunID(req)
	stream := harness.NewSSE(w, flusher.Flush)
	em := harness.NewEmitter(stream)

	emit := func(ev harness.Event) { _ = em.Emit(ev) }
	if ev, err := harness.Started(runID, req.Question); err == nil {
		emit(ev)
	} else {
		emit(mustFailed(runID, err))
		stream.Done()
		return
	}

	c := context.New(context.Realm(s.Realm))
	if s.Embedder != nil {
		if err := qaflow.BindEmbedder(c, s.Embedder); err != nil {
			emit(mustFailed(runID, err))
			stream.Done()
			return
		}
	}
	opts := s.Options
	opts.Reuse = s.Reuse
	opts.Session = req.Session
	if s.Escalate != nil {
		opts.Escalate = s.Escalate
	}
	opts.CorpusVersion, opts.ConfigVersion, opts.StrategyVersion, opts.BeliefVersion = s.committedVersions()
	opts.Emitter = em
	opts.RunID = runID
	opts.StreamSynth = s.StreamSynth

	runner := qaflow.Runner(req.Question, qaflow.BM25Evidence(s.Index(), s.TopK, s.Width), s.Synth, opts)
	if err := runner.Run(c); err != nil {
		// 失败也是**一等事件**：发 error 再收尾（客户端据此停进度条）。
		emit(mustFailed(runID, err))
		stream.Done()
		return
	}

	resp := s.record(c, req.Question)
	// 合成器支持流式 = 答案正文已经逐段发过。
	streamedAnswer := s.StreamSynth != nil
	emitStreamResult(runID, c, resp, runner.View, em, streamedAnswer)

	// 收尾：一帧 done（含提交视图与计量），然后 [DONE]（与 OpenAI 协议同惯例）。
	if ev, err := harness.Done(runID, qaflow.FinalFrom(streamFinal(resp, runner.View, em, c))); err == nil {
		emit(ev)
	}
	stream.Done()
	_ = flusher
}

// emitStreamResult 发终态的四类事件：引用 / 关联文档 / 答案正文。
//
// **顺序有意义**：先引用（证据面）再答案（交付面）——消费者据此知道
// "答案里的每条断言都能对上前面那几条引用"。
func emitStreamResult(runID string, c *context.Context, resp QAResponse, view context.CommittedView, em *harness.Emitter, streamedAnswer bool) {
	emit := func(ev harness.Event) { _ = em.Emit(ev) }
	cits := make([]harness.Citation, 0, len(resp.Citations))
	for _, cit := range resp.Citations {
		id, span := cit, ""
		if i := strings.IndexByte(cit, '#'); i > 0 {
			id, span = cit[:i], cit[i+1:]
		}
		title, text, resolved := "", "", false
		for _, w := range resp.Windows {
			if w.SourceID == id && (span == "" || w.Span == span) {
				title, text, resolved = w.Title, w.Text, true
				break
			}
		}
		cits = append(cits, harness.Citation{DocID: id, Title: title, Span: span, Text: text, Resolved: resolved})
	}
	// **空引用不发帧**：一个没有载荷的 citations 事件是半截事件（harness 契约 4），
	// 而"这一题有没有引用"在 done.counts 里可查——两个渠道说同一件事，不重复。
	if len(cits) > 0 {
		if ev, err := harness.Citations(runID, cits); err == nil {
			emit(ev)
		}
	}
	if ws, ok := context.Get(c, qaflow.KeyWindows); ok {
		if rel := qaflow.RelatedFrom(ws, resp.Citations, 5); len(rel) > 0 {
			if ev, err := harness.Related(runID, rel); err == nil {
				emit(ev)
			}
		}
	}
	// 合成器支持流式时，答案正文**已经**逐段发过了（reasoning/content 帧），
	// 这里绝不能再补一条整段——重复的整段会让客户端把答案显示两遍。
	// 不支持流式才补整段（replace=true）：诚实的形状，伪增量比整段更坏。
	if resp.Answer != "" && !streamedAnswer {
		if ev, err := harness.Content(runID, resp.Answer, true); err == nil {
			emit(ev)
		}
	}
}

// streamFinal 把响应 + 视图 + 出口健康映射成 done 载荷。
func streamFinal(resp QAResponse, view context.CommittedView, em *harness.Emitter, c *context.Context) qaflow.FinalInput {
	in := qaflow.FinalInput{
		Answer: resp.Answer, Refused: resp.Refused, RefusalReason: resp.Reason,
		RouteAction: resp.Route.Action, RouteTier: resp.Route.Signals.Tier,
		Threshold: resp.Route.Signals.Threshold, Coverage: resp.Coverage.Value,
		PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens,
		CostKnown: resp.Usage.CostKnown, Committed: qaflow.CommittedString(view),
	}
	if rec, ok := qaflow.DecisionRecordOf(c); ok {
		in.DecisionKind, in.DecisionApplied, in.DecisionReason = rec.Kind, rec.Applied, rec.Reason
	}
	in.Counts = map[string]int{
		"windows":   len(resp.Windows),
		"citations": len(resp.Citations),
		"delivered": em.Events(),
		"dropped":   em.Health().Dropped, // 出口丢了多少也报上去（降级可见）
	}
	return in
}

// streamRunID 给这次流一个编号。优先用 session（够用且可回放），
// 否则用单调递增的流序号——**每帧都必须能对上"哪一次流"**。
func streamRunID(req QARequest) string {
	if s := strings.TrimSpace(req.Session); s != "" {
		return "stream-" + s
	}
	return fmt.Sprintf("stream-%d", atomic.AddInt64(&streamSeq, 1))
}

var streamSeq int64

func mustFailed(runID string, err error) harness.Event {
	ev, e := harness.Failed(runID, err)
	if e != nil {
		return harness.Event{Kind: harness.KindError, RunID: runID, Error: err.Error()}
	}
	return ev
}

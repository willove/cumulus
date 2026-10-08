package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func fileEv(docID string, rank int, cited bool) FileInfo {
	return FileInfo{Rank: rank, DocID: docID, Title: docID + " 标题", Score: 1.0 / float64(rank), Span: "rune[0:4]", Preview: docID + " 的原文片段", Cited: cited}
}

func emitAll(e *Emitter, t *testing.T) {
	t.Helper()
	ev, err := Started("run-1", "连接池最大连接数是多少")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(ev); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []Event{} {
		_ = ev
	}
	stage, err := Stage("run-1", "evidence", PhaseStart, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(stage); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		ev, err := File("run-1", fileEv("doc-"+string(rune('a'+i-1)), i, i == 2))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	done, err := Stage("run-1", "evidence", PhaseDone, 12)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(done); err != nil {
		t.Fatal(err)
	}
	think, err := Reasoning("run-1", "先看覆盖度，再看边际……")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(think); err != nil {
		t.Fatal(err)
	}
	c1, err := Content("run-1", "最大连接数", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(c1); err != nil {
		t.Fatal(err)
	}
	c2, err := Content("run-1", "是 100。", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(c2); err != nil {
		t.Fatal(err)
	}
	cits, err := Citations("run-1", []Citation{{DocID: "doc-b", Title: "t", Span: "rune[0:4]", Text: "最大连接数默认为 100", Resolved: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(cits); err != nil {
		t.Fatal(err)
	}
	rel, err := Related("run-1", []RelatedInfo{{DocID: "doc-a", Why: "retrieved-uncited", Score: 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(rel); err != nil {
		t.Fatal(err)
	}
	last, err := Done("run-1", DoneInfo{
		Answer: "最大连接数是 100。", LatencyMS: 30, PromptTokens: 10, CompletionTokens: 5,
		RouteAction: "fast", RouteTier: "retrieval", Coverage: 1,
		DecisionKind: "gate/answerable", DecisionReason: "ok", DecisionApplied: true,
		Committed: "corpus=a1b2 config=c3d4 strategy=qa.v0.2",
		Counts:    map[string]int{"stages": 2, "files": 3, "citations": 1, "related": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Emit(last); err != nil {
		t.Fatal(err)
	}
}

// 契约 1：**缺席不改行为**——没挂 sink（以及连发射器都没有）时，发事件不报错、
// 不 panic、不影响任何返回值。
func TestAbsentSinkIsNoop(t *testing.T) {
	e := NewEmitter(nil)
	emitAll(e, t) // 一个都不该失败
	if h := e.Health(); h.Delivered != 0 || h.Dropped != 0 || h.Rejected != 0 {
		t.Fatalf("absent sink must be a silent no-op: %+v", h)
	}
	// 连发射器都没有（流程层没装这一层）也不能炸
	var none *Emitter
	if err := none.Emit(Event{Kind: KindStarted, Question: "q"}); err != nil {
		t.Fatalf("nil emitter must be safe: %v", err)
	}
	if none.Events() != 0 {
		t.Fatal("nil emitter reports no events")
	}
}

// 契约 2：**顺序即真相**——序号从 1 单调，时刻非负，事件顺序即流程顺序。
func TestSequenceAndOrderAreTruth(t *testing.T) {
	rec := NewRecorder()
	e := NewEmitter(rec)
	emitAll(e, t)
	evs := rec.All()
	if len(evs) != 12 {
		t.Fatalf("want 12 events, got %d", len(evs))
	}
	for i, ev := range evs {
		if ev.Seq != i+1 {
			t.Fatalf("seq must be monotonic from 1: event %d has seq %d", i, ev.Seq)
		}
		if ev.AtMS < 0 {
			t.Fatalf("at_ms must be non-negative: %+v", ev)
		}
		if err := ev.Validate(); err != nil {
			t.Fatalf("recorded event must be valid: %v", err)
		}
	}
	kinds := rec.Kinds()
	want := []Kind{KindStarted, KindStage, KindFile, KindFile, KindFile, KindStage, KindReasoning, KindContent, KindContent, KindCitations, KindRelated, KindDone}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event order changed at %d: %v (want %v)", i, kinds, want)
		}
	}
}

// 契约 3：**降级必须可见**——sink 写失败不阻断（Emit 返回 nil）、但计数可查。
func TestSinkFailureDegradesButStaysVisible(t *testing.T) {
	rec := NewRecorder()
	rec.FailWith(errors.New("connection reset"))
	e := NewEmitter(rec)
	emitAll(e, t) // 全程不报错
	h := e.Health()
	if h.Dropped == 0 {
		t.Fatal("sink failures must be counted")
	}
	if h.Delivered != 0 {
		t.Fatalf("nothing should have been delivered: %+v", h)
	}
	var found bool
	for reason := range h.Reasons {
		if strings.Contains(reason, "connection reset") {
			found = true
		}
	}
	if !found {
		t.Fatalf("drop reasons must name the sink error: %+v", h.Reasons)
	}
	// 修好之后恢复投递（不做熔断：出口自己决定要不要放弃）
	rec.FailWith(nil)
	if err := e.Emit(func() Event { ev, _ := Content("run-1", "续上", false); return ev }()); err != nil {
		t.Fatal(err)
	}
	if e.Health().Delivered != 1 {
		t.Fatalf("sink recovery must resume delivery: %+v", e.Health())
	}
}

// 契约 4：**事件即数据**——JSON 往返不变，可单独落库重放。
func TestEventRoundTripsThroughJSON(t *testing.T) {
	rec := NewRecorder()
	e := NewEmitter(rec)
	emitAll(e, t)
	for _, ev := range rec.All() {
		buf, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		var back Event
		if err := json.Unmarshal(buf, &back); err != nil {
			t.Fatal(err)
		}
		if back.Kind != ev.Kind || back.Seq != ev.Seq || back.AtMS != ev.AtMS {
			t.Fatalf("round trip changed the event: %+v → %+v", ev, back)
		}
		again, err := json.Marshal(back)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(buf) {
			t.Fatalf("round trip is not stable:\n%s\n%s", buf, again)
		}
	}
}

// SSE 帧形状必须与旧 cumulus 一致（`event: <kind>` + `data: <json>`），
// 否则老前端要改代码——"继承词表"就得继承到字节级。
func TestSSEFrameShape(t *testing.T) {
	ev, _ := Stage("run-1", "evidence", PhaseStart, 0)
	ev.Seq, ev.AtMS = 3, 42
	buf, err := Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	got := string(buf)
	if !strings.HasPrefix(got, "event: stage\ndata: {") || !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("SSE frame shape changed:\n%q", got)
	}
	if !strings.Contains(got, `"seq":3`) || !strings.Contains(got, `"at_ms":42`) {
		t.Fatalf("frame must carry seq/at_ms:\n%q", got)
	}
}

// SSE 出口：整条流能拼回来，答案与思考都能还原。
func TestSSEStreamDeliversAndRecoversText(t *testing.T) {
	var buf bytes.Buffer
	s := NewSSE(&buf, nil)
	rec := NewRecorder()
	e := NewEmitter(rec)
	emitAll(e, t)
	for _, ev := range rec.All() {
		if err := s.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.String()
	if !strings.Contains(out, "event: file") || !strings.Contains(out, "event: related") {
		t.Fatalf("stream must contain file and related frames:\n%s", out[:min(400, len(out))])
	}
	// 帧数 = 事件数（每帧以空行结束）
	if got := strings.Count(out, "\n\n"); got != len(rec.All()) {
		t.Fatalf("frame count %d != event count %d", got, len(rec.All()))
	}
	if err := s.KeepAlive(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), ": keep-alive\n\n") {
		t.Fatal("keep-alive must be an SSE comment frame")
	}
}

// 构造器拒绝半截事件——**事件流里出现半截事件比不出事件更坏**。
func TestConstructorsRejectIncompleteEvents(t *testing.T) {
	if _, err := Started("r", ""); err == nil {
		t.Fatal("started without question must fail")
	}
	if _, err := File("r", FileInfo{Rank: 1}); err == nil {
		t.Fatal("file without doc_id must fail")
	}
	if _, err := File("r", FileInfo{DocID: "d"}); err == nil {
		t.Fatal("file without rank must fail")
	}
	if _, err := Stage("r", "", PhaseStart, 0); err == nil {
		t.Fatal("stage without name must fail")
	}
	if _, err := Stage("r", "s", StagePhase("weird"), 0); err == nil {
		t.Fatal("stage with illegal phase must fail")
	}
	if _, err := Done("r", DoneInfo{}); err == nil {
		t.Fatal("done without answer and without refusal must fail")
	}
	if _, err := Done("r", DoneInfo{Refused: true}); err != nil {
		t.Fatalf("a refusal is a legitimate done: %v", err)
	}
	if _, err := Failed("r", nil); err == nil {
		t.Fatal("error event without an error must fail")
	}
	// 不合法事件被拒时要计数，且**报错给调用方**（这是调用方的 bug，不是出口的）
	rec := NewRecorder()
	e := NewEmitter(rec)
	if err := e.Emit(Event{Kind: KindFile}); err == nil {
		t.Fatal("emitting an invalid event must report to the caller")
	}
	if h := e.Health(); h.Rejected != 1 || h.Delivered != 0 {
		t.Fatalf("rejection must be counted, not delivered: %+v", h)
	}
}

// 答案拼接：片段按序拼、replace 覆盖。
func TestRecorderAssemblesAnswer(t *testing.T) {
	rec := NewRecorder()
	e := NewEmitter(rec)
	emitAll(e, t)
	if got := rec.AnswerText(); got != "最大连接数是 100。" {
		t.Fatalf("answer assembly wrong: %q", got)
	}
	if got := rec.ReasoningText(); !strings.Contains(got, "覆盖度") {
		t.Fatalf("reasoning assembly wrong: %q", got)
	}
	// replace：整段覆盖
	ev, _ := Content("run-1", "更正：最大连接数是 200。", true)
	if err := e.Emit(ev); err != nil {
		t.Fatal(err)
	}
	if got := rec.AnswerText(); got != "更正：最大连接数是 200。" {
		t.Fatalf("replace must override: %q", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

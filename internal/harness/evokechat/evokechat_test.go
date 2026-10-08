package evokechat

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/harness"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
)

// 真跑一次流程拿到事件流，再用它验收翻译——**不造假帧**。
func liveEvents(t *testing.T) []harness.Event {
	t.Helper()
	idx := retrieval.Build([]retrieval.Document{
		{ID: "d1", Body: "专利法第一条 为了保护专利权人的合法权益，鼓励发明创造，推动发明创造的应用，制定本法。"},
		{ID: "d2", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
	})
	rec := harness.NewRecorder()
	r := qaflow.Runner("专利法是为了什么制定的？", qaflow.BM25Evidence(idx, 3, 60),
		func(_ string, ws []qaflow.EvidenceWindow, _ facts.Report) (qaflow.Answer, qaflow.Usage, error) {
			a := qaflow.Answer{Text: "为保护专利权人的合法权益而制定"}
			if len(ws) > 0 {
				a.Citations = []string{ws[0].SourceID + "#" + ws[0].Span}
			}
			return a, qaflow.Usage{}, nil
		},
		qaflow.Options{Emitter: harness.NewEmitter(rec), RunID: "run-1"})
	c := context.New("evokechat-test")
	if err := r.Run(c); err != nil {
		t.Fatal(err)
	}
	// 模拟应用层补发的终态事件（引用/关联/答案/收尾）——翻译器要能吃全。
	for _, ev := range []harness.Event{
		// 离线合成不发思考，但**消费端要能处理它**（真模型会发）——所以这里
		// 手工补一帧，翻译器的思考路径必须验到。
		must(harness.Reasoning("run-1", "先找窗口，再核对要点。")),
		must(harness.Citations("run-1", []harness.Citation{{DocID: "d1", Title: "专利法第一条", Span: "rune[0:4]", Text: "第一条 …", Resolved: true}})),
		must(harness.Related("run-1", []harness.RelatedInfo{{DocID: "d2", Title: "部署手册", Why: "retrieved-uncited", Score: 2.1}})),
		must(harness.Content("run-1", "为保护专利权人的合法权益而制定", true)),
		must(harness.Done("run-1", harness.DoneInfo{
			Answer: "为保护专利权人的合法权益而制定", PromptTokens: 120, CompletionTokens: 40,
			Committed: "corpus=a1 config=c2 strategy=qa.v0.2",
			Counts:    map[string]int{"delivered": 24, "citations": 1},
		})),
	} {
		if err := rec.Write(ev); err != nil {
			t.Fatal(err)
		}
	}
	return rec.All()
}

func must(ev harness.Event, err error) harness.Event {
	if err != nil {
		panic(err)
	}
	return ev
}

func translateAll(evs []harness.Event, opt Options) ([]Call, *State, []Result) {
	st := &State{MessageID: "m1"}
	var out []Call
	var res []Result
	for _, ev := range evs {
		r := Translate(ev, st, opt)
		out = append(out, r.Calls...)
		res = append(res, r)
	}
	return out, st, res
}

func hasOp(calls []Call, op Op) bool {
	for _, c := range calls {
		if c.Op == op {
			return true
		}
	}
	return false
}

// 整条流能翻译：开场 → 思考 → 正文 → 引用 → 关联 → 收尾，各类型都出现。
func TestTranslateCoversWholeStream(t *testing.T) {
	calls, _, res := translateAll(liveEvents(t), Options{})
	for _, want := range []Op{OpCreateAssistant, OpProgress, OpCitations, OpComplete, "related"} {
		if !hasOp(calls, want) {
			t.Fatalf("missing op %q in %d calls", want, len(calls))
		}
	}
	for _, r := range res {
		if r.FirstErr != nil {
			t.Fatalf("live stream must translate cleanly: %v", r.FirstErr)
		}
		if r.Skipped != 0 {
			t.Fatalf("nothing should be skipped: %d", r.Skipped)
		}
	}
	// 开场只发生一次（重复建消息会让 UI 出现两个助手气泡）
	opens := 0
	for _, c := range calls {
		if c.Op == OpCreateAssistant {
			opens++
		}
	}
	if opens != 1 {
		t.Fatalf("assistant message must be created exactly once: %d", opens)
	}
}

// 契约硬规则：**思考与正文必须走两个不同 API**，混写会串行渲染。
func TestThinkingAndContentUseSeparateOps(t *testing.T) {
	rec := harness.NewRecorder()
	em := harness.NewEmitter(rec)
	for _, ev := range []harness.Event{
		must(harness.Started("run-2", "问题")),
		must(harness.Reasoning("run-2", "思考第一段")),
		must(harness.Content("run-2", "答案第一段", false)),
		must(harness.Content("run-2", "答案第二段", false)),
	} {
		if err := em.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	calls, _, _ := translateAll(rec.All(), Options{})
	var think, content int
	for _, c := range calls {
		switch c.Op {
		case OpAppendThink:
			think++
		case OpAppendContent:
			content++
		}
	}
	if think != 1 || content != 2 {
		t.Fatalf("thinking/content must stay separate: think=%d content=%d", think, content)
	}
}

// 阶段进度是**瞬时**事件：evoke-chat 明确"瞬时事件不推进游标也不造成缺口"。
func TestProgressEventsAreTransient(t *testing.T) {
	evs := liveEvents(t)
	calls, _, res := translateAll(evs, Options{})
	transientCalls := 0
	for _, c := range calls {
		if c.Transient {
			transientCalls++
		}
	}
	if transientCalls == 0 {
		t.Fatal("stage/file progress must be transient")
	}
	// 承载历史的调用必须持久：引用/收尾各自至少产出一个非瞬时调用。
	// （兜底开场的 call 是**脚手架**，标瞬时是对的——它不该推进游标。）
	historyOK := map[harness.Kind]bool{}
	isHistory := func(k harness.Kind) bool {
		switch k {
		case harness.KindCitations, harness.KindDone, harness.KindContent, harness.KindReasoning:
			return true
		}
		return false
	}
	present := map[harness.Kind]bool{}
	for i, r := range res {
		kind := evs[i].Kind
		if !isHistory(kind) {
			continue
		}
		present[kind] = true
		for _, c := range r.Calls {
			if !c.Transient {
				present[kind] = present[kind] // 保持 true
				historyOK[kind] = true
			}
		}
	}
	for kind, ok := range historyOK {
		if !ok {
			t.Fatalf("%s must contribute at least one persistent call (it is part of history)", kind)
		}
	}
	if len(historyOK) < 4 {
		t.Fatalf("expected thinking/content/citations/done in the stream, got %d kinds", len(historyOK))
	}
}

// file 事件可以走进度行，也可以走**工具调用卡**（重但可回看原文）。
func TestFilesAsToolCallsIsOptIn(t *testing.T) {
	evs := liveEvents(t)
	light, _, _ := translateAll(evs, Options{})
	if hasOp(light, OpToolCall) {
		t.Fatal("default must not create tool cards")
	}
	heavy, _, res := translateAll(evs, Options{FilesAsToolCalls: true})
	if !hasOp(heavy, OpToolCall) {
		t.Fatal("opt-in mode must create a tool card per file")
	}
	// 两种口径都不能跳过帧
	for _, r := range res {
		if r.Skipped != 0 {
			t.Fatalf("tool-card mode must not skip: %d", r.Skipped)
		}
	}
}

// 阶段进度必须带 label/detail/percent（契约点名要这三个字段）。
func TestStageProgressCarriesLabelDetailPercent(t *testing.T) {
	evs := liveEvents(t)
	calls, _, _ := translateAll(evs, Options{})
	var sawEvidence bool
	for _, c := range calls {
		if c.Op != OpProgress || c.Args["stage"] != "evidence-supply" {
			continue
		}
		label, _ := c.Args["label"].(string)
		if label == "" || label == "evidence-supply" {
			t.Fatalf("progress label must be a display label, not the raw name: %v", c.Args)
		}
		detail, _ := c.Args["detail"].(string)
		if detail != "" {
			sawEvidence = true
		}
	}
	if !sawEvidence {
		t.Fatal("evidence stage must carry a detail line (what it actually did)")
	}
}

// 收尾帧必须带提交视图（这是"答案为什么变了"的对账依据）与计量。
func TestDoneCarriesCommittedAndContext(t *testing.T) {
	calls, _, _ := translateAll(liveEvents(t), Options{})
	var complete, ctx map[string]any
	for _, c := range calls {
		switch c.Op {
		case OpComplete:
			complete = c.Args
		case OpSetContext:
			ctx = c.Args
		}
	}
	if complete == nil {
		t.Fatal("stream must end with a complete call")
	}
	if committed, _ := complete["committed"].(string); !strings.Contains(committed, "corpus=") {
		t.Fatalf("done must carry the committed view: %v", complete)
	}
	if ctx == nil {
		t.Fatal("usage should surface as context usage")
	}
	// 契约：used 与 capacity 缺一不渲染 → 我们只给 used，**不许伪造 capacity**
	if _, bad := ctx["capacity"]; bad {
		t.Fatalf("do not invent a context capacity: %v", ctx)
	}
	if used, _ := ctx["used"].(int); used <= 0 {
		t.Fatalf("context usage must be positive when reported: %v", ctx)
	}
}

// 拒答是一等结局：complete(reason=…) 而不是 error。
func TestRefusalIsCompletionNotError(t *testing.T) {
	rec := harness.NewRecorder()
	em := harness.NewEmitter(rec)
	for _, ev := range []harness.Event{
		must(harness.Started("run-3", "今天天气怎么样")),
		must(harness.Done("run-3", harness.DoneInfo{Refused: true, RefusalReason: "insufficient-evidence"})),
	} {
		if err := em.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	calls, _, _ := translateAll(rec.All(), Options{})
	if hasOp(calls, OpError) {
		t.Fatal("a refusal must not be translated into an error")
	}
	var complete map[string]any
	for _, c := range calls {
		if c.Op == OpComplete {
			complete = c.Args
		}
	}
	if complete == nil || complete["refused"] != true || complete["reason"] != "insufficient-evidence" {
		t.Fatalf("refusal must carry its reason: %v", complete)
	}
}

// 截断的流（没有 started）也要能显示：翻译器兜底开场，且**只开一次**。
func TestTruncatedStreamStillOpensOnce(t *testing.T) {
	rec := harness.NewRecorder()
	em := harness.NewEmitter(rec)
	for _, ev := range []harness.Event{
		must(harness.Reasoning("run-4", "只剩思考")),
		must(harness.Content("run-4", "只剩答案", false)),
	} {
		if err := em.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	calls, st, _ := translateAll(rec.All(), Options{})
	opens := 0
	for _, c := range calls {
		if c.Op == OpCreateAssistant {
			opens++
		}
	}
	if opens != 1 || !st.Opened {
		t.Fatalf("truncated stream must open exactly once: opens=%d opened=%v", opens, st.Opened)
	}
}

// 信封：seq 必须**一条不落**地映射（消费方靠它补页/发现缺口）。
func TestEnvelopeSeqIsContinuous(t *testing.T) {
	rec := harness.NewRecorder()
	em := harness.NewEmitter(rec)
	for _, ev := range []harness.Event{
		must(harness.Started("run-5", "问题")),
		must(harness.Stage("run-5", "evidence-supply", harness.PhaseStart, 0)),
		must(harness.Content("run-5", "答案", true)),
		must(harness.Done("run-5", harness.DoneInfo{Answer: "答案"})),
	} {
		if err := em.Emit(ev); err != nil {
			t.Fatal(err)
		}
	}
	evs := rec.All()
	var lastSeq int
	for i, ev := range evs {
		env := Envelope(ev)
		seq, _ := env["seq"].(int)
		if seq != i+1 {
			t.Fatalf("envelope seq must be continuous: frame %d has seq %d", i, seq)
		}
		if env["type"] == nil || env["data"] == nil {
			t.Fatalf("envelope must carry type and data: %v", env)
		}
		lastSeq = seq
	}
	if lastSeq != len(evs) {
		t.Fatalf("envelope dropped frames: last=%d total=%d", lastSeq, len(evs))
	}
	// 阶段帧的信封类型是 assistant/progress（契约指定的瞬时事件名）
	for _, ev := range evs {
		if ev.Kind == harness.KindStage && Envelope(ev)["type"] != "assistant/progress" {
			t.Fatalf("stage envelope type wrong: %v", Envelope(ev)["type"])
		}
	}
}

// 未知/残缺帧：宽松模式跳过并**记录原因**（不静默丢），严格模式报错。
func TestBrokenFrameIsSkippedNotSwallowed(t *testing.T) {
	bad := harness.Event{Seq: 2, Kind: harness.KindFile} // 缺载荷
	st := &State{MessageID: "m", Opened: true}
	res := Translate(bad, st, Options{})
	if res.Skipped != 1 || res.FirstErr == nil {
		t.Fatalf("broken frame must be skipped with a recorded reason: %+v", res)
	}
	if len(res.Calls) != 0 {
		t.Fatalf("broken frame must not produce calls: %+v", res.Calls)
	}
	strict := Translate(bad, st, Options{Strict: true})
	if strict.FirstErr == nil || strict.Skipped != 0 {
		t.Fatalf("strict mode must report instead of skipping: %+v", strict)
	}
}

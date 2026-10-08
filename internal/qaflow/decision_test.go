package qaflow

import (
	"net/http"
	"net/http/httptest"
	"testing"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/decide"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/retrieval"
)

func demoWindows() []EvidenceWindow {
	return []EvidenceWindow{
		{SourceID: "d1", Span: "rune[0:4]", Text: "连接池最大连接数默认为 100。"},
		{SourceID: "d2", Span: "rune[4:8]", Text: "部署手册：先改配置再重启。"},
	}
}

func demoCtx() *context.Context { return context.New("decision-test") }

// 不变式 1：**缺席不改行为**——没绑决策面时必须放行、必须有留痕、且不返回错误。
func TestDecisionAbsentIsNoop(t *testing.T) {
	var d *DecisionDecider // nil：这一层没接决策面
	ok, noul, rec := d.Decide(gocontext.Background(), "answerable", "连接池最大连接数是多少", demoWindows())
	if !ok {
		t.Fatal("absent decision must pass through (behaviour unchanged)")
	}
	if noul != 0 || rec.Applied {
		t.Fatalf("absent decision must not fabricate a verdict: %+v", rec)
	}
	if rec.Reason != "not-bound" {
		t.Fatalf("absence must be recorded with a reason: %q", rec.Reason)
	}
	if rec.Source != "" || rec.LatencyMS != 0 {
		t.Fatalf("absent decision must not report a source/latency: %+v", rec)
	}
}

func okServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 不变式 3：**degraded 必须可见**——三种结局（缺席 / 失败 / 成功）留痕各不相同。
func TestDecisionRecordsEveryOutcome(t *testing.T) {
	// 成功
	srv := okServer(t, `{"answers":{"answerable":{"type":"noul","noul":0.93,"confidence":0.9}}}`)
	d := &DecisionDecider{Client: decide.New(srv.URL, "k", "m")}
	ok, noul, rec := d.Decide(gocontext.Background(), "answerable", "连接池最大连接数是多少", demoWindows())
	if !ok || noul != 0.93 || !rec.Applied || rec.Reason != "ok" {
		t.Fatalf("success path wrong: ok=%v noul=%v rec=%+v", ok, noul, rec)
	}

	// 决策说不行（低于闸门）
	srv2 := okServer(t, `{"answers":{"answerable":{"type":"noul","noul":0.2}}}`)
	d2 := &DecisionDecider{Client: decide.New(srv2.URL, "k", "m")}
	ok2, _, rec2 := d2.Decide(gocontext.Background(), "answerable", "连接池最大连接数是多少", demoWindows())
	if ok2 || !rec2.Applied || rec2.Reason != "ok" {
		t.Fatalf("a low noul must block, not error: ok=%v rec=%+v", ok2, rec2)
	}
}

// 不变式 2：**失败不阻断**——决策面挂了，答案照走，只留痕。
func TestDecisionFailureDegradesInsteadOfBlocking(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":"overloaded"}`))
	}))
	defer bad.Close()
	d := &DecisionDecider{Client: decide.New(bad.URL, "k", "m")}
	ok, noul, rec := d.Decide(gocontext.Background(), "answerable", "连接池最大连接数是多少", demoWindows())
	if !ok || noul != 0 {
		t.Fatalf("failure must degrade to pass-through: ok=%v noul=%v", ok, noul)
	}
	if rec.Applied {
		t.Fatalf("failure must not claim applied: %+v", rec)
	}
	if rec.Reason == "" || rec.Reason == "ok" {
		t.Fatalf("failure must be visible in the reason: %q", rec.Reason)
	}
}

// 留痕要能进 context（"有没有在决策"是可查询事实，不是从日志里猜的）。
func TestDecisionRecordReachesContext(t *testing.T) {
	srv := okServer(t, `{"answers":{"answerable":{"type":"noul","noul":0.8}}}`)
	d := &DecisionDecider{Client: decide.New(srv.URL, "k", "decision-model-preview")}
	c := demoCtx()
	_, _, rec := d.Decide(gocontext.Background(), "answerable", "连接池最大连接数是多少", demoWindows())
	if err := context.Set(c, KeyDecision, rec); err != nil {
		t.Fatal(err)
	}
	got, ok := DecisionRecordOf(c)
	if !ok || got.Source != "decision-model-preview" || got.Kind != "answerable" {
		t.Fatalf("record must round-trip through the context: %+v (ok=%v)", got, ok)
	}
}

// 闸门阈值：0（或越界）落回默认 0.5——**不许因为配置笔误就静默全放行**。
func TestDecisionGateThresholdFallsBackToHalf(t *testing.T) {
	srv := okServer(t, `{"answers":{"answerable":{"type":"noul","noul":0.45}}}`)
	d := &DecisionDecider{Client: decide.New(srv.URL, "k", "m"), GateThreshold: -1}
	ok, _, _ := d.Decide(gocontext.Background(), "answerable", "q", demoWindows())
	if ok {
		t.Fatal("0.45 < default gate 0.5 must block")
	}
	srv2 := okServer(t, `{"answers":{"answerable":{"type":"noul","noul":0.45}}}`)
	d2 := &DecisionDecider{Client: decide.New(srv2.URL, "k", "m"), GateThreshold: 0.4}
	ok2, _, _ := d2.Decide(gocontext.Background(), "answerable", "q", demoWindows())
	if !ok2 {
		t.Fatal("explicit 0.4 gate must pass 0.45")
	}
}

// 闸门不接进产品路径（nil）时，**stage 列表与行为都不变**——这是"可选件"
// 的全部意义：一次外部服务缺席不许改变问答结果。
func TestGateAbsentLeavesStageListUnchanged(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "d1", Body: "连接池最大连接数默认为 100，超过需调整配置。"},
		{ID: "d2", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
	})
	retrieve := BM25Evidence(idx, 3, 60)
	// 合成面必须给引用（"每个断言都要能映射回窗口"是合成阶段的纪律），
	// 否则跑不到闸门断言就会被 Verify 先拦下——测试的失败姿势也得对。
	answer := func(_ string, ws []EvidenceWindow, _ facts.Report) (Answer, Usage, error) {
		a := Answer{Text: "最大连接数是 100"}
		if len(ws) > 0 {
			a.Citations = []string{ws[0].SourceID + "#" + ws[0].Span}
		}
		return a, Usage{}, nil
	}
	// 不绑闸门
	off := Runner("连接池最大连接数是多少", retrieve, answer, Options{})
	cOff := context.New("t-off")
	if err := off.Run(cOff); err != nil {
		t.Fatal(err)
	}
	// 绑了闸门但**决策面缺席**（DecisionDecider 的 Client 为 nil）
	degraded := Runner("连接池最大连接数是多少", retrieve, answer, Options{
		Decision: &DecisionDecider{}, // Client=nil → 缺席
	})
	cDeg := context.New("t-deg")
	if err := degraded.Run(cDeg); err != nil {
		t.Fatal(err)
	}

	ansOff, _ := context.Get(cOff, KeyAnswer)
	ansDeg, _ := context.Get(cDeg, KeyAnswer)
	if ansOff.Text != ansDeg.Text || ansOff.Refused != ansDeg.Refused {
		t.Fatalf("absent gate must not change the answer: off=%+v degraded=%+v", ansOff, ansDeg)
	}
	routeDeg, _ := context.Get(cDeg, KeyRoute)
	if routeDeg.Action == "refuse" {
		t.Fatalf("absent gate must not refuse: %+v", routeDeg)
	}
	// 但缺席必须留痕（"有没有在决策"是可查询事实）
	rec, ok := DecisionRecordOf(cDeg)
	if !ok {
		t.Fatal("degraded gate must leave a record")
	}
	if rec.Applied || rec.Reason != "not-bound" {
		t.Fatalf("absence must be recorded honestly: %+v", rec)
	}
	// 不绑闸门的那次**不该**有决策留痕（压根没注册这个 stage）
	if _, ok := DecisionRecordOf(cOff); ok {
		t.Fatal("no gate configured → no decision record (stage was never registered)")
	}
}

// 闸门判不行 → 拒答且带原因（拒答是可解释结局，不是沉默）。
func TestGateRefusesWithReason(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"answerable":{"type":"noul","noul":0.1}}}`))
	}))
	defer bad.Close()
	idx := retrieval.Build([]retrieval.Document{{ID: "d1", Body: "财务报表：三季度收入增长，成本结构继续优化。"}})
	d := &DecisionDecider{Client: decide.New(bad.URL, "k", "m")}
	r := Runner("连接池最大连接数是多少", BM25Evidence(idx, 3, 60),
		func(_ string, ws []EvidenceWindow, _ facts.Report) (Answer, Usage, error) {
			a := Answer{Text: "不该被合成出来的答案"}
			if len(ws) > 0 {
				a.Citations = []string{ws[0].SourceID + "#" + ws[0].Span}
			}
			return a, Usage{}, nil
		},
		Options{Decision: d})
	c := context.New("t-refuse")
	if err := r.Run(c); err != nil {
		t.Fatal(err)
	}
	ans, _ := context.Get(c, KeyAnswer)
	if !ans.Refused || ans.Text != "" {
		t.Fatalf("gate must refuse and suppress the answer: %+v", ans)
	}
	reason, ok := context.Get(c, KeyRefusalReason)
	if !ok || reason != "decision-gate" {
		t.Fatalf("refusal must carry a reason: %q", reason)
	}
}

// 端到端：真客户端（假端点）→ 闸门判"有答案" → 放行且留痕为 applied。
// 真跑踩过：接线看起来都在（Options.Decision 有值、stage 注册了），但
// ItemResult.DecisionApplied 恒为 false——留痕没有从 flow 的 context 流到
// 结果里。这条测试就是钉这个交接点的。
func TestGateLiveClientLeavesAppliedRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth == "" {
			t.Error("decision request must carry the key")
		}
		_, _ = w.Write([]byte(`{"answers":{"answerable":{"type":"noul","noul":0.95,"confidence":0.9}}}`))
	}))
	defer srv.Close()
	idx := retrieval.Build([]retrieval.Document{
		{ID: "d1", Body: "连接池最大连接数默认为 100，超过需调整配置。"},
	})
	c := context.New("t-live")
	r := Runner("连接池最大连接数是多少", BM25Evidence(idx, 3, 60),
		func(_ string, ws []EvidenceWindow, _ facts.Report) (Answer, Usage, error) {
			a := Answer{Text: "100"}
			if len(ws) > 0 {
				a.Citations = []string{ws[0].SourceID + "#" + ws[0].Span}
			}
			return a, Usage{}, nil
		},
		Options{Decision: &DecisionDecider{Client: decide.New(srv.URL, "k", "decision-model-preview")}})
	if err := r.Run(c); err != nil {
		t.Fatal(err)
	}
	rec, ok := DecisionRecordOf(c)
	if !ok {
		t.Fatal("live gate must leave a record in the flow context")
	}
	if !rec.Applied || rec.Reason != "ok" || rec.Source != "decision-model-preview" {
		t.Fatalf("live gate record wrong: %+v", rec)
	}
	if rec.Noul < 0.9 {
		t.Fatalf("gate score must be recorded: %+v", rec)
	}
	route, _ := context.Get(c, KeyRoute)
	if route.Action == "refuse" {
		t.Fatalf("noul 0.95 must pass the gate: %+v", route)
	}
}

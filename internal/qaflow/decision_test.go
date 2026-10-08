package qaflow

import (
	"net/http"
	"net/http/httptest"
	"testing"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/decide"
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

package qaflow

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
)

// 存根取数器：直接写覆盖度信号（被测的是级联逻辑，不是 BM25 排序——
// 排序由 retrieval 的测试负责）。
func coverageStub(ws []EvidenceWindow, coverage float64) func(*context.Context, Rewrite) ([]EvidenceWindow, error) {
	return func(c *context.Context, _ Rewrite) ([]EvidenceWindow, error) {
		if err := context.Set(c, KeyCoverage, CoverageInfo{Value: coverage, Terms: []string{"部署", "端口"}}); err != nil {
			return nil, err
		}
		return ws, nil
	}
}

func win1() []EvidenceWindow {
	return []EvidenceWindow{{SourceID: "a", Span: "rune[0:2]", Text: "连接池巡检", Score: 2}}
}

func winDeep() []EvidenceWindow {
	return []EvidenceWindow{{SourceID: "ops-1", Span: "rune[0:5]", Text: "部署端口默认 8484", Score: 5}}
}

func escOpts(deep func(*context.Context, Rewrite) ([]EvidenceWindow, error)) Options {
	return Options{
		CorpusVersion: "test", ConfigVersion: "test", StrategyVersion: "test", BeliefVersion: "test",
		Escalate: deep,
	}
}

// 级联全通：快路覆盖不足 → escalate → 贵路取到真证据 → 重判 fast。
func TestEscalateResolvesWithDeepBackend(t *testing.T) {
	c := context.New("default")
	if err := Runner("q", coverageStub(win1(), 0.3), offlineStub, escOpts(coverageStub(winDeep(), 0.9))).Run(c); err != nil {
		t.Fatal(err)
	}
	esc, _ := context.Get(c, KeyEscalation)
	if !esc.Triggered || !esc.Executed {
		t.Fatalf("must trigger and execute escalation: %+v", esc)
	}
	if esc.Before != "escalate" || esc.After != "fast" {
		t.Fatalf("escalation must resolve escalate->fast: %+v", esc)
	}
	route, _ := context.Get(c, KeyRoute)
	if route.Action != "fast" || !strings.Contains(route.Reason, "resolved by escalation") {
		t.Fatalf("final route must record the resolution: %s (%s)", route.Action, route.Reason)
	}
	ws, _ := context.Get(c, KeyWindows)
	if len(ws) != 1 || ws[0].SourceID != "ops-1" {
		t.Fatalf("synthesis must see the escalated windows: %v", ws)
	}
}

// 升级后置信度仍低但取到了证据：带疑作答（不是拒答）——升级的钱已经
// 花了，再以低门槛拒答等于把钱扔掉；残余低置信度写进 reason。
func TestEscalateLowConfidenceStillAnswers(t *testing.T) {
	c := context.New("default")
	if err := Runner("q", coverageStub(win1(), 0.3), offlineStub, escOpts(coverageStub(win1(), 0.3))).Run(c); err != nil {
		t.Fatal(err)
	}
	esc, _ := context.Get(c, KeyEscalation)
	if !esc.Executed || esc.After != "fast" {
		t.Fatalf("escalation with evidence must answer: %+v", esc)
	}
	route, _ := context.Get(c, KeyRoute)
	if !strings.Contains(route.Reason, "residual low confidence") {
		t.Fatalf("residual low confidence must be recorded: %s", route.Reason)
	}
	ans, _ := context.Get(c, KeyAnswer)
	if ans.Refused {
		t.Fatal("must not refuse when escalated windows exist")
	}
}

// 贵路也什么都没取到：这才拒答（升级花过钱，但确实没有证据）。
func TestEscalateWithNoEvidenceRefuses(t *testing.T) {
	c := context.New("default")
	empty := coverageStub(nil, 0)
	if err := Runner("q", coverageStub(win1(), 0.3), offlineStub, escOpts(empty)).Run(c); err != nil {
		t.Fatal(err)
	}
	esc, _ := context.Get(c, KeyEscalation)
	if esc.After != "refuse" {
		t.Fatalf("no evidence after escalation must refuse: %+v", esc)
	}
}

// 信号够好：升级件空过，留痕（升级不是每票都走）。
func TestEscalateNoopWhenNotTriggered(t *testing.T) {
	c := context.New("default")
	if err := Runner("q", coverageStub(winDeep(), 0.95), offlineStub, escOpts(coverageStub(winDeep(), 0.95))).Run(c); err != nil {
		t.Fatal(err)
	}
	esc, _ := context.Get(c, KeyEscalation)
	if esc.Triggered || esc.Executed {
		t.Fatalf("must not escalate on good signals: %+v", esc)
	}
	if esc.Reason == "" {
		t.Fatal("no-op must carry a reason")
	}
}

// 判了 escalate 但没配执行处：死标签必须留痕（不许假装升级过）。
func TestEscalateTerminalWithoutBackend(t *testing.T) {
	c := context.New("default")
	if err := Runner("q", coverageStub(win1(), 0.3), offlineStub, escOpts(nil)).Run(c); err != nil {
		t.Fatal(err)
	}
	esc, _ := context.Get(c, KeyEscalation)
	if !esc.Triggered || esc.Executed {
		t.Fatalf("terminal escalation must be visible: %+v", esc)
	}
	if !strings.Contains(esc.Reason, "no escalation backend") {
		t.Fatalf("reason must say why terminal: %q", esc.Reason)
	}
}

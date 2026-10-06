package qaflow

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
)

// 草稿置信度从可观测信号算：覆盖度/区分度/死路率，权重公开。
func TestDraftConfidenceFromSignals(t *testing.T) {
	full := DraftConfidence(RouteSignals{Coverage: 1, Margin: 1, DeadRate: 0})
	if full < 0.99 {
		t.Fatalf("all-good signals must give ~1, got %v", full)
	}
	zero := DraftConfidence(RouteSignals{Coverage: 0, Margin: 0, DeadRate: 1})
	if zero > 0.01 {
		t.Fatalf("all-bad signals must give ~0, got %v", zero)
	}
	// 界内
	mid := DraftConfidence(RouteSignals{Coverage: 0.5, Margin: 0.5, DeadRate: 0.5})
	if mid < 0.49 || mid > 0.51 {
		t.Fatalf("mid signals must give ~0.5, got %v", mid)
	}
}

// 覆盖度低 → escalate，决策里带全部信号（可审计）。
func TestRouteEscalatesOnLowCoverage(t *testing.T) {
	c := context.New("default")
	if err := context.Set(c, KeyWindows, []EvidenceWindow{
		{SourceID: "a", Span: "rune[0:5]", Text: "无关文本", Score: 3},
		{SourceID: "b", Span: "rune[0:5]", Text: "另一段无关文本", Score: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := context.Set(c, KeyCoverage, CoverageInfo{Value: 0.1}); err != nil {
		t.Fatal(err)
	}
	if err := (RouteStage{}).Run(c); err != nil {
		t.Fatal(err)
	}
	d, _ := context.Get(c, KeyRoute)
	if d.Action != "escalate" {
		t.Fatalf("low coverage must escalate, got %s (%s)", d.Action, d.Reason)
	}
	if !strings.Contains(d.Reason, "coverage=0.100") {
		t.Fatalf("decision must carry the signal values: %s", d.Reason)
	}
}

// 没有窗口 → refuse（诚实的不知道）。
func TestRouteRefusesWithoutWindows(t *testing.T) {
	c := context.New("default")
	if err := (RouteStage{}).Run(c); err != nil {
		t.Fatal(err)
	}
	d, _ := context.Get(c, KeyRoute)
	if d.Action != "refuse" {
		t.Fatalf("no windows must refuse, got %s", d.Action)
	}
}

// 覆盖足、区分度高 → fast。
func TestRouteFastOnGoodSignals(t *testing.T) {
	c := context.New("default")
	if err := context.Set(c, KeyWindows, []EvidenceWindow{
		{SourceID: "a", Span: "rune[0:5]", Text: "命中", Score: 10},
		{SourceID: "b", Span: "rune[0:5]", Text: "次席", Score: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := context.Set(c, KeyCoverage, CoverageInfo{Value: 1}); err != nil {
		t.Fatal(err)
	}
	if err := (RouteStage{}).Run(c); err != nil {
		t.Fatal(err)
	}
	d, _ := context.Get(c, KeyRoute)
	if d.Action != "fast" {
		t.Fatalf("good signals must go fast, got %s (%s)", d.Action, d.Reason)
	}
	if d.Signals.Margin < 0.8 || d.Signals.Coverage != 1 {
		t.Fatalf("decision must record signals: %+v", d.Signals)
	}
}

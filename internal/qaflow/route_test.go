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

// 阈值不手调：RouteConfig 给基线，档位与校准程序随信号留痕
// （v0.2 §2.1——换 provider 要重校准，跨 provider 的数字才可比）。
func TestRouteConfigRecordsCalibrationAndTier(t *testing.T) {
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
	cfg := RouteConfig{UpgradeBase: 0.9, Program: ProgramCAUC, ThresholdVersion: "tau0=0.900"}
	if err := (RouteStage{Config: cfg}).Run(c); err != nil {
		t.Fatal(err)
	}
	d, _ := context.Get(c, KeyRoute)
	// 档位说实话：provider 不返回 logprobs，生效的是检索侧兜底档
	if d.Signals.Tier != TierRetrieval {
		t.Fatalf("tier must be %q (no logprobs wired), got %q", TierRetrieval, d.Signals.Tier)
	}
	if d.Signals.ConfidenceKnown {
		t.Fatal("confidence must be reported unknown when the logprob tier is unavailable")
	}
	if d.Signals.CalibrationProgram != ProgramCAUC || d.Signals.ThresholdVersion != "tau0=0.900" {
		t.Fatalf("calibration provenance must travel with the decision: %+v", d.Signals)
	}
	// 置信 ~0.97 < 0.9？不——0.97 > 0.9 走 fast；把线抬到 0.99 才该升级
	if d.Signals.Threshold != 0.9 {
		t.Fatalf("want threshold 0.9 (factsK=1), got %v", d.Signals.Threshold)
	}
	if d.Action != "fast" {
		t.Fatalf("0.97 above 0.9 must stay fast, got %s", d.Action)
	}
}

// 阈值真的生效：同一条问句，换基线换结论。
func TestRouteConfigThresholdChangesDecision(t *testing.T) {
	build := func() *context.Context {
		c := context.New("default")
		_ = context.Set(c, KeyWindows, []EvidenceWindow{
			{SourceID: "a", Span: "rune[0:5]", Text: "命中", Score: 10},
			{SourceID: "b", Span: "rune[0:5]", Text: "次席", Score: 5},
		})
		_ = context.Set(c, KeyCoverage, CoverageInfo{Value: 0.8})
		return c
	}
	loose := build()
	if err := (RouteStage{Config: RouteConfig{UpgradeBase: 0.3}}).Run(loose); err != nil {
		t.Fatal(err)
	}
	dl, _ := context.Get(loose, KeyRoute)
	tight := build()
	if err := (RouteStage{Config: RouteConfig{UpgradeBase: 0.99}}).Run(tight); err != nil {
		t.Fatal(err)
	}
	dt, _ := context.Get(tight, KeyRoute)
	if dl.Action != "fast" || dt.Action != "escalate" {
		t.Fatalf("threshold must drive the decision: loose=%s tight=%s", dl.Action, dt.Action)
	}
}

// CAUC：τ₀ 取强臂校准准确率，钳在 [0,1]。
func TestTau0FromDeepAccuracy(t *testing.T) {
	if got := Tau0FromDeepAccuracy(0.883); got != 0.883 {
		t.Fatalf("tau0 must equal the strong-arm accuracy, got %v", got)
	}
	if got := Tau0FromDeepAccuracy(-1); got != 0 {
		t.Fatalf("negative accuracy clamps to 0, got %v", got)
	}
	if got := Tau0FromDeepAccuracy(1.5); got != 1 {
		t.Fatalf("accuracy above 1 clamps to 1, got %v", got)
	}
}

// 提交视图记的是发生额：路由跑完才知道的档位/程序/阈值由 ViewHook 填。
func TestCommittedViewRecordsCalibration(t *testing.T) {
	c := context.New("tenant_x")
	o := opts()
	o.Route = RouteConfig{UpgradeBase: 0.7, Program: ProgramCAUC, ThresholdVersion: "tau0=0.700"}
	if err := Runner("q", stubRetrieve, offlineStub, o).Run(c); err != nil {
		t.Fatalf("run: %v", err)
	}
	views := c.Views()
	if len(views) != 1 {
		t.Fatalf("want 1 view, got %d", len(views))
	}
	cal := views[0].Calibration
	if cal.Tier != TierRetrieval || cal.Program != ProgramCAUC || cal.ThresholdVersion != "tau0=0.700" {
		t.Fatalf("calibration provenance missing from committed view: %+v", cal)
	}
	if cal.Threshold != 0.7 {
		t.Fatalf("want recorded threshold 0.7, got %v", cal.Threshold)
	}
}

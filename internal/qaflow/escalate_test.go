package qaflow

import (
	"errors"

	gocontext "context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/query"
)

// 存根取数器：直接写覆盖度信号（被测的是级联逻辑，不是 BM25 排序——
// 排序由 retrieval 的测试负责）。
// fnExpander 把函数适配成 query.Expander（测试替身）。
type fnExpander func(gocontext.Context, string) ([]string, error)

func (f fnExpander) Expand(ctx gocontext.Context, q string) ([]string, error) { return f(ctx, q) }

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
	// 升级是**合并**不是替换：贵路窗必须进最终窗集（首程窗不丢——再问
	// 加深时首程刚逐事实取回的证据，一替换就白取了）
	if !hasWindow(ws, "ops-1") {
		t.Fatalf("synthesis must see the escalated window: %v", ws)
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

// 词汇鸿沟桥：升级时关键词级稀薄 → 先扩展（口语翻书面）→ 加权重取。
func TestEscalateExpandsVocabularyGap(t *testing.T) {
	// 快路：主级稀薄（几乎全是语料外词），覆盖低 → escalate
	analysis := query.Analysis{
		Intent:  "search",
		Primary: map[string]float64{"饲养": 1},
		OOV:     []string{"闯红", "灯怎", "么处"},
		Score:   1,
	}
	var expandedWith []string
	weightedCalled := false
	optsEscalate := &EscalateStage{
		Retrieve: coverageStub(win1(), 0.3), // 普通贵路（不该被走到）
		Expand: fnExpander(func(_ gocontext.Context, q string) ([]string, error) {
			expandedWith = append(expandedWith, q)
			return []string{"饲养动物", "噪声"}, nil
		}),
		Weighted: func(weights map[string]float64) ([]EvidenceWindow, error) {
			weightedCalled = true
			// 扩展词按索引口径拆成二元组后着重（整词进不了倒排）
			for _, bg := range []string{"饲养", "养动", "动物"} {
				if weights[bg] != 2.0 {
					t.Fatalf("拆开的扩展词应着重(2.0)，got %v", weights)
				}
			}
			// 原主级降为边注
			if weights["饲养"] != 2.0 {
				t.Fatal("原主级已被扩展词覆盖")
			}
			return winDeep(), nil
		},
	}
	// 手工驱动 stage（不走 Runner，专注级联逻辑）
	c := context.New("default")
	_ = context.Set(c, KeyRewrite, Rewrite{Original: "养狗叫得太吵谁管"})
	_ = context.Set(c, KeyAnalysis, analysis)
	_ = context.Set(c, KeyRoute, RouteDecision{Action: "escalate"})
	_ = context.Set(c, KeyWindows, win1())
	if err := optsEscalate.Run(c); err != nil {
		t.Fatal(err)
	}
	if !weightedCalled {
		t.Fatal("稀薄时必须走加权重取（桥）")
	}
	if len(expandedWith) != 1 || expandedWith[0] != "养狗叫得太吵谁管" {
		t.Fatalf("扩展应拿到原问，got %v", expandedWith)
	}
	ws, _ := context.Get(c, KeyWindows)
	if !hasWindow(ws, "ops-1") {
		t.Fatalf("桥的窗口必须进 context（合并），got %v", ws)
	}
}

// 桥失败不阻塞：扩展报错 → 退化普通贵路（流程继续，不搞死）。
func TestEscalateBridgeFailureFallsBack(t *testing.T) {
	analysis := query.Analysis{Primary: map[string]float64{}, Score: 0}
	stage := EscalateStage{
		Retrieve: coverageStub(win1(), 0.3),
		Expand:   fnExpander(func(gocontext.Context, string) ([]string, error) { return nil, errors.New("llm down") }),
		Weighted: func(map[string]float64) ([]EvidenceWindow, error) {
			t.Fatal("扩展失败时不该走加权路")
			return nil, nil
		},
	}
	c := context.New("default")
	_ = context.Set(c, KeyRewrite, Rewrite{Original: "养狗叫得太吵谁管"})
	_ = context.Set(c, KeyAnalysis, analysis)
	_ = context.Set(c, KeyRoute, RouteDecision{Action: "escalate"})
	_ = context.Set(c, KeyWindows, win1())
	if err := stage.Run(c); err != nil {
		t.Fatalf("桥失败必须退化而不是失败: %v", err)
	}
	ws, _ := context.Get(c, KeyWindows)
	if len(ws) != 1 {
		t.Fatalf("贵路窗口必须在场，got %v", ws)
	}
}

// hasWindow 窗集里有没有这个源的窗。
func hasWindow(ws []EvidenceWindow, sourceID string) bool {
	for _, w := range ws {
		if w.SourceID == sourceID {
			return true
		}
	}
	return false
}

// 升级重判不许丢校准来源：档位/程序/阈值版本是装配的事实，不是窗口的
// 函数。丢过一版——凡走过升级的题，响应与提交视图里的 calibration 全空
// （serve 实测抓到），等于这次判定来历不明。
func TestEscalationKeepsCalibrationProvenance(t *testing.T) {
	c := context.New("default")
	o := escOpts(coverageStub(winDeep(), 0.9))
	o.Route = RouteConfig{UpgradeBase: 0.95, Program: ProgramCAUC, ThresholdVersion: "tau0=0.950"}
	if err := Runner("q", coverageStub(win1(), 0.3), offlineStub, o).Run(c); err != nil {
		t.Fatal(err)
	}
	route, _ := context.Get(c, KeyRoute)
	if route.Signals.Tier != TierRetrieval {
		t.Fatalf("tier lost after escalation: %+v", route.Signals)
	}
	if route.Signals.CalibrationProgram != ProgramCAUC || route.Signals.ThresholdVersion != "tau0=0.950" {
		t.Fatalf("calibration provenance lost after escalation: %+v", route.Signals)
	}
	if route.Signals.Threshold != 0.95 {
		t.Fatalf("threshold lost after escalation: %v", route.Signals.Threshold)
	}
	views := c.Views()
	if len(views) != 1 || views[0].Calibration.Program != ProgramCAUC {
		t.Fatalf("committed view must keep the calibration of an escalated answer: %+v", views)
	}
}

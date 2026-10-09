package qaflow

import (
	"errors"

	gocontext "context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
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
		Weighted: func(_ *context.Context, weights map[string]float64) ([]EvidenceWindow, error) {
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
		Weighted: func(*context.Context, map[string]float64) ([]EvidenceWindow, error) {
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

// 桥失手不得被当成"语料里没有证据"。
//
// 真跑踩过：单文档语料 margin=0 必升级，而 LLM 词汇桥一扩偏，加权重取就 0 命中；
// 系统据此拒答——可 facts 明明显示首程有支撑、得分 12.8。加权 0 命中时退回朴素
// 贵路（桥是增强，它失手不该等于系统失忆）。
func TestEscalateFallsBackWhenWeightedRetrievalMisses(t *testing.T) {
	weightedCalled := 0
	opts := Options{
		CorpusVersion: "test", ConfigVersion: "test", StrategyVersion: "test", BeliefVersion: "test",
		// 首程覆盖 0.3 → 必升级
		Escalate: coverageStub(winDeep(), 0.9), // 朴素贵路：真能取到窗
		Expander: &stubExpander{terms: []string{"完全无关的扩展词"}},
		WeightedRetrieve: func(*context.Context, map[string]float64) ([]EvidenceWindow, error) {
			weightedCalled++
			return nil, nil // 桥扩偏：加权重取一条都取不到
		},
	}
	c := context.New("esc-fallback")
	if err := Runner("q", coverageStub(win1(), 0.3), offlineStub, opts).Run(c); err != nil {
		t.Fatal(err)
	}
	if weightedCalled == 0 {
		t.Fatal("weighted retrieval should have been tried first")
	}
	esc, _ := context.Get(c, KeyEscalation)
	if !esc.Executed {
		t.Fatalf("escalation must run: %+v", esc)
	}
	if esc.After == "refuse" {
		t.Fatalf("加权 0 命中退回朴素贵路后不该拒答（桥失手≠没证据）: %+v", esc)
	}
}

// stubExpander 是固定返回的桥（真桥要 LLM）。
type stubExpander struct{ terms []string }

func (s *stubExpander) Expand(_ gocontext.Context, _ string) ([]string, error) {
	return s.terms, nil
}

// 桥必须**证明自己有用**：首窗分明显低于朴素路 → 弃用桥、回到朴素结果。
//
// 为什么这条护栏不可省：桥扩偏时**不会返回 0 命中**——它会返回"很自信的噪声窗口"
// （真跑教训："养狗叫得太吵"扩出噪声，窗口被拉去太湖流域管理条例）。上一轮那条
// "加权 0 命中才退回"的兜底抓不住这种情况，只有与原问直接对照抓得住。
func TestEscalateRejectsBridgeThatScoresWorse(t *testing.T) {
	analysis := query.Analysis{Intent: "search", Primary: map[string]float64{"饲养": 1}, OOV: []string{"闯红"}, Score: 1}
	noise := []EvidenceWindow{{SourceID: "noise-1", Span: "rune[0:2]", Text: "完全无关的噪声", Score: 0.2}}
	good := []EvidenceWindow{{SourceID: "ops-1", Span: "rune[0:2]", Text: "养狗管理办法", Score: 9}}

	c := context.New("bridge-score")
	stage := &EscalateStage{
		Retrieve: coverageStub(good, 0.9), // 朴素路：好窗口
		Expand:   fnExpander(func(gocontext.Context, string) ([]string, error) { return []string{"饲养动物"}, nil }),
		Weighted: func(*context.Context, map[string]float64) ([]EvidenceWindow, error) { return noise, nil }, // 桥把方向带偏
	}
	_ = context.Set(c, KeyRewrite, Rewrite{Original: "养狗叫得太吵"})
	_ = context.Set(c, KeyAnalysis, analysis)
	_ = context.Set(c, KeyRoute, RouteDecision{Action: "escalate"})
	_ = context.Set(c, KeyWindows, win1())
	_ = context.Set(c, KeyEscalation, EscalationRecord{Triggered: true})

	if err := stage.Run(c); err != nil {
		t.Fatal(err)
	}
	ws, _ := context.Get(c, KeyWindows)
	if !hasWindow(ws, "ops-1") {
		t.Fatalf("桥分更低时应保留朴素路的好窗口，got %v", ws)
	}
	if hasWindow(ws, "noise-1") {
		t.Fatalf("桥的噪声窗口不该进最终窗集：%v", ws)
	}
	rec, _ := context.Get(c, KeyEscalation)
	if rec.Bridge != "rejected-score" {
		t.Fatalf("bridge outcome must be observable: %+v", rec)
	}
}

// 反面：桥确实带来更好的首窗 → 采纳桥（护栏不许把好桥毙掉）。
func TestEscalateKeepsBridgeWhenItWins(t *testing.T) {
	analysis := query.Analysis{Intent: "search", Primary: map[string]float64{"饲养": 1}, OOV: []string{"闯红"}, Score: 1}
	plain := []EvidenceWindow{{SourceID: "plain-1", Span: "rune[0:2]", Text: "一般窗口", Score: 2}}
	bridged := []EvidenceWindow{{SourceID: "ops-1", Span: "rune[0:2]", Text: "养狗管理办法", Score: 11}}

	c := context.New("bridge-good")
	stage := &EscalateStage{
		Retrieve: func(*context.Context, Rewrite) ([]EvidenceWindow, error) { return plain, nil },
		Expand:   fnExpander(func(gocontext.Context, string) ([]string, error) { return []string{"饲养动物"}, nil }),
		Weighted: func(*context.Context, map[string]float64) ([]EvidenceWindow, error) { return bridged, nil },
	}
	_ = context.Set(c, KeyRewrite, Rewrite{Original: "养狗叫得太吵"})
	_ = context.Set(c, KeyAnalysis, analysis)
	_ = context.Set(c, KeyRoute, RouteDecision{Action: "escalate"})
	_ = context.Set(c, KeyWindows, win1())
	_ = context.Set(c, KeyEscalation, EscalationRecord{Triggered: true})

	if err := stage.Run(c); err != nil {
		t.Fatal(err)
	}
	ws, _ := context.Get(c, KeyWindows)
	if !hasWindow(ws, "ops-1") {
		t.Fatalf("bridge window must survive when it scores better: %v", ws)
	}
	rec, _ := context.Get(c, KeyEscalation)
	if rec.Bridge != "used" {
		t.Fatalf("bridge outcome must be recorded as used: %+v", rec)
	}
}

// 护栏本身要**可消融**（CUMULUS_BRIDGE_GUARD=0）：不能验证的护栏不如没有。
// 同一份输入，关护栏后噪声窗口应当进入最终窗集（证明开关真的关掉了它）。
func TestBridgeGuardIsAblatable(t *testing.T) {
	analysis := query.Analysis{Intent: "search", Primary: map[string]float64{"饲养": 1}, OOV: []string{"闯红"}, Score: 1}
	noise := []EvidenceWindow{{SourceID: "noise-1", Span: "rune[0:2]", Text: "完全无关的噪声", Score: 0.2}}
	good := []EvidenceWindow{{SourceID: "ops-1", Span: "rune[0:2]", Text: "养狗管理办法", Score: 9}}

	run := func(guard bool) bool {
		if guard {
			t.Setenv("CUMULUS_BRIDGE_GUARD", "1")
		} else {
			t.Setenv("CUMULUS_BRIDGE_GUARD", "0")
		}
		c := context.New("ablate")
		stage := &EscalateStage{
			Retrieve: coverageStub(good, 0.9),
			Expand:   fnExpander(func(gocontext.Context, string) ([]string, error) { return []string{"饲养动物"}, nil }),
			Weighted: func(*context.Context, map[string]float64) ([]EvidenceWindow, error) { return noise, nil },
		}
		_ = context.Set(c, KeyRewrite, Rewrite{Original: "养狗叫得太吵"})
		_ = context.Set(c, KeyAnalysis, analysis)
		_ = context.Set(c, KeyRoute, RouteDecision{Action: "escalate"})
		_ = context.Set(c, KeyWindows, win1())
		_ = context.Set(c, KeyEscalation, EscalationRecord{Triggered: true})
		if err := stage.Run(c); err != nil {
			t.Fatal(err)
		}
		ws, _ := context.Get(c, KeyWindows)
		return hasWindow(ws, "noise-1")
	}

	if run(true) {
		t.Fatal("with guard on, the noise window must not survive")
	}
	if !run(false) {
		t.Fatal("with guard off, the bridge result must be accepted (ablation ineffective)")
	}
}

// 零窗口的两种处置：**默认直接拒答**（今天的语义），开了开关才"先升级"。
//
// 为什么要有这个开关：词面全落空时先拒答，等于在唯一为这种情况造的机制（贵路 +
// 词汇桥）上场之前就放弃——真跑发现桥因此一次都走不到。但"先升级"也可能把"语料里
// 真的没有"拖成多花一次钱才拒答。两种都说得通，所以做成开关、先量再定。
func TestZeroWindowRoutingModes(t *testing.T) {
	noWindows := func(*context.Context, Rewrite) ([]EvidenceWindow, error) { return nil, nil }
	// 合成面替身：零窗口时**直接拒答**而不是报错（真实链路的拒答是合成阶段
	// 的语义，不是异常；这里只是让测试的注意力落在路由动作上）。
	tolerant := func(_ string, ws []EvidenceWindow, _ facts.Report) (Answer, Usage, error) {
		if len(ws) == 0 {
			return Answer{Refused: true}, Usage{}, nil
		}
		return Answer{Text: "有答案"}, Usage{}, nil
	}

	// 默认：零窗口 → 拒答（不发合成）
	c1 := context.New("zero-default")
	r1 := Runner("q", noWindows, tolerant, Options{Escalate: noWindows})
	if err := r1.Run(c1); err != nil {
		t.Fatal(err)
	}
	route1, _ := context.Get(c1, KeyRoute)
	if route1.Action != "refuse" {
		t.Fatalf("default behaviour must stay refuse-on-zero-window: %+v", route1)
	}

	// 开关开 + 有升级执行处 → 先升级
	c2 := context.New("zero-escalate")
	r2 := Runner("q", noWindows, tolerant, Options{
		Escalate: noWindows,
		Route:    RouteConfig{ZeroWindowEscalate: true},
	})
	if err := r2.Run(c2); err != nil {
		t.Fatal(err)
	}
	route2, _ := context.Get(c2, KeyRoute)
	esc2, _ := context.Get(c2, KeyEscalation)
	if esc2.Triggered != true {
		t.Fatalf("zero-window escalate mode must trigger escalation: %+v (route=%+v)", esc2, route2)
	}

	// 开关开但**没有升级执行处** → 仍然拒答（先升级是空转）
	c3 := context.New("zero-nobackend")
	r3 := Runner("q", noWindows, tolerant, Options{
		Route: RouteConfig{ZeroWindowEscalate: true},
	})
	if err := r3.Run(c3); err != nil {
		t.Fatal(err)
	}
	route3, _ := context.Get(c3, KeyRoute)
	if route3.Action != "refuse" {
		t.Fatalf("no escalate backend must still refuse: %+v", route3)
	}
}

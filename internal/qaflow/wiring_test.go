package qaflow

import (
	"strconv"
	"strings"
	"testing"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/harness"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
)

// 本文件是**接线门禁**：把"某个可选能力装了却不工作"从"读数里看着像在工作"变成
// 一条红。
//
// 为什么需要它：真跑里连续三次撞上这一类——
//
//	1. escalateFn 只在级联臂装配 → 单臂"升级触发了 10 次、桥一次没走"
//	2. traceFunc 的早退 `if em == nil { return nil }` → 把窗口分级器一起挡掉
//	   （读数是 enabled=true, applied=false，极具欺骗性）
//	3. 结局写进 EscalationRecord，而记录是升级**之后**才写的 → 结局恒空
//
// 三次都**不是逻辑错，是接线错**，而且都在测试之外、只在真跑里才显形。门禁的做法：
// **把所有可选面一次全开，跑一遍，然后逐个断言它真的产生了效果**——任何一个"装了
// 但没生效"，这里就红。
//
// 纪律：这道门禁只能加"**真能观测到效果**"的面（事件/挂点/记录）。加不进断言的面
// 不写进来——**不能验证的面不进这道门**，否则门禁自己开始说谎。

// wireDoc 是接线门禁用的三篇文档（金标 = d1）。
func wireDoc() *retrieval.Index {
	return retrieval.Build([]retrieval.Document{
		{ID: "d1", Body: "专利法第一条 为了保护专利权人的合法权益，鼓励发明创造，制定本法。"},
		{ID: "d2", Body: "专利制度历史 专利制度可追溯到中世纪特权制度。"},
	})
}

// wireSynth 是接线门禁的合成面（带引用，过合成阶段的 Verify）。
func wireSynth(_ string, ws []EvidenceWindow, _ facts.Report) (Answer, Usage, error) {
	a := Answer{Text: "为保护专利权人的合法权益"}
	if len(ws) > 0 {
		a.Citations = []string{ws[0].SourceID + "#" + ws[0].Span}
	}
	return a, Usage{}, nil
}

// wireLLM 是**按提示词里的实际窗口数**生成合法回包的假分级器。
//
// 为什么不能写死回包：解析器对漏标/越界一律整体失败（这是对的），而"提示里有几条
// 窗口"取决于检索——写死的回包会随机踩到校验，接线门禁就变成一个**测自己错的测试**。
type wireLLM struct{ text string } // text 非空时直接返回（给不分类的调用用）

func (w wireLLM) Complete(_ gocontext.Context, req llm.Request) (llm.Response, error) {
	if w.text != "" {
		return llm.Response{Text: w.text}, nil
	}
	n := 0
	for _, line := range strings.Split(req.Prompt, "\n") {
		if i := strings.Index(line, "["); i >= 0 {
			if k := strings.Index(line[1:], "]"); k > 0 {
				if _, err := strconv.Atoi(line[i+1 : i+1+k]); err == nil {
					n++
				}
			}
		}
	}
	if n == 0 {
		return llm.Response{Text: `{"ANSWER":[],"RELATED":[],"OUTDATED":[],"UNKNOWN":[]}`}, nil
	}
	out := `{"ANSWER":[1],"RELATED":[`
	for i := 2; i <= n; i++ {
		if i > 2 {
			out += ","
		}
		out += strconv.Itoa(i)
	}
	return llm.Response{Text: out + `],"OUTDATED":[],"UNKNOWN":[]}`}, nil
}

// 所有可选面**一次全开**，逐个断言它们真的产生了效果。
//
// 这是本轮最贵的一次教训换来的：三个能力都"装配成功"、都"enabled"，但其中两个
// 从未真正执行，而**单看读数完全看不出来**。
func TestEveryOptionalCapabilityActuallyRuns(t *testing.T) {
	idx := wireDoc()
	rec := harness.NewRecorder()
	triggered := false

	opts := Options{
		CorpusVersion: "w", ConfigVersion: "w", StrategyVersion: "w", BeliefVersion: "none",
		Emitter:  harness.NewEmitter(rec),
		RunID:    "wire",
		Analyzer: analyzerForWire(idx),
		// 1) 窗口分级：产出挂点 + file 帧带 class
		WindowClassifier: &WindowClassifier{Client: wireLLM{
			text: `{"ANSWER":[1],"RELATED":[2],"OUTDATED":[],"UNKNOWN":[]}`,
		}},
		// 2) 桥：需要 expander + 加权重取 + 有升级执行处
		Expander: wireExpander{terms: []string{"权益"}},
		WeightedRetrieve: func(map[string]float64) ([]EvidenceWindow, error) {
			return BM25Evidence(idx, 2, 80)(context.New("weighted"), Rewrite{Original: "权益"})
		},
		// 3) 升级执行处：判升级时它必须真的被调到
		Escalate: func(c *context.Context, rw Rewrite) ([]EvidenceWindow, error) {
			triggered = true
			return BM25Evidence(idx, 2, 80)(c, rw)
		},
		// 4) 零窗口先升级（开关）
		Route: RouteConfig{ZeroWindowEscalate: true, UpgradeBase: 0.99}, // 高门槛 → 必升级
	}
	c := context.New("wiring")
	if err := Runner("专利权人的合法权益是什么", BM25Evidence(idx, 2, 80), wireSynth, opts).Run(c); err != nil {
		t.Fatal(err)
	}

	// ① 事件出口：阶段 + 窗口帧都要有
	if rec.All() == nil {
		t.Fatal("emitter set but no events recorded")
	}
	var fileFrames int
	for _, ev := range rec.All() {
		if ev.Kind == harness.KindFile {
			fileFrames++
		}
	}
	if fileFrames == 0 {
		t.Fatal("emitter set but no file frames (证据阶段没发窗口)")
	}

	// ② 升级执行处：真被调用过
	if !triggered {
		t.Fatal("escalate backend wired but never invoked")
	}

	// ③ 窗口分级：挂点有值 **且** file 帧带类别
	res, ok := WindowClassOf(c)
	if !ok || !res.Ran || len(res.Classes) == 0 {
		t.Fatalf("window classifier wired but produced nothing (outcome=%q ran=%v)", res.Outcome, res.Ran)
	}
	_ = res.Classes
	var classed int
	for _, ev := range rec.All() {
		if ev.Kind == harness.KindFile && ev.File.Class != "" {
			classed++
		}
	}
	if classed == 0 {
		t.Fatal("classes exist but file frames carry none (分级结果没进事件)")
	}

	// ④ 桥的结局**要么**被记录 **要么**明确跳过——不允许"静默"
	esc, _ := context.Get(c, KeyEscalation)
	if esc.Bridge == "" && esc.Executed {
		t.Fatal("escalation ran but bridge outcome is empty (静默失败：读数里分不出'没走桥'与'走了没记')")
	}
	// ⑤ 分级结局必须**在挂点上**（ran + outcome），不寄生在别人的记录里——
	// 本轮踩了两次的坑，这行就是它的机械保证。
	if res.Outcome == "" {
		t.Fatal("classification outcome must always be written (ok/failed/skipped)，空 = 静默")
	}
}

// **缺席也算一种效果**：什么都不开时，可选面必须**真的什么都不做**（不留残迹）。
func TestNoOptionalCapabilityLeavesTraces(t *testing.T) {
	idx := wireDoc()
	c := context.New("bare-wiring")
	r := Runner("专利权人的合法权益是什么", BM25Evidence(idx, 2, 80), wireSynth,
		Options{CorpusVersion: "w", ConfigVersion: "w", StrategyVersion: "w", BeliefVersion: "none"})
	if err := r.Run(c); err != nil {
		t.Fatal(err)
	}
	if res, ok := WindowClassOf(c); ok && (res.Ran || len(res.Classes) > 0) {
		t.Fatalf("no classifier wired → no classification result may exist: %+v", res)
	}
	esc, ok := context.Get(c, KeyEscalation)
	if ok && esc.Bridge != "" {
		t.Fatalf("no bridge wired → bridge outcome must stay empty: %q", esc.Bridge)
	}
}

func analyzerForWire(idx *retrieval.Index) func(string) query.Analysis {
	return func(q string) query.Analysis { return query.Analyze(q, idx, idx.N) }
}

type wireExpander struct{ terms []string }

func (w wireExpander) Expand(_ gocontext.Context, _ string) ([]string, error) { return w.terms, nil }

package qaflow

import (
	"errors"
	"testing"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
)

// 流程级：Prior 开时，长法律（答案所在，命中深）必须排在短解释前面。
// 真跑教训：问"专利期限多少年"，BM25 把最高法的短解释置顶（26），
// 答案在专利法。prior 融合后该反过来。
func TestPriorReranksLongLawAboveShortDoc(t *testing.T) {
	// 长法律：专利 出现多次，期限（利期）出现多次，标题含专利
	longBody := "中华人民共和国专利法\n"
	for i := 0; i < 12; i++ {
		longBody += "第十条　　专利的申请与审批依照本条例执行，专利的权利要求书应当说明发明内容。\n"
	}
	longBody += "第四十二条　　发明专利权的期限为二十年，实用新型专利权的期限为十年，均自申请日起计算。\n"
	// 短解释：专利 出现 2 次，无期限内容
	shortBody := "最高人民法院关于专利代理管理的若干规定\n本规定所称专利代理机构是指依法设立的专利代理服务机构，专利代理师应当取得职业资格。"
	idx := retrieval.Build([]retrieval.Document{
		{ID: "long", Body: longBody},
		{ID: "short", Body: shortBody},
	})
	q := "专利期限多少年"
	var windows []EvidenceWindow
	retrieve := func(c *context.Context, _ Rewrite) ([]EvidenceWindow, error) {
		hits := retrieveWeighted(c, idx, q, 4, 240)
		for _, h := range hits {
			windows = append(windows, EvidenceWindow{SourceID: h.DocID, Span: h.SpanCoord, Text: h.SpanText, Score: h.Score})
		}
		return windows, nil
	}
	// 手工建 context 驱动（不走 Runner 全链——本轮只证 prior 换序）
	c := context.New("default")
	_ = context.Set(c, KeyPriorOn, true)
	an := query.Analyze(q, idx, idx.N)
	_ = context.Set(c, KeyAnalysis, an)
	_ = context.Set(c, KeyRewrite, Rewrite{Original: q})
	ws, err := retrieve(c, Rewrite{Original: q})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) == 0 {
		t.Fatal("no windows")
	}
	if ws[0].SourceID != "long" {
		t.Fatalf("prior 开时长法律必须置顶，got %s", ws[0].SourceID)
	}
}

// Runner 级：Options.Prior=true 必须让 prior 真跑（跑完 context 里有
// KeyPrior 信号）。上一版只在 stage 级手搓 context 测过，Options→stage
// 的传播漏测——线上 -prior 开了却没生效，这条测试钉死整条链。
func TestRunnerPlumbsPriorOption(t *testing.T) {
	longBody := "中华人民共和国专利法\n"
	for i := 0; i < 12; i++ {
		longBody += "第十条　　专利的申请与审批依照本条例执行。\n"
	}
	longBody += "第四十二条　　发明专利权的期限为二十年。\n"
	idx := retrieval.Build([]retrieval.Document{
		{ID: "long", Body: longBody},
		{ID: "short", Body: "最高人民法院关于专利代理的若干规定\n本规定所称专利代理机构依法设立。"},
	})
	q := "专利期限多少年"
	retrieve := func(c *context.Context, _ Rewrite) ([]EvidenceWindow, error) {
		// evidence 直接用加权+prior 的真实检索（与 BM25Evidence 同路）
		hits := retrieveWeighted(c, idx, q, 4, 240)
		out := make([]EvidenceWindow, 0, len(hits))
		for _, h := range hits {
			out = append(out, EvidenceWindow{SourceID: h.DocID, Title: h.Title, Span: h.SpanCoord, Text: h.SpanText, Score: h.Score})
		}
		return out, nil
	}
	synth := func(_ string, ws []EvidenceWindow) (Answer, Usage, error) {
		if len(ws) == 0 {
			return Answer{}, Usage{}, errors.New("no windows")
		}
		return Answer{Text: ws[0].Text, Citations: []string{ws[0].SourceID + "#" + ws[0].Span}}, Usage{CostKnown: false}, nil
	}
	r := Runner(q, retrieve, synth, Options{
		Analyzer: func(qq string) query.Analysis { return query.Analyze(qq, idx, idx.N) },
		Prior:    true,
	})
	c := context.New("default")
	if err := r.Run(c); err != nil {
		t.Fatal(err)
	}
	ps, ok := context.Get(c, KeyPrior)
	if !ok || len(ps) == 0 {
		t.Fatal("Options.Prior=true 必须让 prior 跑出信号（KeyPrior 有值）")
	}
}

// 升级路径必须带标题：deep 回填的窗口与快路同权（曾经 deep 的
// Hit→Window 转换丢了 Title，升级后全程无标题——真跑踩过第三次同型
// 转换蒸发：evict 一次、deep 一次）。这条测试从 escalate 的取数函数
// 走到底，钉死 Title 过桥。
func TestEscalateCarriesWindowTitles(t *testing.T) {
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law", Body: "中华人民共和国甲法\n第一条 甲法规定连接池默认为一百，超过需调整。"},
	})
	stage := EscalateStage{
		Retrieve: func(_ *context.Context, _ Rewrite) ([]EvidenceWindow, error) {
			// 真深路：走 BM25DeepEvidence（与 serve 同一条）
			return BM25DeepEvidence(idx, 160, DeepOptions{MaxRounds: 2, CoverageTarget: 1.0})(
				context.New("default"), Rewrite{Original: "连接池默认多少"})
		},
	}
	c := context.New("default")
	_ = context.Set(c, KeyRewrite, Rewrite{Original: "连接池默认多少"})
	_ = context.Set(c, KeyRoute, RouteDecision{Action: "escalate"})
	if err := stage.Run(c); err != nil {
		t.Fatal(err)
	}
	ws, _ := context.Get(c, KeyWindows)
	if len(ws) == 0 {
		t.Fatal("深路必须有窗口")
	}
	for _, w := range ws {
		if w.Title == "" {
			t.Fatalf("升级路径的窗口必须带标题（%s 丢了）", w.SourceID[:min(8, len(w.SourceID))])
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

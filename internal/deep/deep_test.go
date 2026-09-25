package deep

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

func srcs() []source.Source {
	long := strings.Repeat("填充无关 padding padding padding。\n", 30) +
		"关键配置：连接池最大 128，超时 30 秒。\n" +
		strings.Repeat("填充无关 padding padding padding。\n", 30)
	thin := strings.Repeat("完全无关的天气描述。", 80)
	return []source.Source{
		source.New("手册", "md", "file://m", "m", "zh", long, nil),
		source.New("杂记", "md", "file://n", "n", "zh", thin, nil),
	}
}

func newEngine() *Engine {
	fe := fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	return New(k, NewMemoryConflict())
}

// thin evidence must escalate to DEEP.
func TestGateDEscalateOnLowConfidence(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	// Unrelated query against thin corpus → thin confidence → DEEP.
	only := []source.Source{source.New("薄", "md", "", "t", "zh", strings.Repeat("无关文本。", 40), nil)}
	res, err := e.Ask(ctx, "连接池最大连接数是多少", only)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated {
		t.Fatalf("low confidence must escalate, got %+v", res)
	}
	if res.Mode != ModeDEEP {
		t.Fatalf("mode=%s want DEEP", res.Mode)
	}
	if res.Loops < 1 {
		t.Fatalf("DEEP must run at least one loop, got %d", res.Loops)
	}
}

// Solid FAST hit must NOT escalate.
func TestSolidFASTDoesNotEscalate(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	res, err := e.Ask(ctx, "连接池最大连接数是多少", srcs())
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated {
		t.Fatalf("solid evidence must stay FAST: conf=%v", res.Answer.Confidence)
	}
	if res.Mode != ModeFAST {
		t.Fatalf("mode=%s", res.Mode)
	}
}

// citations resolve back to source offsets.
func TestGateDCitationsResolve(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	res, err := e.Ask(ctx, "连接池最大连接数是多少", srcs())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Citations.Refs) == 0 {
		t.Fatal("want citations")
	}
	src := res.Answer.SourceID
	body := ""
	for _, s := range srcs() {
		if s.ID == src {
			body = s.Body
		}
	}
	runes := []rune(body)
	for _, r := range res.Citations.Refs {
		if !r.Resolved {
			t.Fatalf("ref %d unresolved: %+v", r.Index, r)
		}
		if r.Start < 0 || r.End > len(runes) {
			t.Fatalf("ref %d out of body range [%d,%d) body=%d", r.Index, r.Start, r.End, len(runes))
		}
		if strings.HasPrefix(r.Quote, "[?]") {
			t.Fatalf("resolved ref must not carry [?]: %+v", r)
		}
	}
	if !strings.Contains(res.Citations.Legend, "refs") {
		t.Fatalf("legend missing: %s", res.Citations.Legend)
	}
}

// Unresolved citation must carry the [?] legend (delivery face).
func TestUnresolvedCarriesQuestionMark(t *testing.T) {
	cs := CitationSet{Refs: []Ref{{Index: 1, Start: 10, End: 5, Quote: "x", Resolved: false}}}
	cs.Refs[0].Quote = "[?] " + cs.Refs[0].Quote
	leg := legend(cs, true)
	if !strings.Contains(leg, "[?]") {
		t.Fatalf("legend must explain [?]: %s", leg)
	}
}

// multi-source samples resolve against their own source; windows that do
// not pin back to the current body stay [?] (stale evidence never becomes现证).
func TestBuildCitationsPerSourceResolution(t *testing.T) {
	a := source.New("手册A", "md", "", "a", "zh", "连接池最大 128，详见后文说明。", nil)
	b := source.New("手册B", "md", "", "b", "zh", "超时时间是 30 秒，别忘了。", nil)
	ra, rb := []rune(a.Body), []rune(b.Body)
	ans := fast.Answer{Query: "q", SourceID: a.ID, Samples: []mcs.Sample{
		{Start: 0, End: 8, Content: string(ra[0:8]), Source: a.ID},   // DEEP: doc-attributed
		{Start: 0, End: 7, Content: string(rb[0:7]), Source: b.ID},   // second source (regression)
		{Start: 0, End: 4, Content: string(ra[0:4]), Source: "fuzz"}, // FAST: method label → fallback
		{Start: 0, End: 8, Content: "这段话其实已经变了", Source: a.ID},       // stale → [?]
	}}
	cs := BuildCitations(ans.Query, ans, []source.Source{a, b})
	if len(cs.Refs) != 4 {
		t.Fatalf("want 4 refs, got %d", len(cs.Refs))
	}
	r0, r1, r2, r3 := cs.Refs[0], cs.Refs[1], cs.Refs[2], cs.Refs[3]
	if r0.SourceID != a.ID || !r0.Resolved {
		t.Fatalf("ref0 must resolve to source A: %+v", r0)
	}
	if r1.SourceID != b.ID || !r1.Resolved {
		t.Fatalf("ref1 must resolve to source B (per-source pin-back): %+v", r1)
	}
	if r2.SourceID != a.ID || !r2.Resolved {
		t.Fatalf("ref2 method-label sample must fall back to answer source: %+v", r2)
	}
	if r3.Resolved || !strings.HasPrefix(r3.Quote, "[?]") {
		t.Fatalf("stale window must stay unresolved with [?]: %+v", r3)
	}
}

// conflict pairs are discoverable.
func TestGateDConflictDiscoverable(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryConflict()
	a := cluster.New("t1", "n1", "连接池是 128", "q1", "s1", nil, []float64{1}, 0.8)
	a.ID = "Ca"
	b := cluster.New("t2", "n2", "连接池是 256", "q2", "s2", nil, []float64{0, 1}, 0.8)
	b.ID = "Cb"
	c, err := DetectConflict(ctx, st, a, b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Between(ctx, "Ca", "Cb")
	if err != nil || len(got) == 0 {
		t.Fatalf("conflict not discoverable: %v %+v", err, got)
	}
	if got[0].Group != c.Group {
		t.Fatalf("group mismatch")
	}
	if !strings.Contains(c.Reason, "128") || !strings.Contains(c.Reason, "256") {
		t.Fatalf("reason must name claims: %s", c.Reason)
	}
	// Same claim → no conflict.
	same := cluster.New("t3", "n3", "连接池是 128", "q3", "s3", nil, []float64{1}, 0.8)
	same.ID = "Cd"
	if _, err := DetectConflict(ctx, st, a, same); err == nil {
		t.Fatal("identical claims must not conflict")
	}
}

// D1: widen must exclude only files this run tried — never the full candidate
// list (L1Pre=false used to starve the extra budget on the same corpus).
// Fixture: admission takes MaxLoops; self-correction may pull up to
// CorrectBudget untried files; one file remains untouched and must be eligible.
func TestWidenExcludeOnlyTried(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	n := MaxLoops + CorrectBudget + 1
	var srcs []source.Source
	for i := 0; i < n; i++ {
		// Distinct bodies → distinct content-addressed IDs (source.IDFor).
		srcs = append(srcs, source.New(
			fmt.Sprintf("弱%d", i), "md", "", fmt.Sprintf("weak%d", i), "zh",
			fmt.Sprintf("%d %s", i, strings.Repeat("完全无关的填充文本。", 40)), nil,
		))
	}
	last := srcs[n-1]
	for i := 1; i < n; i++ {
		if srcs[i].ID == srcs[0].ID {
			t.Fatalf("fixture IDs must be unique: src[%d]==src[0] %s", i, srcs[i].ID)
		}
	}

	e.RankAdmission = func(_ context.Context, _ string, sources []source.Source, _ map[string]bool) ([]source.Source, error) {
		if len(sources) > MaxLoops {
			return sources[:MaxLoops], nil
		}
		return sources, nil
	}
	e.Widen = func(_ context.Context, _ string, exclude map[string]bool, _ int, _ map[string]bool) ([]source.Source, error) {
		if exclude[last.ID] {
			t.Errorf("D1: never-tried file %s must not be in exclude", last.ID)
			return nil, nil
		}
		for i := 0; i < MaxLoops; i++ {
			if !exclude[srcs[i].ID] {
				t.Errorf("D1: admission-tried file %d should be excluded", i)
			}
		}
		return []source.Source{last}, nil
	}

	_, _, _, widened, _, _, _, _, err := e.runDeep(ctx, "连接池最大是多少 以及 超时多久", srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if widened < 1 {
		t.Fatalf("D1: widen must admit the never-tried file, got widened=%d", widened)
	}
}

// D4: filling MaxLoops on an incomplete multi-fact query must still enter
// self-correction (own budget), not share the admission clock.
func TestSelfCorrectAfterFullAdmissionBudget(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	e.Widen = nil
	var srcs []source.Source
	for i := 0; i < MaxLoops; i++ {
		srcs = append(srcs, source.New(
			fmt.Sprintf("薄%d", i), "md", "", fmt.Sprintf("thin%d", i), "zh",
			strings.Repeat("无关内容。", 50), nil,
		))
	}
	e.RankAdmission = func(_ context.Context, _ string, sources []source.Source, _ map[string]bool) ([]source.Source, error) {
		return sources, nil
	}
	_, _, _, _, selfCorrected, _, _, _, err := e.runDeep(ctx, "连接池最大是多少 以及 超时多久", srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !selfCorrected {
		t.Fatal("D4: self-correction must run when admission exhausts MaxLoops and coverage is open")
	}
}

// D2: topKeeps drops low-score windows; Cover must be recomputed on the
// truncated set (pre-truncate Complete must not leak into res.Cover).
func TestCoverRecomputedAfterTopKeeps(t *testing.T) {
	fx := facts.Build("连接池最大是多少 以及 超时多久")
	if len(fx) < 2 {
		t.Fatalf("want K>=2, got %+v", fx)
	}
	var kept []mcs.Sample
	// Distinct sources (and non-overlapping spans) so consolidateWindows keeps
	// them as separate blocks — only then does the top-8 cut drop the tail.
	for i := 0; i < maxKeepWindows; i++ {
		kept = append(kept, mcs.Sample{
			Start: 0, End: 1, Source: fmt.Sprintf("s%d", i), Score: 9,
			Content: "连接池最大是多少 词面命中。",
			Covers:  []string{fx[0].ID},
		})
	}
	// Unique support for f2 lives only on the 9th (lowest-score) block.
	kept = append(kept, mcs.Sample{
		Start: 0, End: 1, Source: "s-low", Score: 4,
		Content: "超时多久 单独一窗。",
		Covers:  []string{fx[1].ID},
	})
	before := facts.ReportForOracle(fx, kept)
	if !before.Complete {
		t.Fatalf("precondition: full pool must cover both facts: %+v", before)
	}
	top := topKeeps(kept)
	if len(top) != maxKeepWindows {
		t.Fatalf("want truncation to %d blocks, got %d", maxKeepWindows, len(top))
	}
	after := facts.ReportForOracle(fx, top)
	if after.Complete {
		t.Fatalf("D2: after truncating the only f2 window, Cover must be incomplete: %+v", after)
	}
	if len(after.Missing) == 0 {
		t.Fatalf("D2: missing must list dropped facts: %+v", after)
	}
}

// A refusal must look like a refusal. The template used to open with 【DEEP 摘要】
// and only hang the uncovered requirement on the tail as an internal fact id, so
// the screen showed a confident-looking digest over an unrelated quote. The
// header prefix stays untouched (fast.RefusedOfSummary keys on 【...摘要】).
func TestIncompleteCoverageLeadsWithTheInsufficiency(t *testing.T) {
	kept := []mcs.Sample{{Source: "src:中华人民共和国社会保险法.txt#1", Start: 3234, End: 3762, Content: "疗服务行为。……", Score: 4}}
	rep := facts.Report{
		Complete: false,
		Missing:  []string{"f1"},
		Facts:    []facts.Fact{{ID: "f1", Query: "工伤是如何认定的", Covered: false}},
	}
	_, conf, text := deepMetrics("工伤是如何认定的", "中华人民共和国社会保险法.txt", kept, rep)

	if !strings.HasPrefix(text, "【DEEP 摘要】工伤是如何认定的\n") {
		t.Fatalf("模板头必须保持原样（模板识别依赖它）:\n%s", text)
	}
	warn := strings.Index(text, "⚠ 证据不足")
	source := strings.Index(text, "【来源】")
	if warn < 0 || warn > source {
		t.Fatalf("拒答提示必须在【来源】之前:\n%s", text)
	}
	if !strings.Contains(text, "不等于答案") {
		t.Fatalf("必须说清引文不是答案:\n%s", text)
	}
	if !strings.Contains(text, "工伤是如何认定的") || strings.Contains(text, "f1") {
		t.Fatalf("未覆盖需求要给人看的文本而不是内部 id:\n%s", text)
	}
	if conf > 0.45 {
		t.Fatalf("未覆盖需求必须压住置信度: %v", conf)
	}

	// 证据齐全时不得出现这条警告
	_, _, complete := deepMetrics("工伤是如何认定的", "t", kept, facts.Report{Complete: true})
	if strings.Contains(complete, "⚠ 证据不足") {
		t.Fatalf("完整覆盖不该报拒答:\n%s", complete)
	}
	// 拿不到事实文本时退回 id，但仍有可读兜底
	if _, _, fallback := deepMetrics("q", "t", kept, facts.Report{Complete: false, Missing: []string{"f9"}}); !strings.Contains(fallback, "f9") {
		t.Fatalf("缺事实文本时应退回 id:\n%s", fallback)
	}
	if _, _, none := deepMetrics("q", "t", kept, facts.Report{Complete: false}); !strings.Contains(none, "（未细分）") {
		t.Fatalf("空 missing 要有兜底文案:\n%s", none)
	}
}

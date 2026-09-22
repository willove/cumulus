package deep

import (
	"context"
	"strings"
	"testing"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/kb"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
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

// 门 D: thin evidence must escalate to DEEP.
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

// 门 D: citations resolve back to source offsets.
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

// 门 D: multi-source samples resolve against their own source; windows that do
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

// 门 D: conflict pairs are discoverable.
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

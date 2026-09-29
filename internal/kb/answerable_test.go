package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// The production failure this file reproduces, verbatim from a real run on a
// 727,731-rune novel (2026-09-28, 5/5 questions answered wrongly):
//
//	cluster list → level_keys[principle] = "【摘要】孙悟空是在哪里学的本事"
//	               level_keys[anchor]   = a poem about blessings, unrelated
//	               level_keys[scenario] = the question
//
// The rejection template had been persisted as a cluster's PRINCIPLE, with an
// irrelevant anchor, and the next same-topic query replayed it (0 tokens).
//
// The shape that matters: the evidence windows DO contain the query's words —
// that is why both existing gates passed. A passage can match a question
// lexically and still not answer it.

// productionSynth stands in for the wired aigate synthesizer: present, and able
// to answer. The stopgap keys on PRESENCE, because presence is what separates
// production (a template means the synthesizer refused) from the offline gates
// (a template is the legitimate answer shape they exist to produce).
type productionSynth struct{ text string }

func (p *productionSynth) Synthesize(context.Context, string, []mcs.Sample) (string, error) {
	return p.text, nil
}

func withSynth(e *Engine, text string) *Engine {
	e.Fast.Synth = &productionSynth{text: text}
	return e
}

// answerWithNoContent builds the exact Answer production produced: the
// deterministic template summary (question echoed, then a quote), Refused left
// FALSE (as observed), and evidence whose text contains the query's terms.
func answerWithNoContent(query string, src source.Source) fast.Answer {
	sm := mcs.Sample{
		Start:   0,
		End:     200,
		Content: "孙悟空在傲来国与四只猴子商议兵器之事，悟空道：怎见容易。",
		Source:  src.ID,
		Score:   6,
	}
	tmpl := synthesizeTemplate(query, src, []mcs.Sample{sm})
	return fast.Answer{
		Query:      query,
		Mode:       fast.ModeFAST,
		SourceID:   src.ID,
		Samples:    []mcs.Sample{sm},
		Coverage:   0.66,
		Confidence: 0.65,
		Summary:    tmpl,
		Skipped:    false,
		Refused:    false, // ← what production actually carried
	}
}

// TestPersistRefusesTemplateAnswer is the stopgap: the persistence boundary
// re-derives the refusal signal instead of trusting ans.Refused to have
// survived every hop. A template summary means the synthesizer never produced an
// answer, whatever the flag says.
func TestPersistRefusesTemplateAnswer(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := withSynth(New(fast.New(mcs.KeywordScorer{Keywords: []string{"孙悟空", "兵器"}}), st, cluster.Local{N: 64}), "真实答案")
	srcs := fixtureSources()

	ans := answerWithNoContent("孙悟空的兵器是什么", srcs[0])
	res, err := e.Persist(ctx, ans, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Persisted {
		t.Fatalf("a template answer was persisted (cluster=%s): the question would be replayed as knowledge",
			res.ClusterID)
	}
	if res.ClusterID != "" {
		t.Fatalf("a refused answer must not report a cluster id, got %s", res.ClusterID)
	}
}

// TestEchoesQueryIsNotWired documents why the echo predicate exists but is not
// in the persistence gate: it has a demonstrated false positive, and its only
// evidence is a test this package wrote itself.
func TestEchoesQueryIsNotWired(t *testing.T) {
	if !fast.EchoesQuery("孙悟空的兵器是什么", "孙悟空的兵器是什么") {
		t.Error("a bare restatement of the question is an echo")
	}
	// The false positive that keeps it out of the gate: this is a legitimate
	// short answer, and the predicate calls it an echo.
	if !fast.EchoesQuery("alpha and beta", "alpha beta") {
		t.Error("expected the documented false positive — if this ever passes, " +
			"the predicate tightened and the gate decision should be revisited")
	}
	if fast.EchoesQuery("连接池最大连接数是多少", "连接池最大 128，超时 30 秒") {
		t.Error("a real answer must not read as an echo")
	}
}

// TestOfflineGatesStillPersistTemplate is the regression guard for the
// Synth guard: with no synthesizer wired, a template summary is the legitimate
// answer shape the deterministic gates depend on, and refusing it would break
// them. Five existing gate tests regressed when this stopgap was first written
// without that condition.
func TestOfflineGatesStillPersistTemplate(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"孙悟空", "兵器"}}), st, cluster.Local{N: 64})
	if e.synthWired() {
		t.Fatal("this test is about the un-wired case")
	}
	srcs := fixtureSources()
	res, err := e.Persist(ctx, answerWithNoContent("孙悟空的兵器是什么", srcs[0]), srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Persisted {
		t.Fatal("an offline run must keep persisting its template answers — " +
			"the deterministic gates depend on it")
	}
}

// TestPersistStillAcceptsRealAnswer is the other half: the stopgap must not
// swallow legitimate answers, including short ones.
func TestPersistStillAcceptsRealAnswer(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64})
	srcs := fixtureSources()

	for _, summary := range []string{
		"连接池最大连接数为 128，超时 30 秒。",
		"最大值 128。",
		"配置项 max=128。",
	} {
		sm := mcs.Sample{
			Start: 0, End: 200, Source: srcs[0].ID, Score: 8,
			Content: "关键配置：连接池最大 128，超时 30 秒。",
		}
		ans := fast.Answer{
			Query: "连接池最大连接数是多少", Mode: fast.ModeFAST,
			SourceID: srcs[0].ID, Samples: []mcs.Sample{sm},
			Coverage: 0.9, Confidence: 0.85, Summary: summary,
		}
		res, err := e.Persist(ctx, ans, srcs)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Persisted {
			t.Fatalf("a real answer was refused: %q", summary)
		}
	}
}

// TestPersistRefusesTemplateWithRealEvidence is the sharp case: the evidence is
// genuinely on-topic and scores well, and only the summary is a template. A
// stopgap keyed on evidence quality alone would still let this through.
func TestPersistRefusesTemplateWithRealEvidence(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := withSynth(New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), st, cluster.Local{N: 64}), "真实答案")
	srcs := fixtureSources()

	sm := mcs.Sample{
		Start: 0, End: 200, Source: srcs[0].ID, Score: 9,
		Content: "关键配置：连接池最大 128，超时 30 秒。",
	}
	q := "连接池最大连接数是多少"
	ans := fast.Answer{
		Query: q, Mode: fast.ModeFAST, SourceID: srcs[0].ID,
		Samples: []mcs.Sample{sm}, Coverage: 0.95, Confidence: 0.9,
		Summary: "【摘要】" + q + "\n【来源】" + srcs[0].Title + "\n[1] (…) …" + "关键配置：连接池最大 128…",
	}
	res, err := e.Persist(ctx, ans, srcs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Persisted {
		t.Fatal("template summary persisted even with on-topic, high-scoring evidence")
	}
}

// synthesizeTemplate mirrors fast.synthesize's output shape without importing
// it as unexported: the header, then the question, then a numbered quote.
func synthesizeTemplate(query string, src source.Source, samples []mcs.Sample) string {
	var b strings.Builder
	b.WriteString("【摘要】")
	b.WriteString(query)
	b.WriteString("\n【来源】")
	title := src.Title
	if title == "" {
		title = src.ID
	}
	b.WriteString(title)
	b.WriteString("\n")
	for i, sm := range samples {
		b.WriteString(sm.Content)
		_ = i
	}
	return b.String()
}

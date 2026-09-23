package prompts

import (
	"strings"
	"testing"
)

func TestAllAssetsLoad(t *testing.T) {
	for _, name := range []string{
		EvaluateSample, FastAnalyze, KeywordsMultilevel, HistoryRewrite, SynthesizeROI,
		JudgeCorrect, KeywordsRefine, QueryAbstract, QueryFromAbstract,
	} {
		body, err := Load(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(body) < 40 {
			t.Fatalf("%s too short", name)
		}
	}
}

func TestJudgeCorrectContract(t *testing.T) {
	body, err := Load(JudgeCorrect)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"{{query}}", "{{reference}}", "{{answer}}", "0–3", "7–9", "JSON"} {
		if !strings.Contains(body, s) {
			t.Fatalf("judge_correct missing %q", s)
		}
	}
}

func TestEvaluateSampleRubricFrozen(t *testing.T) {
	body, err := Load(EvaluateSample)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"0-3", "4-7", "8-10", "{{query}}", "JSON"} {
		if !strings.Contains(body, s) {
			t.Fatalf("rubric missing %q", s)
		}
	}
}

func TestRenderInjectsAndKeepsUnknown(t *testing.T) {
	tmpl := "Q: {{query}} S: {{sample_source}} U: {{unknown}}"
	got := Render(tmpl, map[string]string{"query": "连接池", "sample_source": "fuzz"})
	if !strings.Contains(got, "连接池") || !strings.Contains(got, "fuzz") {
		t.Fatalf("render failed: %s", got)
	}
	if !strings.Contains(got, "{{unknown}}") {
		t.Fatalf("unknown placeholder must remain: %s", got)
	}
}

func TestFastAnalyzeShapeContract(t *testing.T) {
	body, err := Load(FastAnalyze)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"intent", "search", "chat", "doc_summary", "primary", "fallback"} {
		if !strings.Contains(body, s) {
			t.Fatalf("fast_analyze missing %q", s)
		}
	}
}

func TestSynthesizeRefuseContract(t *testing.T) {
	body, err := Load(SynthesizeROI)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"refuse", "[n]", "不得编造", "{{evidences}}"} {
		if !strings.Contains(body, s) {
			t.Fatalf("synthesize missing %q", s)
		}
	}
}

func TestKeywordsRefineContract(t *testing.T) {
	body, err := Load(KeywordsRefine)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"{{query}}", "{{failed}}", "refined", "JSON"} {
		if !strings.Contains(body, s) {
			t.Fatalf("keywords_refine missing %q", s)
		}
	}
}

// Self-Index A.2.1: call 1 abstracts the need from the raw query; call 2
// must not see the raw original wording (only abstract + tried list).
func TestQuerySimulatorTwoCallContract(t *testing.T) {
	a, err := Load(QueryAbstract)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"{{query}}", "JSON"} {
		if !strings.Contains(a, s) {
			t.Fatalf("query_abstract missing %q", s)
		}
	}
	b, err := Load(QueryFromAbstract)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"{{need}}", "{{tried}}", "queries", "JSON"} {
		if !strings.Contains(b, s) {
			t.Fatalf("query_from_abstract missing %q", s)
		}
	}
	if strings.Contains(b, "{{query}}") {
		t.Fatal("second call must not receive the raw original query (A.2.1 isolation)")
	}
}

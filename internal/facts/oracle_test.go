package facts

import (
	"testing"

	"github.com/cumubase/ask/internal/mcs"
)

// the oracle vector decides coverage — a window whose covers name the
// fact counts even when keyword overlap would miss (and vice versa).
func TestEvaluateOracleUsesCovers(t *testing.T) {
	fx := Build("连接池最大是多少 以及 超时多久")
	hits := []mcs.Sample{
		{Start: 0, End: 40, Content: "完全不同的措辞：上限 128。", Source: "src:a", Score: 9, Covers: []string{"f1"}},
		{Start: 40, End: 80, Content: "三十秒后断开。", Source: "src:b", Score: 8, Covers: []string{"f2"}},
	}
	rep := EvaluateOracle(fx, hits)
	if !rep.Complete {
		t.Fatalf("oracle covers must complete both facts: %+v", rep)
	}
	if rep.Facts[0].SourceID != "src:a" || rep.Facts[1].SourceID != "src:b" {
		t.Fatalf("oracle must bind each fact to its own window: %+v", rep.Facts)
	}
	// Keyword path would miss f1 entirely (zero lexical overlap).
	kw := Evaluate(fx, hits)
	if kw.Complete {
		t.Fatalf("keyword path must stay honest on paraphrase: %+v", kw)
	}
}

func TestReportForPicksOracleWhenAnnotated(t *testing.T) {
	fx := Build("问题一")
	plain := []mcs.Sample{{Start: 0, End: 5, Content: "问题一内容", Source: "s", Score: 8}}
	if rep := ReportFor(fx, plain); !rep.Complete {
		t.Fatalf("plain samples fall back to keyword path: %+v", rep)
	}
	annotated := []mcs.Sample{{Start: 0, End: 5, Content: "别的措辞", Source: "s", Score: 8, Covers: []string{"f1"}}}
	if rep := ReportFor(fx, annotated); !rep.Complete {
		t.Fatalf("annotated samples take the oracle path: %+v", rep)
	}
}

// D3: FactAware empty covers mean "covered nothing" — ReportForOracle must
// not fall back to keywords and re-mark an honest miss as complete.
func TestReportForOracleEmptyCovers(t *testing.T) {
	fx := Build("连接池最大是多少 以及 超时多久")
	// Lexically strong on both halves, but the oracle claimed no covers.
	samples := []mcs.Sample{
		{Start: 0, End: 40, Content: "连接池最大是多少 完全命中词面。", Source: "s1", Score: 9, Covers: []string{}},
		{Start: 40, End: 80, Content: "以及 超时多久 也命中词面。", Source: "s2", Score: 9, Covers: nil},
	}
	kw := ReportFor(fx, samples)
	if !kw.Complete {
		t.Fatalf("stub ReportFor may still complete via keywords (baseline): %+v", kw)
	}
	orc := ReportForOracle(fx, samples)
	if orc.Complete {
		t.Fatalf("oracle empty covers must stay incomplete: %+v", orc)
	}
	if len(orc.Missing) == 0 {
		t.Fatalf("missing must list open facts: %+v", orc)
	}
}

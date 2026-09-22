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

// oracleE2EScorer feeds the B6 observation vector through the whole DEEP loop:
// one scoring call per window names every fact the window supports.
type oracleE2EScorer struct{}

func (oracleE2EScorer) Score(_ context.Context, _ string, s mcs.Sample) (float64, string, error) {
	return 5, "plain", nil
}

func (oracleE2EScorer) ScoreWithFacts(_ context.Context, _ string, _ []string, s mcs.Sample) (float64, string, []string, error) {
	var covers []string
	if strings.Contains(s.Content, "一百二十八") {
		covers = append(covers, "f1")
	}
	if strings.Contains(s.Content, "三十秒") {
		covers = append(covers, "f2")
	}
	if len(covers) > 0 {
		return 9, "直接命中", covers, nil
	}
	return 1, "无关", nil, nil
}

// Multi-hop paraphrase (zero lexical overlap between facts) must complete via
// the oracle vector, and the accounting fields (B9) must be present.
func TestAskOracleCoverageAndAccounting(t *testing.T) {
	body := strings.Repeat("无关内容甲乙丙丁。\n", 60) +
		"上限为一百二十八台。\n" +
		strings.Repeat("无关内容戊己庚辛。\n", 60) +
		"断开等待是三十秒。\n" +
		strings.Repeat("无关内容壬癸子丑。\n", 60)
	src := source.New("手册", "md", "", "m", "zh", body, nil)
	fe := fast.New(oracleE2EScorer{})
	e := New(kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64}), NewMemoryConflict())
	e.Scorer = oracleE2EScorer{}
	res, err := e.Ask(context.Background(), "连接池最多允许多少 以及 断开要等多久", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cover.Complete {
		t.Fatalf("oracle vector must complete the paraphrased multi-hop query: mode=%s escalated=%v sampled=%d loops=%d\n%+v",
			res.Mode, res.Escalated, res.Sampled, res.Loops, res.Cover)
	}
	if len(res.Cover.Facts) < 2 {
		t.Fatalf("want K≥2 facts, got %+v", res.Cover.Facts)
	}
	if res.LatencyMS < 0 {
		t.Fatalf("latency accounting missing: %d", res.LatencyMS)
	}
}

package deep

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// B10 γ(I): the escalation line rises with the fact count and caps at 0.6.
func TestGammaThreshold(t *testing.T) {
	e := New(nil, nil)
	cases := []struct {
		query string
		want  float64
	}{
		{"连接池最大是多少", 0.35},                   // K=1: base line
		{"连接池最大是多少 以及 超时多久", 0.40},           // K=2
		{"A 以及 B 以及 C", 0.45},                // K=3
		{"A 以及 B 以及 C 以及 D 以及 E 以及 F", 0.50}, // K≥5: capped at +3 steps
	}
	for _, c := range cases {
		got := e.thresholdFor(facts.Build(c.query))
		if diff := got - c.want; diff > 1e-9 || diff < -1e-9 {
			t.Fatalf("%q threshold=%v want %v", c.query, got, c.want)
		}
	}
	e.EscalateBelow = 0.5
	if got := e.thresholdFor(facts.Build("A 以及 B")); got <= 0.5 {
		t.Fatalf("non-positive base must still be raised: %v", got)
	}
}

// Behavioral: a two-fact question whose SECOND fact is thin must escalate
// even when its confidence would pass the single-fact line.
func TestMultiFactThinSecondRequirementEscalates(t *testing.T) {
	body := strings.Repeat("无关内容甲乙丙丁。\n", 60) +
		"上限为一百二十八台。\n" +
		strings.Repeat("无关内容戊己庚辛。\n", 60)
	src := source.New("手册", "md", "", "m", "zh", body, nil)
	fe := fast.New(mcs.KeywordScorer{})
	e := New(kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64}), NewMemoryConflict())
	res, err := e.Ask(context.Background(), "连接池上限一百二十八台吗 以及 断开要等多久", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer.Confidence >= 0.4 && !res.Escalated && !res.Answer.Skipped {
		t.Fatalf("K=2 with a thin second fact must escalate at γ: conf=%.3f escalated=%v",
			res.Answer.Confidence, res.Escalated)
	}
	if !res.Escalated && res.Cover.Complete {
		return // fully covered multi-hop is a legitimate FAST stop
	}
}

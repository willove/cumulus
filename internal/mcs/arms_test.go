package mcs

import (
	"context"
	"strings"
	"testing"
)

// the global arm must sweep the whole span, not cluster at one end —
// with a fixed seed the draws stay reproducible while covering both edges.
func TestGlobalScatterSpan(t *testing.T) {
	n := 6000
	runes := make([]rune, n)
	for i := range runes {
		runes[i] = '填'
	}
	cfg := DefaultConfig()
	s := New(cfg, KeywordScorer{})
	got := s.globalScatter(runes, 12)
	if len(got) != 12 {
		t.Fatalf("want 12 windows, got %d", len(got))
	}
	minC, maxC := n, 0
	for _, sm := range got {
		c := (sm.Start + sm.End) / 2
		if c < minC {
			minC = c
		}
		if c > maxC {
			maxC = c
		}
		if sm.Arm != "global" {
			t.Fatalf("arm label wrong: %q", sm.Arm)
		}
	}
	if minC > n/5 {
		t.Fatalf("global draws never reached the head: min center %d", minC)
	}
	if maxC < 4*n/5 {
		t.Fatalf("global draws never reached the tail: max center %d", maxC)
	}
}

type armScorer struct{ favor string }

func (a armScorer) Score(_ context.Context, _ string, s Sample) (float64, string, error) {
	if s.Arm == a.favor {
		return 8, "hit", nil
	}
	return 0, "miss", nil
}

// online weights: an arm that yields scoreable windows pulls budget from
// arms that yield nothing — but never below the one-slot floor.
func TestLambdaShiftsBudgetToYieldingArm(t *testing.T) {
	bodyRunes := make([]rune, 6000)
	for i := range bodyRunes {
		bodyRunes[i] = '填'
	}
	body := string(bodyRunes)
	cfg := DefaultConfig()
	cfg.SamplesPerRound = 9
	cfg.Rounds = 4
	cfg.SmallFileRunes = 10
	cfg.Window = 120
	s := New(cfg, armScorer{favor: "global"})
	if _, err := s.SampleBody(context.Background(), "查询", body); err != nil {
		t.Fatal(err)
	}
	sum := s.Weights["lex"] + s.Weights["local"] + s.Weights["global"]
	if sum < 0.99 || sum > 1.01 {
		t.Fatalf("weights must stay normalized, sum=%v", sum)
	}
	if s.Weights["global"] <= s.Weights["lex"] || s.Weights["global"] <= s.Weights["local"] {
		t.Fatalf("yielding arm must gain weight: %+v", s.Weights)
	}
}

// oracle path: a FactAware scorer's covers flow into samples.
type oracleScorer struct{}

func (oracleScorer) Score(_ context.Context, _ string, s Sample) (float64, string, error) {
	return 5, "plain", nil
}

func (oracleScorer) ScoreWithFacts(_ context.Context, _ string, facts []string, s Sample) (float64, string, []string, error) {
	if strings.Contains(s.Content, "连接池最大") {
		return 9, "直接答案", []string{"f1"}, nil
	}
	return 1, "无关", nil, nil
}

func TestFactHintsFlowCoversIntoSamples(t *testing.T) {
	body := "无关填充。\n" + repeatRune("连接池最大 128。", 40)
	cfg := DefaultConfig()
	cfg.SmallFileRunes = 5 // force sampling
	s := New(cfg, oracleScorer{})
	s.FactHints = []string{"f1:连接池最大是多少"}
	samples, err := s.SampleBody(context.Background(), "连接池", body)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sm := range samples {
		for _, c := range sm.Covers {
			if c == "f1" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("oracle covers must flow into samples (%d windows)", len(samples))
	}
}

func repeatRune(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

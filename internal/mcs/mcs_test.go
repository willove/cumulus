package mcs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestShortBodyReturnsFullROI(t *testing.T) {
	s := New(DefaultConfig(), KeywordScorer{Keywords: []string{"广州", "端口"}})
	got, err := s.SampleBody(context.Background(), "广州 端口", "系统部署在广州机房，端口是 8480。")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source != "full" {
		t.Fatalf("want one full ROI, got %+v", got)
	}
	if got[0].Score < 8 {
		t.Fatalf("direct answer should score >=8, got %v", got[0])
	}
}

func TestKnownAnswerWindowIsTop(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString(strings.Repeat("无关段落填充文本 padding padding padding padding padding。\n", 8))
	}
	b.WriteString("【ANSWER】数据库连接池默认最大连接数是 128，超时 30 秒。")
	for i := 0; i < 40; i++ {
		b.WriteString(strings.Repeat("无关段落填充文本 padding padding padding padding padding。\n", 8))
	}
	body := b.String()
	s := New(DefaultConfig(), KeywordScorer{Keywords: []string{"连接池", "128"}})
	got, err := s.SampleBody(context.Background(), "连接池最大连接数是多少", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no samples")
	}
	found := false
	limit := 3
	if limit > len(got) {
		limit = len(got)
	}
	for _, sm := range got[:limit] {
		if strings.Contains(sm.Content, "128") {
			found = true
			break
		}
	}
	if !found {
		top := got[0].Content
		if len(top) > 80 {
			top = top[:80]
		}
		t.Fatalf("known answer window not in top-3; top=%q", top)
	}
}

func TestCoverageAndConfidence(t *testing.T) {
	top := []Sample{{Content: "连接池 128"}}
	c := Coverage("连接池 128", top)
	if c != 1 {
		t.Fatalf("coverage=%v want 1", c)
	}
	conf := Confidence(9, 1)
	if conf < 0.9 {
		t.Fatalf("confidence=%v want >=0.9", conf)
	}
	if Confidence(0, 0) != 0 {
		t.Fatal("zero inputs should give zero confidence")
	}
}

func TestHasAnyToken(t *testing.T) {
	if !HasAnyToken("连接池最大 128", []string{"连接池", "端口"}) {
		t.Error("hit token must be found")
	}
	if HasAnyToken("无关内容", []string{"连接池", "端口"}) {
		t.Error("no token may match a body without them")
	}
	if !HasAnyToken("anything", nil) {
		t.Error("empty token set must keep the file (unfilterable)")
	}
}

// totalFailScorer fails every window: the network is down, auth expired, the
// quota ran out. What must come back is an error the caller can surface —
// not an all-ScoreFailed page that downstream filters into 证据不足 and
// reports as a clean refusal.
type totalFailScorer struct{}

func (totalFailScorer) Score(context.Context, string, Sample) (float64, string, error) {
	return 0, "", errors.New("scorer down")
}

func TestSampleBodyErrorsWhenEveryWindowFails(t *testing.T) {
	s := New(DefaultConfig(), totalFailScorer{})
	_, err := s.SampleBody(context.Background(), "广州 端口", "系统部署在广州机房，端口是 8480。")
	if err == nil || !strings.Contains(err.Error(), "scorer failed for all") {
		t.Fatalf("want total-failure error, got %v", err)
	}
}

// The scorer must be monotone in coverage: the old mid band rose to ~14 and
// dropped to 8 at the 0.8 break, so 4-of-5 keywords scored WORSE than 3-of-5.
func TestKeywordScorerMonotoneInDensity(t *testing.T) {
	kws := []string{"甲", "乙", "丙", "丁", "戊"}
	prev := -1.0
	for hits := 0; hits <= len(kws); hits++ {
		content := strings.Repeat("填充文本。", 3)
		for i := 0; i < hits; i++ {
			content += kws[i]
		}
		sc, _, err := KeywordScorer{Keywords: kws}.Score(context.Background(), "q", Sample{Content: content})
		if err != nil {
			t.Fatal(err)
		}
		if hits > 0 && sc < prev {
			t.Fatalf("score dropped as coverage rose: hits=%d score=%v prev=%v", hits, sc, prev)
		}
		prev = sc
	}
	// The 0.8 break specifically: 4/5 must not score below 3/5.
	sc45, _, _ := KeywordScorer{Keywords: kws}.Score(context.Background(), "q", Sample{Content: "甲乙丙丁"})
	sc35, _, _ := KeywordScorer{Keywords: kws}.Score(context.Background(), "q", Sample{Content: "甲乙丙"})
	if sc45 < sc35 {
		t.Fatalf("density 0.8 (%v) below density 0.6 (%v)", sc45, sc35)
	}
}

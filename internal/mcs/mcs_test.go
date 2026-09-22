package mcs

import (
	"context"
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

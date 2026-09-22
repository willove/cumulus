package fast

import (
	"context"
	"strings"
	"testing"

	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

func TestFASTFindsKnownAnswer(t *testing.T) {
	body := strings.Repeat("填充无关内容 padding padding padding。\n", 30) +
		"关键配置：连接池最大 128，超时 30 秒。\n" +
		strings.Repeat("填充无关内容 padding padding padding。\n", 30)
	src := source.New("配置手册", "md", "file://cfg", "cfg", "zh", body, nil)
	other := source.New("无关", "md", "file://x", "x", "zh", "今天天气不错，适合散步。", nil)
	e := New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	ans, err := e.Search(context.Background(), "连接池最大连接数", []source.Source{src, other})
	if err != nil {
		t.Fatal(err)
	}
	if ans.SourceID != src.ID {
		t.Fatalf("picked %s want %s", ans.SourceID, src.ID)
	}
	if ans.Mode != "FAST" || ans.LLMCalls != 2 {
		t.Fatalf("mode=%s calls=%d", ans.Mode, ans.LLMCalls)
	}
	if ans.Confidence < 0.35 {
		t.Fatalf("confidence %v too low", ans.Confidence)
	}
	if ans.Skipped {
		t.Fatal("good answer must not skip cluster persist")
	}
	if !strings.Contains(ans.Summary, "128") {
		t.Fatalf("summary missing answer: %s", ans.Summary)
	}
}

func TestFASTQualityGateSkipsThinEvidence(t *testing.T) {
	src := source.New("薄", "md", "", "thin", "zh", strings.Repeat("毫无关系的文本。", 200), nil)
	e := New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	ans, err := e.Search(context.Background(), "连接池最大连接数是多少", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if !ans.Skipped {
		t.Fatalf("thin evidence must skip cluster, conf=%v", ans.Confidence)
	}
}

func TestRankPrefersHigherTf(t *testing.T) {
	a := source.New("a", "md", "", "a", "zh", "缓存 缓存 缓存 其他", nil)
	b := source.New("b", "md", "", "b", "zh", "缓存 其他文本很多", nil)
	ranked := rankSources([]string{"缓存"}, []source.Source{a, b})
	if len(ranked) == 0 || ranked[0].src.ID != a.ID {
		t.Fatalf("want a first, got %+v", ranked)
	}
}

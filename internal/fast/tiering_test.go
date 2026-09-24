package fast

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

func TestRuleAnalyzerIntents(t *testing.T) {
	an := RuleAnalyzer{}
	cases := []struct {
		query, want string
	}{
		{"你好", IntentChat},
		{"连接池最大连接数是多少", IntentSearch},
		{"总结一下这份文档", IntentDocSummary},
		{"量子引力波检测方法综述", IntentSearch}, // 综述 is a noun, not a doc verb
	}
	for _, c := range cases {
		a, err := an.Analyze(context.Background(), c.query)
		if err != nil {
			t.Fatal(err)
		}
		if a.Intent != c.want {
			t.Fatalf("%q intent=%s want %s", c.query, a.Intent, c.want)
		}
		if c.want == IntentSearch && len(a.Primary) == 0 {
			t.Fatalf("%q search intent needs primary keywords", c.query)
		}
	}
}

func TestMatchFilenameTier(t *testing.T) {
	src := source.New("连接池专册", "md", "file://docs/notes.md", "pool", "zh", "内容。", nil)
	if ans, ok := MatchFilename("连接池专册", []source.Source{src}); !ok {
		t.Fatal("exact title must hit FILENAME_ONLY")
	} else if ans.Mode != ModeFilenameOnly || ans.LLMCalls != 0 || ans.SourceID != src.ID {
		t.Fatalf("bad filename answer: %+v", ans)
	}
	if _, ok := MatchFilename("notes.md", []source.Source{src}); !ok {
		t.Fatal("extension lookup must hit FILENAME_ONLY")
	}
	if _, ok := MatchFilename("连接池专册是多少", []source.Source{src}); ok {
		t.Fatal("interrogative query must not be a name lookup")
	}
	if _, ok := MatchFilename("不存在的名字", []source.Source{src}); ok {
		t.Fatal("no match must fall through")
	}
}

type fixedSynth struct{ err error }

func (f fixedSynth) Synthesize(_ context.Context, _ string, _ []mcs.Sample) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "LLM 合成摘要 [1]", nil
}

func TestSynthesizerPreferredAndFallsBack(t *testing.T) {
	body := strings.Repeat("填充 padding padding。\n", 30) + "关键配置：连接池最大 128。\n"
	src := source.New("手册", "md", "", "m", "zh", body, nil)
	e := New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
	e.Synth = fixedSynth{}
	ans, err := e.Search(context.Background(), "连接池最大连接数", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Summary != "LLM 合成摘要 [1]" {
		t.Fatalf("synthesizer ignored: %q", ans.Summary)
	}
	e.Synth = fixedSynth{err: fmt.Errorf("endpoint down")}
	ans, err = e.Search(context.Background(), "连接池最大连接数", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ans.Summary, "【摘要】") {
		t.Fatalf("error must degrade to template: %q", ans.Summary)
	}
}

type stubAnalyzer struct{ a Analysis }

func (s stubAnalyzer) Analyze(_ context.Context, _ string) (Analysis, error) { return s.a, nil }

type stubExpander struct{ levels [][]string }

func (s stubExpander) Expand(_ context.Context, _ string, _ int) ([][]string, error) {
	return s.levels, nil
}

func TestKeywordCascadeUsesExpander(t *testing.T) {
	src := source.New("手册", "md", "", "m", "zh", "连接池最大 128。", nil)
	e := New(mcs.KeywordScorer{Keywords: []string{"连接池"}})
	// Primary and fallback both miss; expander level 2 hits.
	e.Analyzer = stubAnalyzer{Analysis{Intent: IntentSearch}}
	e.Expander = stubExpander{levels: [][]string{{"无关词"}, {"连接池"}}}
	ans, err := e.Search(context.Background(), "查询", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if ans.SourceID != src.ID {
		t.Fatalf("expander cascade must find the source: %+v", ans)
	}
}

func TestChatIntentExitsWithoutRetrieval(t *testing.T) {
	src := source.New("手册", "md", "", "m", "zh", "连接池最大 128。", nil)
	e := New(mcs.KeywordScorer{})
	ans, err := e.Search(context.Background(), "你好", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if ans.Mode != ModeChat || !ans.Skipped || len(ans.Samples) != 0 {
		t.Fatalf("chat must exit cleanly: %+v", ans)
	}
}

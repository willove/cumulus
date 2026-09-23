package prior

import (
	"strings"
	"testing"

	"github.com/cumubase/ask/internal/source"
)

// path/title/struct signals must lift the right file.
func TestBuildRanksPathSignal(t *testing.T) {
	generic := strings.Repeat("填充内容 padding padding。\n", 20)
	a := source.New("连接池配置手册", "md", "file://a", "a", "zh", generic, nil)
	b := source.New("天气记录", "md", "file://b", "b", "zh", generic, nil)
	bel := Build("连接池配置", []source.Source{a, b}, nil, 10)
	if len(bel.Files) == 0 {
		t.Fatal("no files scored")
	}
	if bel.Files[0].SourceID != a.ID {
		t.Fatalf("title/path signal must rank the manual first: %+v", bel.Files)
	}
	if bel.Files[0].Signals[SigPath] <= bel.Files[1].Signals[SigPath] {
		t.Fatalf("path signal must separate: %v vs %v",
			bel.Files[0].Signals[SigPath], bel.Files[1].Signals[SigPath])
	}
}

// Regression: span offsets are rune indices — posScore must slice runes, not
// bytes (byte slicing mangles CJK segments and misses seg hits).
func TestPosScoreSlicesRunesNotBytes(t *testing.T) {
	body := "# 首页\n一二三填充内容。\n## 连接池\n最大连接数一百二十八。\n"
	s := source.New("手册", "md", "", "m", "zh", body, nil)
	if len(s.Structure) < 2 {
		t.Fatalf("want ≥2 spans, got %+v", s.Structure)
	}
	bel := Build("一百二十八", []source.Source{s}, nil, 1)
	pos := bel.TopPos[s.ID]
	if pos.Anchor != "连接池" {
		t.Fatalf("seg hit must anchor at 连接池 (rune slicing): %+v", pos)
	}
	if pos.Start < 0 || pos.End > len([]rune(s.Body)) || pos.Start >= pos.End {
		t.Fatalf("pos offsets out of rune bounds: %+v (body=%d runes)", pos, len([]rune(s.Body)))
	}
}

func TestFilterAdmitted(t *testing.T) {
	a := source.New("连接池手册", "md", "", "a", "zh", "连接池最大 128。", nil)
	b := source.New("无关", "md", "", "b", "zh", strings.Repeat("无关。", 50), nil)
	bel := Build("连接池", []source.Source{a, b}, nil, 1)
	if len(bel.Admitted()) != 1 {
		t.Fatalf("topK=1 admitted %v", bel.Admitted())
	}
	kept := bel.Filter([]source.Source{a, b})
	if len(kept) != 1 || kept[0].ID != a.ID {
		t.Fatalf("filter kept %+v", kept)
	}
}

// Rank over supplied fields (the fast-cascade adapter) must honor path/title
// signals.
func TestRankUsesFields(t *testing.T) {
	generic := strings.Repeat("填充内容 padding padding。\n", 20)
	a := source.New("连接池配置手册", "md", "file://a", "a", "zh", generic, nil)
	b := source.New("天气记录", "md", "file://b", "b", "zh", generic, nil)
	files := Rank([]string{"连接池"}, []source.Source{a, b}, nil, 10).Files
	if len(files) == 0 || files[0].SourceID != a.ID {
		t.Fatalf("rank must lift the manual: %+v", files)
	}
}

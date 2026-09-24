package source

import "testing"

// SSOT §3.4.6 #2: the structure is a locator map, so its spans must TILE the
// body — every rune belongs to exactly one span and SliceSpan can reach it.
// Text before the first heading used to belong to no span at all, so a
// preamble was unreachable and the character-level round-trip could not hold.
func TestStructureTilesBody(t *testing.T) {
	bodies := []string{
		"前言段落，不属于任何标题。\n\n# 标题一\n\n内容一\n\n--- page 2 ---\n\n内容二\n",
		"# 从标题开始\n\n没有导语。\n",
		"纯文本，没有任何标记。\n",
		"",
		"--- page 3 ---\n只有页码标记。\n",
	}
	for _, body := range bodies {
		spans := BuildStructure(body)
		runes := []rune(body)
		covered := make([]int, len(runes))
		for _, s := range spans {
			if s.Start < 0 || s.End > len(runes) || s.Start > s.End {
				t.Fatalf("body %q: span %+v out of range", body, s)
			}
			for i := s.Start; i < s.End; i++ {
				covered[i]++
			}
		}
		for i, c := range covered {
			if c != 1 {
				t.Fatalf("body %q: rune %d (%q) covered %d times, want exactly 1",
					body, i, string(runes[i]), c)
			}
		}
	}
}

// The preamble span must carry the text before the first mark and nothing else.
func TestPreambleSpan(t *testing.T) {
	body := "开头文字\n# 标题\n正文"
	spans := BuildStructure(body)
	if len(spans) != 2 {
		t.Fatalf("want preamble + heading, got %+v", spans)
	}
	if spans[0].Kind != "preamble" || spans[0].Start != 0 {
		t.Fatalf("first span must be the preamble: %+v", spans[0])
	}
	got, err := SliceSpan(body, spans, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "开头文字\n" {
		t.Fatalf("preamble slice = %q", got)
	}
}

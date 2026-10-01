package fast

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// locate() decides which structure label the deterministic template prints
// for a sampled window. It must never stamp a label for a span the window is
// not inside — that produces a citation that looks confident and points
// somewhere else. DEEP's citation face leaves Span empty in the same
// situation; these assertions pin FAST to matching that behaviour.
func TestLocatePinsTheLabelOnlyWhenTheWindowIsInside(t *testing.T) {
	src := source.Source{
		ID:    "cfg",
		Title: "配置手册",
		Structure: []source.Span{
			{Kind: "h", Label: "第一章", Start: 0, End: 100},
			{Kind: "h", Label: "第二章", Start: 100, End: 200},
		},
	}
	cases := []struct {
		name       string
		start, end int
		want       string
		why        string
	}{
		{"inside first span", 10, 40, "第一章", "window fully contained by span 1"},
		{"inside second span", 120, 180, "第二章", "window fully contained by span 2"},
		{"exactly on the boundary", 100, 200, "第二章", "start==Start and end==End is contained"},
		{"past the last span", 200, 250, spanUnresolved, "no span contains it — must not claim 第一章"},
		{"spans the whole structure and more", 0, 250, spanUnresolved, "small-file full-body path hits this"},
		{"straddles two spans", 90, 110, spanUnresolved, "no single span contains a straddling window"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := locate(src, tc.start, tc.end); got != tc.want {
				t.Fatalf("locate(%d,%d) = %q, want %q — %s", tc.start, tc.end, got, tc.want, tc.why)
			}
		})
	}
}

// The regression this guards: a window outside every span used to be
// labelled src.Structure[0].Label, so a sample drawn past the end of the
// documented structure was reported as being in the first section.
func TestLocateNeverFallsBackToTheFirstSpan(t *testing.T) {
	src := source.Source{
		ID:    "cfg",
		Title: "配置手册",
		Structure: []source.Span{
			{Kind: "h", Label: "第一章", Start: 0, End: 10},
		},
	}
	if got := locate(src, 500, 600); got == "第一章" {
		t.Fatalf("locate returned %q for a window outside every span — this is the L4 lie", got)
	}
	if got := locate(src, 500, 600); got != spanUnresolved {
		t.Fatalf("locate = %q, want %q", got, spanUnresolved)
	}
}

// A document with no structure at all is a different case: there is nothing
// to be wrong about, "body" is the honest answer and must be preserved.
func TestLocateWithoutStructureSaysBody(t *testing.T) {
	src := source.Source{ID: "plain", Title: "纯文本"}
	if got := locate(src, 0, 999); got != "body" {
		t.Fatalf("locate on structure-less source = %q, want %q", got, "body")
	}
}

// End to end through the template: the rendered summary must carry the
// unresolved marker, and the citation legend entry must not be silently
// replaced by a real-looking section name.
func TestSynthesizeMarksUnresolvableWindows(t *testing.T) {
	body := strings.Repeat("甲乙丙丁。", 40) // 200 runes
	src := source.Source{
		ID:    "cfg",
		Title: "配置手册",
		Body:  body,
		Structure: []source.Span{
			{Kind: "h", Label: "第一章", Start: 0, End: 100},
		},
	}
	// sample at [150,200) — outside the only declared span
	sm := mcs.Sample{Source: "cfg", Start: 150, End: 200, Content: "丁甲乙丙。"}
	out := synthesize("测试查询", src, []mcs.Sample{sm})

	if !strings.Contains(out, spanUnresolved) {
		t.Fatalf("synthesize output missing the unresolved marker %q:\n%s", spanUnresolved, out)
	}
	if strings.Contains(out, "(第一章") {
		t.Fatalf("synthesize claimed 第一章 for an out-of-span window:\n%s", out)
	}
}

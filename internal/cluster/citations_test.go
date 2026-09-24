package cluster

import (
	"strings"
	"testing"

	"github.com/cumubase/ask/internal/mcs"
)

func TestRenumberCitations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		remap map[int]int
		want  string
	}{
		{"shift", "[1] a [2] b", map[int]int{1: 3, 2: 4}, "[3] a [4] b"},
		{"unmapped markers survive", "[1] a [9] b", map[int]int{1: 2}, "[2] a [9] b"},
		{"non-markers survive", "[1] [a] [12x] [] (see [3])", map[int]int{1: 2, 3: 1}, "[2] [a] [12x] [] (see [1])"},
		{"empty remap", "[1]", nil, "[1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenumberCitations(tc.in, tc.remap); got != tc.want {
				t.Fatalf("RenumberCitations(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Folding concatenates two summaries whose [n] markers each numbered their own
// evidence. After the merge the appended block must point at ITS evidence, and
// a window the survivor already carries must point at that copy rather than at
// a duplicate.
func TestFoldIntoRenumbersAppendedCitations(t *testing.T) {
	win := New("own", "name", "[1] (src:a1 [0,6)) 甲 [2] (src:a2 [0,6)) 乙", "q1", "src:a1",
		[]mcs.Sample{
			{Source: "src:a1", Start: 0, End: 6, Content: "甲甲甲"},
			{Source: "src:a2", Start: 0, End: 6, Content: "乙乙乙"},
		}, nil, 0.8)
	// The loser repeats the survivor's second window and adds one of its own.
	los := New("own", "name", "[1] (src:a2 [0,6)) 乙 [2] (src:b [0,6)) 丙", "q2", "src:b",
		[]mcs.Sample{
			{Source: "src:a2", Start: 0, End: 6, Content: "乙乙乙"},
			{Source: "src:b", Start: 0, End: 6, Content: "丙丙丙"},
		}, nil, 0.8)

	foldInto(&win, &los)

	if len(win.Evidence) != 3 {
		t.Fatalf("merged evidence = %d windows, want 3 (a1, a2, b with the repeat deduped)", len(win.Evidence))
	}
	if !strings.Contains(win.Content, "[2] (src:a2 [0,6)) 乙 [3] (src:b [0,6)) 丙") {
		t.Fatalf("appended block must be renumbered against the merged evidence: %q", win.Content)
	}
}

// A duplicate mapping must not fabricate a second copy of the same window.
func TestFoldIntoDedupesRepeatedWindows(t *testing.T) {
	win := New("own", "name", "[1] (src:a [0,6)) 甲", "q1", "src:a",
		[]mcs.Sample{{Source: "src:a", Start: 0, End: 6, Content: "甲甲甲"}}, nil, 0.8)
	los := New("own", "name", "[1] (src:a [0,6)) 甲", "q2", "src:a",
		[]mcs.Sample{{Source: "src:a", Start: 0, End: 6, Content: "甲甲甲"}}, nil, 0.8)

	foldInto(&win, &los)

	if len(win.Evidence) != 1 {
		t.Fatalf("merged evidence = %d windows, want 1 (the same span is one window)", len(win.Evidence))
	}
	if !strings.Contains(win.Content, "[1] (src:a [0,6)) 甲") {
		t.Fatalf("the repeat must keep pointing at the survivor's copy: %q", win.Content)
	}
	if strings.Contains(win.Content, "[2]") {
		t.Fatalf("a deduped window must not be renumbered to a phantom second copy: %q", win.Content)
	}
}

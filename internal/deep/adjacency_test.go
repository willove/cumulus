package deep

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// The measured shape this file pins (2026-09-28, live on the 727k-rune
// novel): block i scored 8.0 with its window cutting off mid-scene, and the
// continuation block i+1 — holding the answer's second half — sat BEHIND a
// 0.0-scored block in the admission order. With CLUS_DEEP_ADJACENCY the
// covering block's sibling is pulled to the front of the queue and explored
// next; without it the loop spends its budget on the junk the ranker put
// ahead.

// markerScorer scores by a marker token embedded in each body, so a test can
// script exactly which block is the partial answer, which is the
// continuation, and which is junk.
type markerScorer struct{ scored []string }

func (m *markerScorer) Score(_ context.Context, _ string, s mcs.Sample) (float64, string, error) {
	switch {
	case strings.Contains(s.Content, "|partial|"):
		m.scored = append(m.scored, "partial")
		return 6, "covers f1", nil
	case strings.Contains(s.Content, "|continuation|"):
		m.scored = append(m.scored, "continuation")
		return 9, "covers f1", nil
	default:
		m.scored = append(m.scored, "junk")
		return 0, "no cover", nil
	}
}

// adjFixture builds a parent document's three blocks with REAL block
// metadata (ingest's own Meta convention, explicit indices), plus the query
// text in every body so fact coverage is reachable.
func adjFixture(t *testing.T, q string) (srcs []source.Source, order []string) {
	t.Helper()
	blk := func(i int, marker string) source.Source {
		body := strings.Repeat("正文内容。", 60) + " " + marker + " " + q + " 相关记载。"
		b := source.BlockOf{Index: i, Start: i * 400, End: i*400 + 400}
		return source.New("novel.txt", "md", "file://novel.txt",
			source.BlockParentKey("novel.txt", b), "zh", body,
			source.BlockMeta("novel.txt", b, 1200))
	}
	srcs = []source.Source{blk(0, "|partial|"), blk(1, "|continuation|"), blk(2, "")}
	// The ranker's order: the junk block AHEAD of the continuation — the
	// whole measured defect.
	order = []string{"blk/novel.txt/000000", "blk/novel.txt/000002", "blk/novel.txt/000001"}
	return srcs, order
}

func adjacencyEngine(t *testing.T, sc *markerScorer, srcs []source.Source, order []string) *Engine {
	t.Helper()
	fe := fast.New(sc)
	k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
	e := New(k, NewMemoryConflict())
	e.Scorer = sc
	e.MaxLoops = 10
	byKey := map[string]source.Source{}
	for _, s := range srcs {
		byKey[s.BusinessKey] = s
	}
	ranked := make([]source.Source, 0, len(order))
	for _, key := range order {
		s, ok := byKey[key]
		if !ok {
			t.Fatalf("fixture order references unknown block %q", key)
		}
		ranked = append(ranked, s)
	}
	e.RankAdmission = func(context.Context, string, []source.Source, map[string]bool) ([]source.Source, error) {
		return ranked, nil
	}
	return e
}

func TestAdjacencyPullsContinuationForward(t *testing.T) {
	q := "孙悟空的师父是谁？学成了哪些本领？"
	srcs, order := adjFixture(t, q)

	t.Setenv("CLUS_DEEP_ADJACENCY", "1")
	sc := &markerScorer{}
	e := adjacencyEngine(t, sc, srcs, order)
	_, _, _, _, _, _, _, reason, err := e.runDeep(context.Background(), q, srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "sufficient" {
		t.Fatalf("with the pull, the continuation must complete the cover and stop: reason=%q scored=%v", reason, sc.scored)
	}
	if len(sc.scored) < 2 || sc.scored[1] != "continuation" {
		t.Fatalf("the continuation must be explored SECOND (right after its sibling), got %v", sc.scored)
	}
}

// Off (the default, and every gate): the queue is the ranker's order and the
// junk block is scored before the continuation — the measured waste, kept as
// the regression baseline until the A/B flips the default.
func TestAdjacencyOffKeepsRankerOrder(t *testing.T) {
	q := "孙悟空的师父是谁？学成了哪些本领？"
	srcs, order := adjFixture(t, q)

	sc := &markerScorer{}
	e := adjacencyEngine(t, sc, srcs, order)
	_, _, _, _, _, _, _, _, err := e.runDeep(context.Background(), q, srcs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.scored) < 2 || sc.scored[1] != "junk" {
		t.Fatalf("with the flag off the ranker's order must stand, got %v", sc.scored)
	}
}

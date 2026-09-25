package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// A merged cluster appends a summary that numbered its own evidence from 1.
// That list now starts after the cluster's own, so the marker must shift — or
// the appended text cites the cluster's evidence instead of its own.
func TestMergeShiftsAppendedMarkers(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.EnsureCollection(ctx, "clus_clusters"); err != nil {
		t.Fatal(err)
	}
	store := cluster.NewCumuStore(engine, "clus_clusters")
	e := New(fastStub(), store, embedStub())
	srcs := []source.Source{source.New("手册", "md", "", "cfg", "zh", "连接池最大 128，超时 30 秒。", nil)}
	query := "连接池最大连接数是多少"
	qe, err := e.embed(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	answer := func(start, end int, quote, summary string) fast.Answer {
		return fast.Answer{
			Query: query, Mode: fast.ModeFAST, SourceID: srcs[0].ID,
			Samples:    []mcs.Sample{{Source: srcs[0].ID, Start: start, End: end, Content: quote, Score: 8}},
			Summary:    summary,
			Confidence: 0.6,
		}
	}
	first := answer(0, 18, "连接池最大 128，超时 30 秒。",
		"【摘要】连接池最大连接数是多少\n[1] (cfg [0,18)) 连接池最大 128，超时 30 秒。")
	second := answer(0, 6, "连接池最大。",
		"【摘要】连接池最大连接数是多少 追加\n[1] (cfg [0,6)) 连接池最大 128。")

	if _, err := e.saveAnswer(ctx, first, srcs, qe, false); err != nil {
		t.Fatal(err)
	}
	res, err := e.saveAnswer(ctx, second, srcs, qe, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged {
		t.Fatalf("the second write must fold into the existing cluster: %+v", res)
	}
	all, err := store.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("clusters = %d, want 1", len(all))
	}
	if !strings.Contains(all[0].Content, "[2] (cfg [0,6))") {
		t.Fatalf("the appended block must shift its marker past the cluster's evidence: %q", all[0].Content)
	}
}

// The dedup half of the same contract: a merge whose windows the cluster
// ALREADY carries must renumber its markers onto the surviving entries. The
// remap used to assume plain appending ("have length + k + 1"), so a dropped
// duplicate left its marker pointing past the merged evidence — saved with
// the cluster, and every later reuse resolved a citation to nothing.
func TestMergeDuplicateWindowDoesNotDangle(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.EnsureCollection(ctx, "clus_clusters"); err != nil {
		t.Fatal(err)
	}
	store := cluster.NewCumuStore(engine, "clus_clusters")
	e := New(fastStub(), store, embedStub())
	srcs := []source.Source{source.New("手册", "md", "", "cfg", "zh", "连接池最大 128，超时 30 秒。", nil)}
	query := "连接池最大连接数是多少"
	qe, err := e.embed(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	answer := func(summary string) fast.Answer {
		return fast.Answer{
			Query: query, Mode: fast.ModeFAST, SourceID: srcs[0].ID,
			// The IDENTICAL window both times: the deterministic sampler on an
			// unchanged corpus repeats itself, which is exactly the shape a
			// [MergeTheta, ReuseTheta)-band re-ask produces.
			Samples:    []mcs.Sample{{Source: srcs[0].ID, Start: 0, End: 6, Content: "连接池最大。", Score: 8}},
			Summary:    summary,
			Confidence: 0.6,
		}
	}
	first := answer("【摘要】连接池最大连接数是多少\n[1] (cfg [0,6)) 连接池最大。")
	second := answer("【摘要】连接池最大连接数是多少 复问\n[1] (cfg [0,6)) 连接池最大。")

	if _, err := e.saveAnswer(ctx, first, srcs, qe, false); err != nil {
		t.Fatal(err)
	}
	res, err := e.saveAnswer(ctx, second, srcs, qe, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Merged {
		t.Fatalf("the repeat must fold into the existing cluster: %+v", res)
	}
	all, err := store.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("clusters = %d, want 1", len(all))
	}
	c := all[0]
	if len(c.Evidence) != 1 {
		t.Fatalf("G-idem: the duplicate window must not be appended, evidence = %d", len(c.Evidence))
	}
	// Every marker in the persisted content must resolve against the merged
	// evidence — the regression signature was a [2] with one evidence entry.
	for i := 1; i <= 9; i++ {
		marker := "[" + string(rune('0'+i)) + "]"
		if i > len(c.Evidence) && strings.Contains(c.Content, marker) {
			t.Fatalf("dangling citation %s with %d evidence entries: %q", marker, len(c.Evidence), c.Content)
		}
	}
	if !strings.Contains(c.Content, "[1] (cfg [0,6))") {
		t.Fatalf("the appended block must renumber onto the surviving entry: %q", c.Content)
	}
}

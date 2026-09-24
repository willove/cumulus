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

package graph

import (
	"context"
	"testing"
	"time"

	"github.com/cumubase/ask/internal/cluster"
)

// D4 optional freshness pass: stale-source neighbors are pruned, fresh ones
// survive, unresolvable ones are never pruned (阙疑不剪).
func TestHopTSPrunesStaleNeighbors(t *testing.T) {
	ctx := context.Background()
	cs := cluster.NewMemory()
	base := cluster.New("t0", "起点", "基础内容", "q", "src:fresh", nil, []float64{1, 0}, 0.8)
	near := cluster.New("t1", "新鲜邻域", "连接池内容", "q", "src:near", nil, []float64{1, 0}, 0.8)
	old := cluster.New("t2", "陈旧邻域", "连接池内容", "q", "src:old", nil, []float64{1, 0}, 0.8)
	for _, c := range []cluster.Cluster{base, near, old} {
		if err := cs.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	es := NewMemory()
	for _, ed := range []Edge{
		{From: base.ID, To: near.ID, Weight: 0.8, Source: SourceCoOcur},
		{From: base.ID, To: old.ID, Weight: 0.8, Source: SourceCoOcur},
	} {
		ed.ID = ed.From + "-" + ed.To
		if err := es.Save(ctx, ed); err != nil {
			t.Fatal(err)
		}
	}
	fresh := map[string]time.Time{
		"src:near": time.Now(),
		"src:old":  time.Now().Add(-30 * 24 * time.Hour),
	}
	ex := NewExpander(es, cs)
	ex.HopTS = 7 * 24 * time.Hour
	ex.Freshness = func(sourceID string) (time.Time, bool) {
		ts, ok := fresh[sourceID]
		return ts, ok
	}
	got, err := ex.Expand(ctx, ExpandRequest{StartID: base.ID})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.Cluster.ID)
	}
	if len(ids) != 1 || ids[0] != near.ID {
		t.Fatalf("hopTS must keep only the fresh neighbor, got %v", ids)
	}
}

func TestHopTSOffKeepsAll(t *testing.T) {
	ctx := context.Background()
	cs := cluster.NewMemory()
	base := cluster.New("t0", "起点", "内容", "q", "src:a", nil, []float64{1}, 0.8)
	old := cluster.New("t2", "邻域", "内容", "q", "src:old", nil, []float64{1}, 0.8)
	for _, c := range []cluster.Cluster{base, old} {
		if err := cs.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	es := NewMemory()
	if err := es.Save(ctx, Edge{ID: base.ID + "-" + old.ID, From: base.ID, To: old.ID, Weight: 0.8, Source: SourceCoOcur}); err != nil {
		t.Fatal(err)
	}
	ex := NewExpander(es, cs)
	ex.HopTS = time.Nanosecond // freshness set but resolver absent → no pruning
	got, err := ex.Expand(ctx, ExpandRequest{StartID: base.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("without Freshness resolver hopTS must not prune, got %v", got)
	}
}

package cluster

import (
	"context"
	"fmt"
	"testing"

	"github.com/willove/cumulite"
)

// M9: All() used to read a single hardcoded page of 1000 — exactly the design's
// cluster ceiling (≤10³) — so a full population silently truncated and the
// maintenance faces (tidy, embed_sim backfill, the scoreboard list) operated on
// a partial view.
func TestAllPaginatesPastOnePage(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	const coll = "clus_clusters"
	if err := engine.EnsureCollection(ctx, coll); err != nil {
		t.Fatal(err)
	}
	const total = 1201 // one full page + a partial second page
	for start := 0; start < total; start += 100 {
		batch := make([]map[string]any, 0, 100)
		for i := start; i < start+100 && i < total; i++ {
			batch = append(batch, map[string]any{
				"_id":        fmt.Sprintf("C%05d", i),
				"topic_key":  fmt.Sprintf("tk%05d", i),
				"name":       "n",
				"content":    "连接池最大 128。",
				"queries":    []any{"q"},
				"embed":      []any{1.0, 0.0},
				"confidence": 0.9,
				"hotness":    0.5,
				"lifecycle":  LifecycleStable,
				"version":    1,
				"source_id":  "src:d",
			})
		}
		if _, err := engine.Insert(ctx, coll, batch); err != nil {
			t.Fatalf("insert batch at %d: %v", start, err)
		}
	}
	st := NewCumuStore(engine, coll)
	all, err := st.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != total {
		t.Fatalf("All() returned %d clusters, want %d (silent truncation)", len(all), total)
	}
	// Every id is present exactly once.
	seen := map[string]int{}
	for _, c := range all {
		seen[c.ID]++
	}
	if len(seen) != total {
		t.Fatalf("distinct ids=%d, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("cluster %s appeared %d times", id, n)
		}
	}
}

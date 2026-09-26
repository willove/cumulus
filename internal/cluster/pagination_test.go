package cluster

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/mcs"
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

// The persist judge's verdict must survive the CumuStore round-trip: Save
// writes an explicit field map (not a whole-struct marshal), so a new
// Cluster field is silently dropped here while the in-memory store keeps
// it — the C2 stamp died at exactly this boundary once.
func TestCumuStoreRoundTripKeepsJudgeVerdict(t *testing.T) {
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
	st := NewCumuStore(engine, coll)
	yes := true
	in := Cluster{
		ID: "c1", TopicKey: "k", Name: "n", Content: "c",
		Queries: []string{"q"}, Lifecycle: "stable", Version: 1,
		JudgeOK: &yes, JudgeWhy: "直接回答了问题",
	}
	if err := st.Save(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := st.Get(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if out.JudgeOK == nil || !*out.JudgeOK {
		t.Fatalf("judge_ok lost in the round-trip: %v", out.JudgeOK)
	}
	if out.JudgeWhy != "直接回答了问题" {
		t.Fatalf("judge_why lost in the round-trip: %q", out.JudgeWhy)
	}
}

// CumuStore.Save writes an explicit field map, NOT a whole-struct marshal
// like the memory store — so a field added to Cluster is silently dropped
// on the Badger path while every memory-store test stays green. The C2
// judge stamp died at exactly this seam: in-memory tests passed, the
// durable record came back empty, and nothing on the write path errored.
// This walks the struct's json tags and fails when one is missing from the
// document the store actually WROTE (read raw, not parsed — the parsed
// form cannot reveal a field Save never wrote). The next added field is
// now the gate's problem, not whoever-remembers'.
func TestCumuStoreSaveCoversEveryClusterField(t *testing.T) {
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
	st := NewCumuStore(engine, coll)
	yes := true
	// Every field populated, so no conditional branch can hide a gap.
	in := Cluster{
		ID: "c1", TopicKey: "k", TopicKeys: []string{"k2"},
		LevelKeys: []LevelKey{{Level: "scenario", Text: "t"}},
		KeyEmbeds: [][]float64{{1}},
		Name:      "n", Content: "c", Queries: []string{"q"},
		Embed: []float64{1}, Confidence: 0.5, Hotness: 0.5,
		Lifecycle: "stable", Version: 2, SourceID: "s",
		Evidence: []mcs.Sample{{Content: "e"}},
		JudgeOK:  &yes, JudgeWhy: "w", Flags: map[string]bool{"f": true},
	}
	if err := st.Save(ctx, in); err != nil {
		t.Fatal(err)
	}
	res, err := engine.Query(ctx, coll, contract.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Documents) != 1 {
		t.Fatalf("stored docs = %d, want 1", len(res.Documents))
	}
	doc := res.Documents[0]
	typ := reflect.TypeOf(Cluster{})
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if _, ok := doc[name]; !ok {
			t.Errorf("Cluster field %q is missing from the CumuStore document — Save's field map is out of sync with the struct (this is how the judge stamp vanished)", name)
		}
	}
}

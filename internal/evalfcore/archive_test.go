package evalfcore

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/store"
)

// countingPort 数写入次数：append-only 是要被证明的性质，不是注释。
type countingPort struct {
	store.Port
	structPuts int
	valuePuts  int
	queries    int
}

func (c *countingPort) PutStruct(ctx context.Context, coll, id string, doc any) error {
	c.structPuts++
	return c.Port.PutStruct(ctx, coll, id, doc)
}

func (c *countingPort) PutValue(ctx context.Context, key string, value []byte) error {
	c.valuePuts++
	return c.Port.PutValue(ctx, key, value)
}

func (c *countingPort) Query(ctx context.Context, coll string, filter map[string]any, skip, limit int, out any) (int, error) {
	c.queries++
	return c.Port.Query(ctx, coll, filter, skip, limit, out)
}

// bigRun 造一个超过单值上限的档案（Badger in-memory 单值 1 MiB）。
func bigRun(n int) RunState {
	s := RunState{RunID: "big", Status: StatusRunning, ItemsTotal: n, Arm: "bm25"}
	for i := 0; i < n; i++ {
		s.Results = append(s.Results, ItemResult{
			ItemID:         "q" + strings.Repeat("y", 8),
			Answer:         strings.Repeat("证据窗口原文", 200), // ~1.4 KB/题
			CitationsTotal: 9, CitationsResolved: 9,
		})
	}
	s.ItemsDone = n
	return s
}

func newArchivePort(t *testing.T) (*Archive, store.Port) {
	t.Helper()
	p, err := store.Open("", true)
	if err != nil {
		t.Fatal(err)
	}
	return NewArchive(p), p
}

// 超过 1 MiB 的档案必须能存能读：逐题文档 + 小清单，值本身都远小于上限。
// （修复前：整份档案一个 KV 值，449 题 × 9 窗 = 1.05 MB 在第 253 题处炸。）
func TestArchiveRoundTripsOversizedRun(t *testing.T) {
	ar, p := newArchivePort(t)
	ctx := context.Background()
	state := bigRun(1200)
	raw, err := EncodeState(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 1<<20 {
		t.Fatalf("fixture must exceed the in-memory single-value limit, got %d bytes", len(raw))
	}
	if err := ar.SaveRun(ctx, state); err != nil {
		t.Fatalf("save oversized run: %v", err)
	}
	// 清单是一个小值；正文都在文档集合里
	manRaw, err := p.GetValue(ctx, runKey("big"))
	if err != nil {
		t.Fatal(err)
	}
	if len(manRaw) > 4096 {
		t.Fatalf("manifest must stay small, got %d bytes", len(manRaw))
	}
	man, err := decodeManifest(manRaw)
	if err != nil {
		t.Fatal(err)
	}
	if man.Format != archiveFormat || man.Items != 1200 || man.Collection != EvalItemsCollection {
		t.Fatalf("bad manifest: %+v", man)
	}
	got, err := ar.LoadRun(ctx, "big")
	if err != nil {
		t.Fatalf("load oversized run: %v", err)
	}
	if len(got.Results) != 1200 || got.Results[900].Answer != state.Results[900].Answer {
		t.Fatalf("round trip lost data: %d results", len(got.Results))
	}
	if got.ItemsDone != 1200 || got.Arm != "bm25" {
		t.Fatalf("manifest fields lost: %+v", got)
	}
}

// append-only：第二次存更多题时，只写新增的那些（写放大从 O(n²) 降到 O(1)/题）。
func TestArchiveAppendsOnlyNewItems(t *testing.T) {
	p, err := store.Open("", true)
	if err != nil {
		t.Fatal(err)
	}
	cp := &countingPort{Port: p}
	ar := NewArchive(cp)
	ctx := context.Background()

	if err := ar.SaveRun(ctx, bigRun(100)); err != nil {
		t.Fatal(err)
	}
	first := cp.structPuts
	if first != 100 {
		t.Fatalf("first save must write 100 item docs, wrote %d", first)
	}
	if err := ar.SaveRun(ctx, bigRun(250)); err != nil {
		t.Fatal(err)
	}
	if got := cp.structPuts - first; got != 150 {
		t.Fatalf("second save must write only the 150 new items, wrote %d", got)
	}
	state, err := ar.LoadRun(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Results) != 250 {
		t.Fatalf("want 250 items after append, got %d", len(state.Results))
	}
}

// 清单最后写：题写了、清单没更新 ⇒ 读出来还是上一份完整前缀
// （进度不领先于已落盘结果；半程被 kill 不会读到半份）。
func TestArchiveManifestIsWrittenLast(t *testing.T) {
	ar, p := newArchivePort(t)
	ctx := context.Background()
	if err := ar.SaveRun(ctx, bigRun(40)); err != nil {
		t.Fatal(err)
	}
	// 手工把更多题写进集合（模拟"题写了一半、清单还没写"）
	for i := 40; i < 90; i++ {
		doc := itemDoc{RunID: "big", Seq: i, ItemResult: ItemResult{ItemID: "late", Answer: "x"}}
		if err := p.PutStruct(ctx, EvalItemsCollection, itemID("big", i), doc); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ar.LoadRun(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 40 {
		t.Fatalf("manifest must gate the prefix: got %d results, want 40", len(got.Results))
	}
}

// 分页读：结果条数超过页大小时也要完整读回（顺序按序号）。
func TestArchivePagesThroughItems(t *testing.T) {
	ar, _ := newArchivePort(t)
	ctx := context.Background()
	state := bigRun(pageSize + 37)
	for i := range state.Results {
		state.Results[i].ItemID = itemID("seq", i) // 用序号当题号，便于检查顺序
	}
	if err := ar.SaveRun(ctx, state); err != nil {
		t.Fatal(err)
	}
	got, err := ar.LoadRun(ctx, "big")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != pageSize+37 {
		t.Fatalf("paged read lost items: %d", len(got.Results))
	}
	for i, r := range got.Results {
		if want := itemID("seq", i); r.ItemID != want {
			t.Fatalf("page order broken at %d: got %s want %s", i, r.ItemID, want)
		}
	}
}

// 旧档案兼容：单值 RunState 仍能读回来（档案格式换过两版）。
func TestArchiveReadsLegacySingleValue(t *testing.T) {
	ar, p := newArchivePort(t)
	ctx := context.Background()
	legacy, err := EncodeState(RunState{RunID: "old", Status: StatusDone, ItemsTotal: 2, Results: []ItemResult{{ItemID: "a"}, {ItemID: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.PutValue(ctx, runKey("old"), legacy); err != nil {
		t.Fatal(err)
	}
	got, err := ar.LoadRun(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || len(got.Results) != 2 || got.ItemsDone != 0 {
		t.Fatalf("legacy read wrong: %+v", got)
	}
}

func TestArchiveNotFound(t *testing.T) {
	ar, _ := newArchivePort(t)
	if _, err := ar.LoadRun(context.Background(), "missing"); err == nil {
		t.Fatal("missing run must report not-found")
	}
}

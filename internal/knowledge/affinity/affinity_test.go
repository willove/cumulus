package affinity

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/willove/cumulite"
)

// Decay: weight halves every τ days since last use.
func TestDecayHalvesPerTau(t *testing.T) {
	now := time.Now()
	if got := decay(1.0, now.Add(-float64Days(tauDays)).UnixMilli(), now); got < 0.49 || got > 0.51 {
		t.Fatalf("half-life value = %v, want ~0.5", got)
	}
	if got := decay(1.0, now.UnixMilli(), now); got < 0.999999 {
		t.Fatalf("fresh value = %v, want ~1.0 (ms truncation)", got)
	}
	if got := decay(1.0, now.Add(-10*float64Days(tauDays)).UnixMilli(), now); got > 0.01 {
		t.Fatalf("10τ value = %v, want ~0.001", got)
	}
	// Zero score and zero timestamp never resurrect weights.
	if decay(0, now.UnixMilli(), now) != 0 || decay(-1, now.UnixMilli(), now) != 0 {
		t.Fatal("non-positive scores must stay zero")
	}
	if decay(2, 0, now) != 2 {
		t.Fatal("missing timestamp falls back to the raw score")
	}
}

func float64Days(d float64) time.Duration { return time.Duration(d * 24 * float64(time.Hour)) }

// Weights: per-token normalization by its own top document; tokens with no
// presence contribute nothing; result lands in [0, #tokens].
func TestWeightsNormalizePerToken(t *testing.T) {
	now := time.Now()
	tok := &TokenDoc{ID: tokenDocID("宠物"), Token: "宠物", Docs: []DocWeight{
		{SourceID: "src:a", Score: 4, Hits: 4, LastMS: now.UnixMilli()},
		{SourceID: "src:b", Score: 1, Hits: 1, LastMS: now.UnixMilli()},
	}}
	got := weightsFrom([]*TokenDoc{tok}, []string{"宠物"}, now)
	if got["src:a"] != 1.0 {
		t.Fatalf("top doc = %v, want 1.0", got["src:a"])
	}
	if got["src:b"] != 0.25 {
		t.Fatalf("second doc = %v, want 0.25", got["src:b"])
	}
	if _, ok := got["src:zzz"]; ok {
		t.Fatal("unknown doc must not appear")
	}
}

// Two tokens sharing a document accumulate; one strong token cannot drown
// the other (each is normalized before summing).
func TestWeightsAccumulateAcrossTokens(t *testing.T) {
	now := time.Now()
	docs := []*TokenDoc{
		{ID: tokenDocID("宠物"), Token: "宠物", Docs: []DocWeight{
			{SourceID: "src:a", Score: 6, LastMS: now.UnixMilli()},
			{SourceID: "src:b", Score: 3, LastMS: now.UnixMilli()},
		}},
		{ID: tokenDocID("伤人"), Token: "伤人", Docs: []DocWeight{
			{SourceID: "src:a", Score: 2, LastMS: now.UnixMilli()},
			{SourceID: "src:c", Score: 2, LastMS: now.UnixMilli()},
		}},
	}
	got := weightsFrom(docs, []string{"宠物", "伤人"}, now)
	if got["src:a"] != 2.0 { // 6/6 + 2/2
		t.Fatalf("src:a = %v, want 2.0", got["src:a"])
	}
	if got["src:b"] != 0.5 { // 3/6
		t.Fatalf("src:b = %v, want 0.5", got["src:b"])
	}
	if got["src:c"] != 1.0 { // 2/2
		t.Fatalf("src:c = %v, want 1.0", got["src:c"])
	}
}

// Pruning: a hot token keeps only its top-N cells, strongest first.
func TestPruneKeepsTopNStrongestFirst(t *testing.T) {
	doc := &TokenDoc{Docs: []DocWeight{{SourceID: "low", Score: 1}, {SourceID: "high", Score: 9}, {SourceID: "mid", Score: 5}}}
	prune(doc, 2)
	if len(doc.Docs) != 2 || doc.Docs[0].SourceID != "high" || doc.Docs[1].SourceID != "mid" {
		t.Fatalf("pruned = %+v, want [high mid]", doc.Docs)
	}
}

// Upsert: repeated use increments rather than duplicates, and refreshes recency.
func TestUpsertIncrementsAndRefreshes(t *testing.T) {
	doc := &TokenDoc{}
	upsert(doc, "src:a", 0.4, 1000)
	upsert(doc, "src:a", 0.4, 2000)
	if len(doc.Docs) != 1 || doc.Docs[0].Score != 0.8 || doc.Docs[0].Hits != 2 || doc.Docs[0].LastMS != 2000 {
		t.Fatalf("upsert = %+v, want score .8 hits 2 last 2000", doc.Docs)
	}
}

// Outcome weights: confident answers teach, weak ones still leave a trace.
func TestOutcomeWeight(t *testing.T) {
	for conf, want := range map[float64]float64{0.9: 1.0, 0.7: 1.0, 0.5: 0.4, 0.4: 0.4, 0.2: 0.15, 0.0: 0.15} {
		if got := OutcomeWeight(conf); got != want {
			t.Fatalf("OutcomeWeight(%v) = %v, want %v", conf, got, want)
		}
	}
}

// TrimTokens: sub-2-rune tokens connect everything and are dropped.
func TestTrimTokens(t *testing.T) {
	got := TrimTokens([]string{"宠物", "的", "宠物", "Pet", "a"}, 0)
	if len(got) != 2 || got[0] != "宠物" || got[1] != "pet" {
		t.Fatalf("trimmed = %v, want [宠物 pet]", got)
	}
}

func TestTrimTokensCap(t *testing.T) {
	got := TrimTokens([]string{"aa", "bb", "cc"}, 2)
	if len(got) != 2 {
		t.Fatalf("capped = %v, want 2 entries", got)
	}
}

// weightsFrom is Weights over pre-loaded docs (no store): the store glue is
// covered by the e2e gate; the scoring math is what must never drift.
func weightsFrom(docs []*TokenDoc, tokens []string, now time.Time) map[string]float64 {
	out := map[string]float64{}
	byTok := map[string]*TokenDoc{}
	for _, d := range docs {
		byTok[d.Token] = d
	}
	for _, tok := range tokens {
		doc := byTok[tok]
		if doc == nil || len(doc.Docs) == 0 {
			continue
		}
		var best float64
		for _, dw := range doc.Docs {
			if v := decay(dw.Score, dw.LastMS, now); v > best {
				best = v
			}
		}
		if best <= 0 {
			continue
		}
		for _, dw := range doc.Docs {
			if v := decay(dw.Score, dw.LastMS, now); v > 0 {
				out[dw.SourceID] += v / best
			}
		}
	}
	return out
}

var _ = context.Background // store paths are exercised end-to-end

// The serve face constructs a store per request, so Record's read-modify-write
// must serialize ACROSS instances: with only a per-instance mutex, concurrent
// Records for one token loaded the same document and the later save erased the
// earlier increments — learning that silently vanished.
func TestRecordSerializesAcrossStoreInstances(t *testing.T) {
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	ctx := context.Background()
	if err := engine.EnsureCollection(ctx, Collection); err != nil {
		t.Fatal(err)
	}
	const n = 128
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewLedger(portAdapter{engine}, Collection) // per-request construction
			<-start
			if err := s.Record(ctx, []string{"宠物"}, []string{"src:a"}, 1.0, time.Now()); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	var td TokenDoc
	if err := (portAdapter{engine}).GetStruct(ctx, Collection, "aff|宠物", &td); err != nil {
		t.Fatal(err)
	}
	if len(td.Docs) != 1 {
		t.Fatalf("want one ledger cell, got %v", td.Docs)
	}
	if hits := td.Docs[0].Hits; hits != int64(n) {
		t.Fatalf("hits = %v, want %d — concurrent Records lost increments", hits, n)
	}
}

// portAdapter 让 cumulite.Engine 满足 store.Port（原版只用到 Get/Insert/Replace，
// next 的 Port 多两个方法）——测试里用它把原版的集成测试原样搬过来。
type portAdapter struct{ e *cumulite.Engine }

func (a portAdapter) EnsureCollection(ctx context.Context, c string) error {
	return a.e.EnsureCollection(ctx, c)
}
func (a portAdapter) PutStruct(ctx context.Context, coll, id string, doc any) error {
	b, err := toMap(doc)
	if err != nil {
		return err
	}
	if _, err := a.e.GetDocument(ctx, coll, id); err == nil {
		_, err := a.e.ReplaceDocument(ctx, coll, id, b)
		return err
	}
	_, err = a.e.Insert(ctx, coll, []map[string]any{b})
	return err
}
func (a portAdapter) GetStruct(ctx context.Context, coll, id string, out any) error {
	d, err := a.e.GetDocument(ctx, coll, id)
	if err != nil {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func (a portAdapter) Delete(ctx context.Context, coll, id string) error {
	_, err := a.e.GetDocument(ctx, coll, id)
	if err != nil {
		return nil
	}
	_, err = a.e.DeleteDocument(ctx, coll, id)
	return err
}
func (a portAdapter) PutValue(context.Context, string, []byte) error { return nil }
func (a portAdapter) GetValue(context.Context, string) ([]byte, error) {
	return nil, errors.New("not found")
}
func (a portAdapter) Query(context.Context, string, map[string]any, int, int, any) (int, error) {
	return 0, nil
}
func (a portAdapter) Health(ctx context.Context) error {
	_, err := a.e.Health(ctx)
	return err
}
func (a portAdapter) ListIDs(context.Context, string, int) ([]string, error) {
	return nil, nil
}

// toMap 把结构体转成 cumulite 吃的 map（通过 JSON，两边键名一致）。
func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

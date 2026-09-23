package kb

import (
	"context"
	"testing"

	"github.com/cumubase/ask/internal/source"
)

type fakeReader struct {
	srcs  []source.Source
	calls int
}

func (f *fakeReader) SourcesByIDs(_ context.Context, ids []string) ([]source.Source, error) {
	f.calls++
	byID := map[string]source.Source{}
	for _, s := range f.srcs {
		byID[s.ID] = s
	}
	var out []source.Source
	for _, id := range ids {
		if s, ok := byID[id]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// G2: a warm prior validates against ONLY the anchored docs — the reader is
// asked for those ids and the reuse succeeds without any full corpus.
func TestTryReuseNarrowValidatesAnchoredOnly(t *testing.T) {
	ctx := context.Background()
	src := source.New("手册", "md", "", "m", "zh", "连接池最大 128，超时 30 秒。", nil)
	noise := source.New("无关", "md", "", "n", "zh", "完全不相干的内容。", nil)
	e := New(fastStub(), clusterStub(), embedStub())
	reader := &fakeReader{srcs: []source.Source{src}} // noise deliberately absent
	e.SourceReader = reader

	// Seed the cluster through the normal path (full corpus available).
	if _, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{src, noise}); err != nil {
		t.Fatal(err)
	}
	before := reader.calls

	res, narrow, ok, err := e.TryReuseNarrow(ctx, "连接池最大连接数是多少")
	if err != nil || !ok {
		t.Fatalf("narrow reuse must hit: ok=%v err=%v", ok, err)
	}
	if !res.Reused || res.ClusterID == "" || len(res.Answer.Samples) == 0 {
		t.Fatalf("narrow reuse result must carry the cached evidence: %+v", res)
	}
	if reader.calls != before+1 {
		t.Fatalf("exactly one narrow read expected: %d → %d", before, reader.calls)
	}
	if len(narrow) != 1 || narrow[0].ID != src.ID {
		t.Fatalf("narrow set must be the anchored docs only: %+v", narrow)
	}
}

// G2 guard: when the anchored source is gone, the narrow read cannot validate
// the prior — reuse must NOT fire (the caller falls back to the full path,
// which self-heals).
func TestTryReuseNarrowRefusesWhenAnchorGone(t *testing.T) {
	ctx := context.Background()
	src := source.New("手册", "md", "", "m", "zh", "连接池最大 128。", nil)
	e := New(fastStub(), clusterStub(), embedStub())
	e.SourceReader = &fakeReader{srcs: []source.Source{src}}
	if _, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{src}); err != nil {
		t.Fatal(err)
	}
	// The source disappears from the corpus.
	e.SourceReader = &fakeReader{}
	if _, _, ok, err := e.TryReuseNarrow(ctx, "连接池最大连接数是多少"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("prior with a vanished anchor must not reuse")
	}
}

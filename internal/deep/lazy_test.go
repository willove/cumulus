package deep

import (
	"context"
	"strings"
	"testing"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/kb"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

type countingReader struct {
	srcs  []source.Source
	calls int
}

func (r *countingReader) SourcesByIDs(_ context.Context, ids []string) ([]source.Source, error) {
	r.calls++
	byID := map[string]source.Source{}
	for _, s := range r.srcs {
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

// end-to-end at the engine level: a warm hit must never materialize the
// full corpus (loader stays at one call — the cold first ask).
func TestAskLazySkipsCorpusLoadOnWarmHit(t *testing.T) {
	ctx := context.Background()
	src := source.New("手册", "md", "", "m", "zh", "连接池最大 128，超时 30 秒。", nil)
	kbE := kb.New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}),
		cluster.NewMemory(), cluster.Local{N: 64})
	kbE.SourceReader = &countingReader{srcs: []source.Source{src}}
	e := New(kbE, NewMemoryConflict())

	loads := 0
	load := func(context.Context) ([]source.Source, error) {
		loads++
		return []source.Source{src}, nil
	}

	if _, err := e.AskLazy(ctx, "连接池最大连接数是多少", load); err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("cold ask must load the corpus once, got %d", loads)
	}
	res, err := e.AskLazy(ctx, "连接池最大连接数是多少", load)
	if err != nil {
		t.Fatal(err)
	}
	if loads != 1 {
		t.Fatalf("warm hit must not load the corpus: loads=%d", loads)
	}
	if !res.Reused {
		t.Fatalf("second ask must reuse: %+v", res)
	}
	if len(res.Citations.Refs) == 0 {
		t.Fatal("reused answer must still carry resolved citations")
	}
}

// guard: when the reuse attempt escalates (multi-fact, incomplete cover),
// the lazy path must pay for the corpus exactly then.
func TestAskLazyLoadsCorpusWhenEscalating(t *testing.T) {
	ctx := context.Background()
	a := source.New("薄A", "md", "", "a", "zh", "连接池最大 128。", nil)
	b := source.New("薄B", "md", "", "b", "zh", "超时时间是 30 秒。", nil)
	kbE := kb.New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128", "超时"}}),
		cluster.NewMemory(), cluster.Local{N: 64})
	kbE.SourceReader = &countingReader{srcs: []source.Source{a, b}}
	e := New(kbE, NewMemoryConflict())

	loads := 0
	load := func(context.Context) ([]source.Source, error) {
		loads++
		return []source.Source{a, b}, nil
	}
	// Seed a single-fact cluster, then ask the multi-fact question: the
	// reuse attempt must fail the cover gate and escalate.
	if _, err := e.AskLazy(ctx, "连接池最大是多少", load); err != nil {
		t.Fatal(err)
	}
	before := loads
	res, err := e.AskLazy(ctx, "连接池最大是多少 以及 超时多久", load)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated {
		t.Fatal("multi-fact follow-up must escalate")
	}
	if loads != before+1 {
		t.Fatalf("escalation must load the corpus exactly once: %d → %d", before, loads)
	}
	if !strings.Contains(res.Answer.Summary, "128") {
		t.Fatalf("escalated answer must cite the corpus: %q", res.Answer.Summary[:80])
	}
}

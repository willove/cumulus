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

type stubRewriter struct{ out string }

func (s stubRewriter) Rewrite(_ context.Context, _ []string, _ string) (string, error) {
	return s.out, nil
}

// Ask must search on the rewritten (standalone) query.
func TestAskUsesRewrittenQuery(t *testing.T) {
	ctx := context.Background()
	fe := fast.New(mcs.KeywordScorer{})
	e := New(kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64}), NewMemoryConflict())
	e.History = []string{"连接池最大连接数是多少"}
	e.HistoryRewriter = stubRewriter{out: "连接池最大连接数是多少"}
	srcs := []source.Source{source.New("手册", "md", "", "m", "zh",
		"连接池最大 128，超时 30 秒。\n", nil)}
	// Deliberately vague follow-up: only the rewritten query can retrieve.
	res, err := e.Ask(ctx, "那它最大是多少来着", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer.Query != "连接池最大连接数是多少" {
		t.Fatalf("rewritten query must drive retrieval: %+v", res.Answer)
	}
	if !strings.Contains(res.Answer.Summary, "128") {
		t.Fatalf("summary must answer the rewritten query: %q", res.Answer.Summary)
	}
}

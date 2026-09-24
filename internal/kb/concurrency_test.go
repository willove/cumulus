package kb

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
	"github.com/willove/cumulite"
)

// Concurrent writes to one topic must not lose an update. Choosing a fold
// target is a read-modify-write of the cluster snapshot, so without the
// engine's write lock two goroutines read the same candidate set, both decide
// to create the cluster (one deterministic id) and one snapshot is discarded.
func TestConcurrentWritesKeepEveryUpdate(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := engine.EnsureCollection(ctx, "ask_clusters"); err != nil {
		t.Fatal(err)
	}
	store := cluster.NewCumuStore(engine, "ask_clusters")
	e := New(fastStub(), store, embedStub())
	srcs := []source.Source{source.New("手册", "md", "", "cfg", "zh", "连接池最大 128，超时 30 秒。", nil)}
	// One query yields one topic key and therefore one cluster id, so every
	// concurrent write below targets the same cluster.
	query := "连接池最大连接数是多少"
	qe, err := e.embed(ctx, query)
	if err != nil {
		t.Fatal(err)
	}

	const turns = 8
	var wg sync.WaitGroup
	for i := 0; i < turns; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ans := fast.Answer{
				Query: query, Mode: fast.ModeFAST, SourceID: srcs[0].ID,
				Samples: []mcs.Sample{{
					Source: srcs[0].ID, Start: 0, End: 18,
					Content: "连接池最大 128，超时 30 秒。", Score: 8,
				}},
				Summary:    fmt.Sprintf("【摘要】%s 第%d次", query, i),
				Confidence: 0.6,
			}
			if _, err := e.saveAnswer(ctx, ans, srcs, qe, false); err != nil {
				t.Errorf("saveAnswer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	all, err := store.All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("clusters = %d, want the concurrent writes folded into one", len(all))
	}
	for i := 0; i < turns; i++ {
		if want := fmt.Sprintf("第%d次", i); !strings.Contains(all[0].Content, want) {
			t.Fatalf("update %q is missing from the cluster — a concurrent write was lost: %q", want, all[0].Content)
		}
	}
}

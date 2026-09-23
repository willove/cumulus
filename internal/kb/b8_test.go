package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

// warm-prior validation: after the source is updated, the old cluster's
// evidence no longer pins back — reuse must be refused, the cluster marked
// 待复核 (emerging), and the L0 path must serve the FRESH answer.
func TestStalePriorFallsToL0AndSelfHeals(t *testing.T) {
	ctx := context.Background()
	d1 := "关键配置：连接池最大 128，超时 30 秒。"
	d2 := "关键配置：连接池最大 256，超时 45 秒。"
	src1 := source.New("手册", "md", "", "k", "zh", d1, nil)
	src2 := source.New("手册", "md", "", "k", "zh", d2, nil)
	if src1.ID == src2.ID {
		t.Fatal("content-addressed ids must differ after update")
	}
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "最大", "超时", "256", "45"}}),
		cluster.NewMemory(), cluster.Local{N: 64})

	r1, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{src1})
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Persisted || r1.ClusterID == "" {
		t.Fatalf("first ask must persist a cluster: %+v", r1)
	}

	r2, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{src2})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Reused {
		t.Fatalf("stale prior must not be reused: %+v", r2)
	}
	if !strings.Contains(r2.Answer.Summary, "256") {
		t.Fatalf("L0 must serve the fresh answer: %q", r2.Answer.Summary)
	}
	if !r2.Merged || r2.ClusterID != r1.ClusterID {
		t.Fatalf("fresh answer must fold back into the same cluster (self-heal): %+v", r2)
	}
	all, _ := e.Store.All(ctx)
	for _, c := range all {
		if c.ID == r1.ClusterID && c.Lifecycle != cluster.LifecycleEmerging {
			t.Fatalf("stale prior must be marked 待复核: %+v", c.Lifecycle)
		}
	}
	r3, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{src2})
	if err != nil {
		t.Fatal(err)
	}
	if !r3.Reused || r3.Answer.Summary != r2.Answer.Summary || strings.Contains(r3.Answer.Summary, "128") {
		t.Fatalf("healed reuse must contain only the fresh answer: %+v", r3)
	}
	if r3.Answer.Confidence != r2.Answer.Confidence || len(r3.Answer.Samples) != len(r2.Answer.Samples) {
		t.Fatalf("healed reuse must preserve fresh confidence and all evidence: %+v", r3)
	}
}

// A prior whose evidence still pins back stays reusable (control).
func TestFreshPriorStillReusable(t *testing.T) {
	ctx := context.Background()
	src := source.New("手册", "md", "", "k", "zh", "关键配置：连接池最大 128，超时 30 秒。", nil)
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "最大"}}),
		cluster.NewMemory(), cluster.Local{N: 64})
	if _, err := e.Ask(ctx, "连接池最大连接数是多少", []source.Source{src}); err != nil {
		t.Fatal(err)
	}
	r2, err := e.Ask(ctx, "连接池最大连接数是多大", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Reused {
		t.Fatalf("valid prior must stay reusable: %+v", r2)
	}
}

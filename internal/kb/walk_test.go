package kb

import (
	"context"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/graph"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// The P4 pathway upgrade is driven by REPEATED WALKS of the same directed
// link: ask A, ask B, ask A, ask B — the second A→B traversal upgrades the
// query_seq edge to a pathway. Walking is the only derivation source (no
// LLM guessing), so this gate is the pathway contract.
func TestRepeatedWalkUpgradesPathway(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"路由器", "交换机"}}), st, cluster.Local{N: 64})
	srcs := []source.Source{
		source.New("路由器", "md", "file://r", "r", "zh", "路由器基本配置步骤", nil),
		source.New("交换机", "md", "file://s", "s", "zh", "交换机基本配置步骤", nil),
	}
	var a, b string
	for i, q := range []string{"路由器基本配置步骤", "交换机基本配置步骤", "路由器基本配置步骤", "交换机基本配置步骤"} {
		r, err := e.Ask(ctx, q, srcs)
		if err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
		if r.ClusterID == "" {
			t.Fatalf("ask %d (%q): no cluster", i, q)
		}
		if i == 0 {
			a = r.ClusterID
		}
		if i == 1 {
			b = r.ClusterID
		}
	}
	if a == "" || b == "" || a == b {
		t.Fatalf("two distinct clusters required: %s / %s", a, b)
	}
	edges := graphEdges(t, e, a)
	var toB []graph.Edge
	for _, ed := range edges {
		if ed.To == b && ed.Source == graph.SourceQuerySeq {
			toB = append(toB, ed)
		}
	}
	if len(toB) != 1 {
		t.Fatalf("want exactly one query_seq edge A→B, got %+v", edges)
	}
	if toB[0].Kind != graph.KindPathway || toB[0].Hits < graph.PathwayMinHits {
		t.Fatalf("second walk must upgrade to pathway: %+v", toB[0])
	}
	if toB[0].Reason == "" {
		t.Fatalf("pathway edge must record its walk count: %+v", toB[0])
	}
}

// A single walk stays a plain weak edge — the upgrade needs repetition.
func TestSingleWalkStaysWeak(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"路由器", "交换机"}}), st, cluster.Local{N: 64})
	srcs := []source.Source{
		source.New("路由器", "md", "file://r", "r", "zh", "路由器基本配置步骤", nil),
		source.New("交换机", "md", "file://s", "s", "zh", "交换机基本配置步骤", nil),
	}
	var a, b string
	for i, q := range []string{"路由器基本配置步骤", "交换机基本配置步骤"} {
		r, err := e.Ask(ctx, q, srcs)
		if err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
		if i == 0 {
			a = r.ClusterID
		} else {
			b = r.ClusterID
		}
	}
	for _, ed := range graphEdges(t, e, a) {
		if ed.To == b && ed.Source == graph.SourceQuerySeq {
			if ed.Kind != "" || ed.Hits != 1 {
				t.Fatalf("one walk must stay weak: %+v", ed)
			}
			return
		}
	}
	t.Fatalf("no query_seq edge A→B after two asks")
}

// mapCursor is an in-memory LastClusterCursor: the production wiring is
// KV-backed, the semantics under test are the same.
type mapCursor struct{ id string }

func (m *mapCursor) LoadLastCluster(context.Context) (string, error) { return m.id, nil }
func (m *mapCursor) SaveLastCluster(_ context.Context, id string) error {
	m.id = id
	return nil
}

// With a cursor, the warm reuse path no longer masks the previous cluster:
// a reuse bumps its own cluster (so the updated_at fallback sees prev ==
// cur and never links), while the cursor keeps the true ask order. The
// alternating walk then links BOTH directions and upgrades A→B.
func TestWalkWithCursorLinksBothDirections(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"路由器", "交换机"}}), st, cluster.Local{N: 64})
	e.Cursor = &mapCursor{}
	srcs := []source.Source{
		source.New("路由器", "md", "file://r", "r", "zh", "路由器基本配置步骤", nil),
		source.New("交换机", "md", "file://s", "s", "zh", "交换机基本配置步骤", nil),
	}
	var a, b string
	for i, q := range []string{"路由器基本配置步骤", "交换机基本配置步骤", "路由器基本配置步骤", "交换机基本配置步骤"} {
		r, err := e.Ask(ctx, q, srcs)
		if err != nil {
			t.Fatalf("ask %d: %v", i, err)
		}
		if i == 0 {
			a = r.ClusterID
		}
		if i == 1 {
			b = r.ClusterID
		}
	}
	// The reverse walk B→A exists (the updated_at fallback dropped it).
	var reverse bool
	for _, ed := range graphEdges(t, e, b) {
		if ed.To == a && ed.Source == graph.SourceQuerySeq {
			reverse = true
		}
	}
	if !reverse {
		t.Fatalf("cursor must keep the reverse walk B→A")
	}
	// And A→B has been walked twice → pathway.
	for _, ed := range graphEdges(t, e, a) {
		if ed.To == b && ed.Source == graph.SourceQuerySeq {
			if ed.Kind != graph.KindPathway || ed.Hits < 2 {
				t.Fatalf("A→B must be a walked pathway: %+v", ed)
			}
			return
		}
	}
	t.Fatalf("no query_seq edge A→B")
}

func graphEdges(t *testing.T, e *Engine, from string) []graph.Edge {
	t.Helper()
	st := e.edgeStore()
	if st == nil {
		t.Fatalf("engine has no edge store")
	}
	es, err := st.From(context.Background(), from)
	if err != nil {
		t.Fatalf("edges from %s: %v", from, err)
	}
	return es
}

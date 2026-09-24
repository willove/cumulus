package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

type memCites struct {
	edges []citesEdge
}

type citesEdge struct {
	clusterID, sourceID string
	start, end          int
}

func (m *memCites) SaveCite(_ context.Context, clusterID, sourceID string, start, end int, _ float64) error {
	m.edges = append(m.edges, citesEdge{clusterID, sourceID, start, end})
	return nil
}

// Fresh answers write cluster → source evidence edges (clus_cites).
func TestFreshClusterWritesCites(t *testing.T) {
	ctx := context.Background()
	body := strings.Repeat("填充 padding padding。\n", 30) + "关键配置：连接池最大 128。\n"
	src := source.New("手册", "md", "", "m", "zh", body, nil)
	cites := &memCites{}
	e := New(fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}}), cluster.NewMemory(), cluster.Local{N: 64})
	e.Cites = cites
	res, err := e.Ask(ctx, "连接池最大连接数", []source.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if res.ClusterID == "" {
		t.Fatal("cluster not persisted")
	}
	if len(cites.edges) == 0 {
		t.Fatal("cites not written on fresh cluster")
	}
	c := cites.edges[0]
	if c.clusterID != res.ClusterID || c.sourceID != src.ID {
		t.Fatalf("cite must point at cluster→source: %+v", c)
	}
	if c.start >= c.end {
		t.Fatalf("cite window invalid: %+v", c)
	}
}

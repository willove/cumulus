package kb

import (
	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
)

// Offline engine parts shared by the reuse tests: deterministic FAST tier,
// in-memory cluster store, local hash embedder.
func fastStub() *fast.Engine {
	return fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
}

func clusterStub() *cluster.Memory { return cluster.NewMemory() }

func embedStub() cluster.Embedder { return cluster.Local{N: 64} }

package kb

import (
	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/mcs"
)

// Offline engine parts shared by the G2 reuse tests: deterministic FAST tier,
// in-memory cluster store, local hash embedder.
func fastStub() *fast.Engine {
	return fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128"}})
}

func clusterStub() *cluster.Memory { return cluster.NewMemory() }

func embedStub() cluster.Embedder { return cluster.Local{N: 64} }

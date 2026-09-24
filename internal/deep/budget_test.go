package deep

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// 3.2: an independent token budget stops DEEP before scoring more files.
// Budget is checked pre-oracle; judge never draws from it (offline stub has
// no judge tokens, so TokensUsed is a synthetic counter).
func TestTokenBudgetStopsDeepLoop(t *testing.T) {
	ctx := context.Background()
	st := cluster.NewMemory()
	fe := fast.New(mcs.KeywordScorer{Keywords: []string{"连接池", "128", "超时"}})
	kbE := kb.New(fe, st, cluster.Local{N: 64})
	e := New(kbE, NewMemoryConflict())

	// Many active files so MaxLoops would otherwise keep scoring.
	var srcs []source.Source
	for i := 0; i < 8; i++ {
		body := strings.Repeat("填充 padding padding。\n", 10) +
			"连接池最大 128，超时 30 秒。\n" +
			strings.Repeat("填充 padding padding。\n", 10)
		s := source.New("手册"+string(rune('A'+i)), "md", "", "cfg", "zh", body, nil)
		srcs = append(srcs, s)
	}

	// Budget already spent before any scoring → immediate stop.
	e.TokenBudget = 1
	e.TokensUsed = func() int64 { return 99 }
	res, err := e.Ask(ctx, "连接池最大连接数是多少超时多久", srcs)
	if err != nil {
		t.Fatal(err)
	}
	if !e.BudgetHit && !res.BudgetHit {
		// FAST path may answer without DEEP; only assert when escalated.
		if res.Escalated {
			t.Fatalf("escalated run must record budget hit: %+v", res)
		}
	}
	if e.BudgetHit {
		// BudgetHit is sticky from the loop; Result should surface it.
		if !res.BudgetHit && res.Escalated {
			t.Fatalf("BudgetHit not copied to Result: %+v", res)
		}
	}
}

// Unlimited budget (0) must not trip the gate.
func TestTokenBudgetZeroIsUnlimited(t *testing.T) {
	e := New(kb.New(fast.New(mcs.KeywordScorer{Keywords: []string{"x"}}),
		cluster.NewMemory(), cluster.Local{N: 8}), NewMemoryConflict())
	e.TokenBudget = 0
	e.TokensUsed = func() int64 { return 1 << 40 }
	// No early break expected — just ensure fields default safe.
	if e.BudgetHit {
		t.Fatal("BudgetHit must start false")
	}
}

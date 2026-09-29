package deep

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/cluster"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// TestThresholdForLegacyDefault pins the byte-identical default: base 0.35,
// +0.05 per extra fact, and the cap never binds at defaults (the old absolute
// 0.6 cap was unreachable: 0.35+0.15 < 0.6).
func TestThresholdForLegacyDefault(t *testing.T) {
	t.Setenv("CLUS_ESCALATE_BELOW", "")
	e := New(nil, NewMemoryConflict())
	for n, want := range map[int]float64{1: 0.35, 2: 0.40, 4: 0.50, 7: 0.50} {
		fx := make([]facts.Fact, n)
		if got := e.thresholdFor(fx); math.Abs(got-want) > 1e-9 {
			t.Fatalf("thresholdFor(%d facts) = %v, want %v", n, got, want)
		}
	}
}

// TestThresholdForRaisedLine pins the knob: a raised base lifts every band,
// gamma still adds headroom, the relative cap keeps DEEP reachable (≤0.95),
// and — unlike the old absolute 0.6 cap — never LOWERS the line under the base.
func TestThresholdForRaisedLine(t *testing.T) {
	t.Setenv("CLUS_ESCALATE_BELOW", "0.85")
	e := New(nil, NewMemoryConflict())
	for n, want := range map[int]float64{1: 0.85, 2: 0.90, 4: 0.95, 7: 0.95} {
		fx := make([]facts.Fact, n)
		if got := e.thresholdFor(fx); got != want {
			t.Fatalf("raised thresholdFor(%d facts) = %v, want %v", n, got, want)
		}
	}
	// Garbage and out-of-range values fall back to the historical constant.
	for _, v := range []string{"abc", "-1", "1.5"} {
		t.Setenv("CLUS_ESCALATE_BELOW", v)
		if got := New(nil, NewMemoryConflict()).EscalateBelow; got != EscalateBelow {
			t.Fatalf("env %q: EscalateBelow = %v, want the 0.35 default", v, got)
		}
	}
}

// midBandScorer hands every window the same mid-band score so the FAST
// answer's confidence lands in the measured leak band (0.65–0.75, correct
// rate 9/31) regardless of sampling.
type midBandScorer struct{}

func (midBandScorer) Score(_ context.Context, _ string, _ mcs.Sample) (float64, string, error) {
	return 6, "mid-band", nil
}

// TestMidBandEscalatesUnderRaisedLine is the calibration property the knob
// exists for: a mid-band FAST answer (historically served, measured 29%
// correct) must escalate to DEEP when the line is raised — and must stay
// served under the default so the A/B's control arm is today's behaviour.
func TestMidBandEscalatesUnderRaisedLine(t *testing.T) {
	q := "连接池最大连接数是多少"
	docs := func() []source.Source {
		out := make([]source.Source, 0, 4)
		for _, k := range []string{"blk/a", "blk/b", "blk/c", "blk/d"} {
			body := q + " 相关记载。" + strings.Repeat("正文内容。", 3000)
			out = append(out, source.New(k, "md", "file://"+k, k, "zh", body, nil))
		}
		return out
	}()

	run := func(env string) string {
		t.Setenv("CLUS_ESCALATE_BELOW", env)
		sc := midBandScorer{}
		fe := fast.New(sc)
		k := kb.New(fe, cluster.NewMemory(), cluster.Local{N: 64})
		e := New(k, NewMemoryConflict())
		e.Scorer = sc
		res, err := e.Ask(context.Background(), q, docs)
		if err != nil {
			t.Fatalf("Ask (env=%q): %v", env, err)
		}
		return res.Mode
	}

	if got := run(""); got != fast.ModeFAST {
		t.Fatalf("default line: mid-band answer served as %v, want FAST (control arm = today)", got)
	}
	if got := run("0.85"); got != ModeDEEP {
		t.Fatalf("raised line: mid-band answer served as %v, want DEEP escalation", got)
	}
}

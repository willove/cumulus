package deep

import (
	"context"
	"os"
	"strings"

	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/mcs"
)

// ConsistencyChecker is the pre-synthesis evidence-agreement gate (收益层 2,
// CLUS_SYNTH_CONSISTENCY, default OFF). One call sees the kept windows
// TOGETHER and reports whether they agree on the answer.
type ConsistencyChecker interface {
	CheckConsistency(ctx context.Context, query string, windows []mcs.Sample) (agree bool, conflictSummary string, err error)
}

// consistencyEnabled reports whether the gate is armed.
func consistencyEnabled() bool { return os.Getenv("CLUS_SYNTH_CONSISTENCY") == "1" }

// consistencyGateWindows picks the windows the checker sees: at least two
// DISTINCT sources at/above the cover line (a single doc cannot disagree
// with itself), at most the top cover-grade window per source, capped at
// four sources so the call cannot grow with the crawl. nil means "do not
// check" — the single-source fast path never pays for the gate.
func consistencyGateWindows(kept []mcs.Sample) []mcs.Sample {
	bySrc := map[string]mcs.Sample{}
	order := []string{}
	for _, sm := range kept {
		if sm.Score < facts.CoverScore {
			continue
		}
		if _, ok := bySrc[sm.Source]; !ok {
			order = append(order, sm.Source)
			bySrc[sm.Source] = sm
			continue
		}
		if sm.Score > bySrc[sm.Source].Score {
			bySrc[sm.Source] = sm
		}
	}
	if len(order) < 2 {
		return nil
	}
	if len(order) > 4 {
		order = order[:4]
	}
	out := make([]mcs.Sample, len(order))
	for i, src := range order {
		out[i] = bySrc[src]
	}
	return out
}

// consistencyGate runs the check when armed. Best-effort by design but
// visible: an erroring checker must not break the answer, and the Verbose
// log is where a silently-dead gate would first show.
func (e *Engine) consistencyGate(ctx context.Context, query string, kept []mcs.Sample) (bool, string) {
	if !consistencyEnabled() || e.Consistency == nil {
		return false, ""
	}
	wins := consistencyGateWindows(kept)
	if wins == nil {
		return false, ""
	}
	agree, why, err := e.Consistency.CheckConsistency(ctx, query, wins)
	if err != nil {
		if e.Verbose != nil {
			e.Verbose("consistency gate: checker failed (answer stands unmarked): %v", err)
		}
		return false, ""
	}
	if agree {
		return false, ""
	}
	if e.Verbose != nil {
		e.Verbose("consistency gate: contested — %s", why)
	}
	return true, why
}

// markContested deterministically rewrites a contested answer: the flag for
// the wire, and a one-line divergence statement prefixed to the summary so
// the reader sees BOTH claims before the model's pick. Deterministic prefix,
// no synth-prompt change — the failure form changes from confident-wrong to
// explicitly-hedged, which is the property the adversarial yardsticks
// measure (23/30 and 29/30 confident-wrong, 0 hedged, 2026-09-29/30).
func markContested(ans *fast.Answer, why string) {
	ans.Contested = true
	trim := strings.TrimSpace(why)
	if trim == "" {
		trim = "证据片段对同一事实点给出不同答案"
	}
	ans.Summary = "⚠ 语料证据存在分歧：" + trim + "。以下回答依据其中一方，请对照来源核实。\n\n" + ans.Summary
}

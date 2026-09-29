package deep

import (
	"context"
	"os"
	"strings"

	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// ConsistencyChecker is the pre-synthesis evidence-agreement gate (收益层 2,
// CLUS_SYNTH_CONSISTENCY, default OFF). One call sees the kept windows
// TOGETHER and reports whether they agree on the answer.
type ConsistencyChecker interface {
	CheckConsistency(ctx context.Context, query string, windows []mcs.Sample) (agree bool, conflictSummary string, err error)
}

// consistencyEnabled reports whether the gate is armed.
func consistencyEnabled() bool { return os.Getenv("CLUS_SYNTH_CONSISTENCY") == "1" }

// headRunes is how much of each source's head rides along with its top
// window: self-describing metadata ("《T》 作者：X。") lives at the head, and
// the agreeprobe discrimination (2026-09-30) measured body-only windows at
// 1/8 flagged vs 8/8 with the head included — window-miss, not model-blind.
const headRunes = 300

// consistencyGateWindows picks the windows the checker sees: at least two
// DISTINCT sources at/above the cover line (a single doc cannot disagree
// with itself), the top cover-grade window per source PLUS the source's head
// (where the divergence signal lives), capped at three sources so the call
// cannot grow with the crawl. nil means "do not check".
func consistencyGateWindows(kept []mcs.Sample, corpus []source.Source) []mcs.Sample {
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
	if len(order) > 3 {
		order = order[:3]
	}
	byID := map[string]source.Source{}
	for _, s := range corpus {
		byID[s.ID] = s
	}
	out := make([]mcs.Sample, 0, 2*len(order))
	for _, src := range order {
		top := bySrc[src]
		out = append(out, top)
		s, ok := byID[src]
		if !ok {
			continue
		}
		// The head rides along unless the top window already covers it.
		if top.Start < headRunes {
			continue
		}
		r := []rune(s.Body)
		end := headRunes
		if end > len(r) {
			end = len(r)
		}
		out = append(out, mcs.Sample{Source: src, Start: 0, End: end, Content: string(r[:end]), Score: top.Score})
	}
	return out
}

// consistencyGate runs the check when armed. Best-effort by design but
// visible: an erroring checker must not break the answer, and the Verbose
// log is where a silently-dead gate would first show.
func (e *Engine) consistencyGate(ctx context.Context, query string, kept []mcs.Sample, corpus []source.Source) (bool, string) {
	if !consistencyEnabled() || e.Consistency == nil {
		return false, ""
	}
	wins := consistencyGateWindows(kept, corpus)
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

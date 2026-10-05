// deeploop — DEEP 循环本体：收容/扩征/自纠采样、预算与邻接扩征、渲染与分步上报。
// 从 deep.go 纯搬运（2026-10-04 拆分），无语义改动。
package deep

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// sufficientScore is the "this window is strong enough to end the loop" line.
const sufficientScore = 8.0

// sufficientLine is sufficientScore at runtime (R1 takeover point): the
// strong-window stop is the loop's main cost/quality dial, paired with
// facts.CoverScoreLine as the cover line. Default byte-identical.
func sufficientLine() float64 {
	v := strings.TrimSpace(os.Getenv("CLUS_SUFFICIENT_SCORE"))
	if v == "" {
		return sufficientScore
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 10 {
		return sufficientScore
	}
	return f
}

// budgetRiskFraction is the share of the per-query token budget past which a
// merely-covering window is accepted as the answer. It is a fraction, not a
// token count, so it scales with whatever budget an operator sets.
const budgetRiskFraction = 0.6

// DefaultTokenBudget is the per-query cap applied when an operator sets none.
//
// It is derived, not guessed. Offline, on the pipeline's own admission order
// over a real 727k-rune novel (104 blocks, 104 hard mid-chapter queries),
// recall is R@1 51.0% / R@2 60.6% / R@3 69.2% / R@4 72.1% / R@8 76.0% /
// R@64 80.8% — the curve saturates at four documents, and 20% is unreachable
// at ANY budget. A scoring call costs ~6,665 tokens measured (73,212 tokens
// over 11 calls), so four calls is ~26,660 ≈ 27,000.
//
// Live A/B on five questions, same book, same store:
//
//	gate<6, no budget  → 47,293 tokens/query mean
//	gate<4 + 27,000    → 27,480 tokens/query mean  (−42%)
//	latency             33.0s → 17.7s (−46%)
//
// with no visible quality change (4 answered correctly, 1 refused, both arms).
//
// The attribution run matters: three of the five questions cost the SAME in
// both arms (they stop via a strong cover, the FAST tier, or candidate
// exhaustion). Only the widening-shaped ones differ, and the budget is what
// capped them — the gate change alone saved almost nothing. So this number is
// a hard ceiling, not a quality lever, and it must not be read as one.
//
// Overshoot is by design: budgetHit is consulted before each admitted file and
// one scoring batch costs ~6.7k tokens, so a query may legally land ~one batch
// over the line (measured 30,839 on a 27,000 budget, 2026-09-28). The cap
// bounds the spend to budget+one-batch, not to the budget exactly.
const DefaultTokenBudget int64 = 27_000

// adjacencyPullCap bounds the sibling hops per query: two siblings per
// covering block, and at most this many total, so a corpus of many
// covering-grade blocks cannot turn the pull into a second full crawl.
const adjacencyPullCap = 4

// adjacencyIndex answers "which sources are block i±1 of this one", built
// once per DEEP query over the full candidate list. nil (flag off, or a
// corpus with no blocks) disables the pull entirely.
type adjacencyIndex struct {
	byParent map[string]map[int]source.Source
}

func newAdjacencyIndex(srcs []source.Source) *adjacencyIndex {
	if os.Getenv("CLUS_DEEP_ADJACENCY") != "1" {
		return nil
	}
	ai := &adjacencyIndex{byParent: map[string]map[int]source.Source{}}
	for _, s := range srcs {
		parent, _, _, ok := source.BlockParent(s.Meta)
		if !ok {
			continue
		}
		idx, ok := source.BlockIndex(s)
		if !ok {
			continue
		}
		m := ai.byParent[parent]
		if m == nil {
			m = map[int]source.Source{}
			ai.byParent[parent] = m
		}
		m[idx] = s
	}
	if len(ai.byParent) == 0 {
		return nil
	}
	return ai
}

// siblings returns the i±1 blocks of s within the same parent, in index
// order. Activity is the caller's loop's own concern (it already skips
// non-active sources).
func (ai *adjacencyIndex) siblings(s source.Source) []source.Source {
	parent, _, _, ok := source.BlockParent(s.Meta)
	if !ok {
		return nil
	}
	idx, ok := source.BlockIndex(s)
	if !ok {
		return nil
	}
	m := ai.byParent[parent]
	var out []source.Source
	for _, n := range []int{idx + 1, idx - 1} { // next sibling first: the forward continuation is the measured case
		if sib, ok := m[n]; ok {
			out = append(out, sib)
		}
	}
	return out
}

// deepEvidenceRunes is the DEEP admission sampler's evidence budget in
// runes: CLUS_MCS_DEEP_EVIDENCE when set and positive, else 15000 — now
// the same whole-body budget the FAST tier uses. MEASURED 2026-09-26
// (paired single run, same frozen base, 13-query set, scripts/bench/
// results-ev15k.json vs results-head.json): 5000 cost MORE, not less —
// total wall 778s → 493s (-37%), tokens 370K → 346K (-6%), honest
// refusals 4 → 0. The mechanism: this corpus averages ~10K runes per
// document, so a 5000-rune cap cut every document at its waist and hid
// the answer's half from the scorer, and DEEP then paid for extra loops
// to find what the cap had removed. Caveat on record: one paired run —
// repeat before treating -37% as settled (R-E6).
func deepEvidenceRunes() int {
	if v := os.Getenv("CLUS_MCS_DEEP_EVIDENCE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 15000
}

// coveredFacts counts the facts a report marks covered — the marginal a
// single file's observation is measured against.
func coveredFacts(r facts.Report) int {
	n := 0
	for _, f := range r.Facts {
		if f.Covered {
			n++
		}
	}
	return n
}

// deepOutcome is one DEEP pass. It used to be nine positional returns, which
// made every call site read `_, _, _, _, _, _, _, reason, err` — the reader
// had to COUNT positions to learn that the eighth value was the stop reason.
// The five test call sites all did exactly that. Note that Admitted (the
// rank-admission set) and Corpus (what citations resolve against) are
// deliberately different things; the positional form invited conflating them.
type deepOutcome struct {
	Answer        fast.Answer
	Cover         facts.Report
	Loops         int
	Widened       int
	SelfCorrected bool
	Admitted      []string
	Corpus        []source.Source
	StopReason    string
}

// runDeep is the ReAct-shaped loop with per-fact coverage:
// sample sources → evaluate fact coverage → bounded self-correction on the
// weakest (missing) requirements → synthesize. Offline stub is deterministic.
// Returns admitted source IDs (every file the loop scored) for ir-rag 1.5
// not-retrieved decomposition.
func (e *Engine) runDeep(ctx context.Context, query string, sources []source.Source, affinity map[string]bool) (deepOutcome, error) {
	// Sampling telemetry: every admission/widen/self-correct SampleBody call
	// accumulates here and is reported once on exit (any exit path). DEEP's
	// repeated whole-body window scoring is where this corpus's token burn
	// lives, so it needs its own line in the per-stage split.
	var sampleNS int64
	// Admission pre-filter (CLUS_DEEP_SKIP_ZERO_HIT): a file sharing no
	// token with the query gets no LLM scorer call — every measured widen
	// pass burned whole-window calls on files that then scored 0. OFF by
	// default: the files this skips are exactly the lexically-unreachable
	// ones a semantic-only match could still rescue, so the operator opts
	// in knowingly.
	skipZeroHit := os.Getenv("CLUS_DEEP_SKIP_ZERO_HIT") == "1"
	qToks := mcs.Fields(query)
	if affinity == nil {
		affinity = map[string]bool{}
	}
	fx := e.decompose(ctx, query)
	// oracle hints: "f1:描述" strings let a FactAware scorer emit the
	// per-fact observation vector in the same scoring call.
	hints := make([]string, len(fx))
	for i, f := range fx {
		hints[i] = f.ID + ":" + f.Query
	}
	_, oracleMode := e.scorer().(mcs.FactAware)
	// D3: FactAware empty covers = "covered nothing"; never fall back to
	// keywords. Offline KeywordScorer keeps ReportFor's stub path.
	report := func(samples []mcs.Sample) facts.Report {
		if oracleMode {
			return facts.ReportForOracle(fx, samples)
		}
		return facts.ReportFor(fx, samples)
	}
	// kept accumulates this query's covering windows; declared before
	// newSampler because the closure reads it at call time (c_d digest).
	var kept []mcs.Sample
	newSampler := func() *mcs.Sampler {
		cfg := mcs.EnvConfig()
		// DEEP admission budget: this loop scores every admitted file, and
		// whole-body inputs made deep_sample the dominant cost (measured on
		// the live endpoint: 83.5s of a 114s query, ~15 scorer calls × up to
		// 15K-rune inputs). Feeding the query-densest deepEvidenceRunes()
		// view instead cuts the per-call input ~3x. Offsets stay exact
		// against the live body (the budget window is a real rune slice via
		// mcs' densestWindow), so citations, warm-prior validation and
		// cluster evidence binding are unaffected — only how much of each
		// file reaches the scorer changes. Never grows past EnvConfig.
		if n := deepEvidenceRunes(); n > 0 && cfg.MaxEvidence > n {
			cfg.MaxEvidence = n
		}
		smp := mcs.New(cfg, e.scorer())
		smp.FactHints = hints
		return smp
	}
	// Admission order matters at scale: on a 10k-article corpus the caller's
	// list is arbitrary (ingest order), so sampling it directly spends the
	// whole budget on the first files that happen to be there. Rank first —
	// the caller's fast engine carries the same cascade the FAST tier uses —
	// then explore in relevance order (Sirchmunk Phase-1 对齐).
	ranked := e.rankAdmission(ctx, query, sources, affinity)
	// D1: widen excludes only files this run actually attempted — not the
	// full candidate list (L1Pre=false used to pass the whole corpus in and
	// starve the extra budget).
	tried := map[string]bool{}
	widenExclude := func() map[string]bool {
		out := make(map[string]bool, len(tried))
		for id := range tried {
			out[id] = true
		}
		return out
	}

	var best fast.Answer
	loops := 0
	var bestSrc source.Source
	bestScore := -1.0
	rep := report(kept)
	// Registered after kept/rep exist so the closure reads their FINAL
	// values — the detail says what the loop admitted and what stayed
	// uncovered, i.e. the signal that drove every extra round.
	defer func() {
		if sampleNS > 0 && e.Stages != nil {
			e.Stages("deep_sample", time.Duration(sampleNS), map[string]any{
				"admitted": len(kept), "missing": rep.Missing, "facts": rep.K,
			})
		}
	}()
	// Stop-reason accounting: whichever break fires first names the exit,
	// "" means the candidates ran out (or the ctx died) with budget to spare.
	// prevP/noImprove drive the per-round pessimistic exit below.
	reason := ""
	prevP := 0.0
	noImprove := 0
	// Adjacency widen (CLUS_DEEP_ADJACENCY, default OFF): blocks are fixed-
	// rune slices of one parent, so a scene that answers a question routinely
	// straddles a block boundary — measured 2026-09-28 on the 727k-rune novel,
	// twice in one day: block 0 scored 8.0 with its window cutting off
	// mid-scene, and the continuation block (the one holding the answer's
	// second half) sat BEHIND a 0.0-scored block in the admission order. When
	// a block scores at/over the cover line, its i±1 siblings are pulled to
	// the FRONT of the exploration queue — near-zero ranking cost, bounded by
	// adjacencyPullCap. Off by default: it reorders exploration on every DEEP
	// query, so it earns its default through a paired A/B like every other
	// behavioural knob in this loop.
	adjacency := newAdjacencyIndex(sources)
	queue := ranked
	queued := map[string]bool{}
	if adjacency != nil {
		q := make([]source.Source, len(ranked))
		copy(q, ranked)
		queue = q // ranked is rankAdmission's slice; never mutate its backing
		for _, s := range q {
			queued[s.ID] = true
		}
	}
	pulled := 0
	for qi := 0; qi < len(queue); qi++ {
		s := queue[qi]
		if cancelled(ctx) {
			break
		}
		if s.Status != source.StatusActive {
			continue
		}
		if skipZeroHit && !mcs.HasAnyToken(s.Body, qToks) {
			continue
		}
		// Weakest-requirement stop: the strongest window with full
		// coverage is enough — sampling further admitted files wastes budget
		// and latency (真机: 85s/13944 tokens 空转在已答问题上).
		//
		// The second arm is the budget-aware stop (2026-07-28, perf-plan §4.9).
		// The fixed >= 8 line was calibrated on a corpus where a correct
		// answer scores well above it; on a real 727k-rune novel the block
		// holding the answer scored 5.0 while the wrong blocks scored 2.0/1.0/
		// 1.0 — clean separation, but permanently under the line, so the loop
		// could not tell it was done and kept scoring (measured: 73,212 tokens
		// for one question, ~21,000 of it after the answer was already in
		// hand).
		//
		// So: a STRONG answer always stops, and a merely COVERING one stops
		// when the query is running out of budget. That encodes "context and
		// money are finite" in the stop condition itself, instead of tuning a
		// generosity constant — and with no budget set the behaviour is exactly
		// what it was, so nothing changes until an operator sets one.
		if rep.Complete && (bestScore >= sufficientLine() ||
			(bestScore >= facts.CoverScoreLine() && e.budgetAtRisk())) {
			reason = "sufficient"
			if e.Verbose != nil {
				e.Verbose("early stop: covered, best=%.1f, files=%d, budget_at_risk=%v",
					bestScore, loops, e.budgetAtRisk())
			}
			break
		}
		loops++
		if loops > e.MaxLoops {
			reason = "budget"
			break
		}
		tried[s.ID] = true
		// Independent token stop (LENS Def 3): check BEFORE scoring this
		// file so an exhausted budget never starts another oracle batch.
		if e.budgetHit() {
			reason = "budget"
			if e.Verbose != nil {
				e.Verbose("token budget hit after %d files", loops)
			}
			break
		}
		_ts := time.Now()
		endScore := e.begin()
		samples, err := newSampler().SampleBody(ctx, query, s.Body)
		sampleNS += int64(time.Since(_ts))
		bridged := false
		bridgeBest := 0.0
		if err == nil && nonePass(samples) && strings.TrimSpace(e.SampleContext) != "" {
			if s2, err2 := newSampler().SampleBody(ctx, query+" "+e.SampleContext, s.Body); err2 == nil {
				for _, sm := range s2 {
					if sm.Score > bridgeBest {
						bridgeBest = sm.Score
					}
				}
				samples = s2
				bridged = true
			}
		}
		if e.stageTok != nil {
			endScore(&e.stageTok.Score)
		}
		if err != nil {
			if e.Verbose != nil {
				e.Verbose("file %s: sample error %v", s.BusinessKey, err)
			}
			continue
		}
		localBest := 0.0
		fileKept := 0
		for _, sm := range samples {
			if sm.Score > localBest {
				localBest = sm.Score
			}
			if sm.Score >= facts.CoverScoreLine() {
				sm.Source = s.ID
				kept = append(kept, sm)
				fileKept++
			}
		}
		if e.OnFile != nil {
			e.OnFile(s.ID, s.BusinessKey, localBest, len(samples))
		}
		if e.Verbose != nil {
			e.Verbose("file %s: windows=%d best=%.1f kept=%d", s.BusinessKey, len(samples), localBest, len(kept))
		}
		if localBest > bestScore {
			bestScore = localBest
			bestSrc = s
		}
		rep = report(kept)
		// The adjacency pull lives HERE, after the score is known and before
		// the next candidate: a covering-grade block justifies one hop to each
		// sibling, and the sibling lands at qi+1 so it is explored next. A
		// sibling already in the queue is MOVED UP (out from behind whatever
		// junk the ranker put ahead of it), not duplicated.
		if adjacency != nil && pulled < adjacencyPullCap && localBest >= facts.CoverScoreLine() {
			for _, nb := range adjacency.siblings(s) {
				if tried[nb.ID] {
					continue
				}
				if queued[nb.ID] {
					for j := qi + 1; j < len(queue); j++ {
						if queue[j].ID == nb.ID {
							queue = append(queue[:j], queue[j+1:]...)
							break
						}
					}
				}
				queued[nb.ID] = true
				pulled++
				queue = append(queue[:qi+1], append([]source.Source{nb}, queue[qi+1:]...)...)
				if e.Verbose != nil {
					e.Verbose("adjacency: pulled %s ahead of the queue (sibling of %s, best=%.1f)",
						nb.BusinessKey, s.BusinessKey, localBest)
				}
				if pulled >= adjacencyPullCap {
					break
				}
			}
		}
		// Session-bridge early exit: an anchored document (the thread's own,
		// weight ≥ 0.6) that only the fallback could sample is the answer.
		// Continuing the crawl just burns budget on documents the session
		// never endorsed — the 174s pathology this rule exists to prevent.
		if bridged && len(kept) > 0 && e.DocWeights[s.ID] >= 0.6 && bridgeBest >= bridgeFloorScore {
			reason = "session-bridge"
			if e.Verbose != nil {
				e.Verbose("file %s: session bridge ends the loop (anchored doc, bridged windows)", s.BusinessKey)
			}
			break
		}
		// Per-round pessimistic exit (u_d, Jev-Mem §3.3 的单出口在零 LLM 头上的
		// 对应物). Opt-in on the SAME knob as the FAST-boundary early refuse
		// (CLUS_EARLY_ABSTAIN): after ≥2 scored files, p_fail at/over the line
		// on consecutive NON-improving rounds ⇒ more admission buys nothing —
		// stop, and skip self-correction/widening below the same way. The
		// post-search gates still own the final refuse decision.
		if e.Abstain != nil && e.Abstain.EarlyAbove > 0 && loops >= 2 {
			mean := 0.0
			for _, sm := range kept {
				mean += sm.Score
			}
			if len(kept) > 0 {
				mean /= float64(len(kept))
			}
			conf := mcs.Confidence(mean, mcs.Coverage(query, kept))
			// Skipped mirrors the FAST-boundary feature honestly: mid-loop,
			// "skipped" means no usable window YET. Without it the head's
			// strongest failure signal (+1.80) never fires and this exit is
			// dead code.
			p := e.Abstain.PFail(abstain.FromAnswer(query, len(ranked), len(kept),
				bestScore, len(rep.Missing), conf, len(kept) == 0, false))
			switch {
			case p < e.Abstain.EarlyAbove:
				noImprove = 0
			case p >= prevP:
				noImprove++
			default:
				noImprove = 0
			}
			prevP = p
			if noImprove >= 2 {
				reason = "utility"
				if e.Verbose != nil {
					e.Verbose("utility stop: p_fail=%.2f flat for %d rounds, files=%d", p, noImprove, loops)
				}
				break
			}
		}
	}

	// Self-correction (D4): own budget, independent of admission MaxLoops.
	// Prefer files admission never reached, then re-sample tried ones with
	// the missing-fact queries. A utility stop skips it by design: the loop
	// just decided more retrieval buys nothing (opt-in trade, see the
	// per-round exit above).
	selfCorrected := false
	if reason != "utility" && !rep.Complete && e.CorrectBudget > 0 && !e.budgetHit() {
		selfCorrected = true
		var order []source.Source
		for _, s := range sources {
			if s.Status == source.StatusActive && !tried[s.ID] {
				order = append(order, s)
			}
		}
		for _, s := range sources {
			if s.Status == source.StatusActive && tried[s.ID] {
				order = append(order, s)
			}
		}
		correctUsed := 0
		// 1.7: coverage gaps ⇒ boost the global (blind-spot) arm. The lex
		// anchor arm already failed for the original wording, so self-
		// correction spends its slots on scatter/L2 exploration instead of
		// re-probing the same anchors at a different phrasing.
		gap := 0.0
		if len(fx) > 0 {
			gap = float64(len(rep.Missing)) / float64(len(fx))
		}
		exploreSampler := func() *mcs.Sampler {
			ecfg := mcs.EnvConfig()
			if n := deepEvidenceRunes(); n > 0 && ecfg.MaxEvidence > n {
				ecfg.MaxEvidence = n
			}
			smp := mcs.New(ecfg, e.scorer())
			smp.FactHints = hints
			if gap > 0 {
				smp.ExploreBoost = 1 + 2*gap // 1.0 (no gap) → 3.0 (all open)
			}
			return smp
		}
		// 2.4: MissingQueries first, then two-call complements (Jaccard-filtered)
		// so self-correction is not locked to the original wording.
		mqs := facts.MissingQueries(fx, rep)
		if e.QuerySim != nil && !e.budgetHit() {
			triedQ := make([]string, 0, len(mqs)+1)
			triedQ = append(triedQ, query)
			triedQ = append(triedQ, mqs...)
			if extra, err := e.QuerySim.Complement(ctx, query, triedQ); err == nil && len(extra) > 0 {
				mqs = append(mqs, facts.FilterDissimilar(query, triedQ, extra, 0)...)
			} else if err != nil && e.Verbose != nil {
				// Best-effort by design, but it must be visible: a silently
				// failing complement generator makes self-correction look like
				// it had nothing to add.
				e.Verbose("query sim failed, self-correction stays on missing facts: %v", err)
			}
		}
	outer_correct:
		for _, mq := range mqs {
			if cancelled(ctx) {
				break outer_correct
			}
			for _, s := range order {
				if cancelled(ctx) {
					break outer_correct
				}
				if correctUsed >= e.CorrectBudget || e.budgetHit() {
					break outer_correct
				}
				correctUsed++
				loops++
				tried[s.ID] = true
				_ts := time.Now()
				samples, err := exploreSampler().SampleBody(ctx, mq, s.Body)
				if err == nil && nonePass(samples) && strings.TrimSpace(e.SampleContext) != "" {
					if s2, err2 := exploreSampler().SampleBody(ctx, mq+" "+e.SampleContext, s.Body); err2 == nil {
						samples = s2
					}
				}
				sampleNS += int64(time.Since(_ts))
				if err != nil {
					continue
				}
				localBest := 0.0
				for _, sm := range samples {
					if sm.Score > localBest {
						localBest = sm.Score
					}
					if sm.Score >= facts.CoverScoreLine() {
						sm.Source = s.ID
						kept = append(kept, sm)
					}
				}
				if localBest > bestScore {
					bestScore = localBest
					bestSrc = s
				}
			}
		}
		rep = report(kept)
	}

	// Widening: coverage still open OR no strong window found → re-admit
	// files from the FULL corpus by fresh keyword rankings. The widening
	// allowance is deliberately independent of MaxLoops: the initial
	// admission usually spends the whole loop budget, which must not starve
	// exploration (Sirchmunk ReAct 对齐). Covers alone cannot veto widening:
	// a generous oracle can mark wrong-doc windows "complete".
	// D1: exclude = tried only (see widenExclude).
	widened := 0
	// Documents widening admitted from outside the corpus: citations must
	// resolve against them too, so the run reports what it actually sampled.
	var widenedDocs []source.Source
	if os.Getenv("CLUS_DEBUG_WIDEN") == "1" {
		fmt.Fprintf(os.Stderr, "deep: widen gate kept=%d complete=%v best=%.1f hook=%v tried=%d\n", len(kept), rep.Complete, bestScore, e.Widen != nil, len(tried))
	}
	// Covers alone cannot veto widening: a generous oracle can mark
	// wrong-doc windows "complete". The budget term uses the REAL loop count
	// (passing 0 made the predicate collapse to !Complete, so the "budget
	// aware" gate never saw the budget).
	//
	// The score term is now the COVER line, not a fixed 6 (2026-07-28,
	// perf-plan §4.9). It used to disagree with the early stop: stop at >= 8,
	// widen below 6, so a best window in [4,8) satisfied neither — and the
	// loop went widening anyway. Measured on a real 727k-rune novel: the block
	// holding the answer scored 5.0, the wrong ones 2.0/1.0/1.0, and the loop
	// spent ~21,000 of its 73,212 tokens widening AFTER already having a
	// covering window. "A window that covers a requirement" is now the
	// single line both gates share, so there is no dead band: below it we
	// widen, at or above it we are done.
	if reason != "utility" &&
		(facts.NeedContinue(rep, loops, e.MaxLoops+e.CorrectBudget+e.WidenBudget) ||
			(bestScore < facts.CoverScoreLine() && !e.budgetAtRisk())) &&
		e.Widen != nil && !e.budgetHit() {
		keptIDs := map[string]bool{}
		for _, sm := range kept {
			keptIDs[sm.Source] = true
		}
		for k := range lawAffinity(keptIDs, sources) {
			affinity[k] = true
		}
		endWn := e.begin()
		extra, werr := e.Widen(ctx, query, widenExclude(), e.WidenBudget, affinity)
		if e.stageTok != nil {
			endWn(&e.stageTok.Widen)
		}
		if werr == nil && len(extra) > 0 {
			widenedDocs = append(widenedDocs, extra...)
			for _, s := range extra {
				if e.budgetHit() || cancelled(ctx) {
					break
				}
				if skipZeroHit && !mcs.HasAnyToken(s.Body, qToks) {
					continue
				}
				loops++
				widened++
				tried[s.ID] = true
				_ts := time.Now()
				endW := e.begin()
				samples, err := newSampler().SampleBody(ctx, query, s.Body)
				sampleNS += int64(time.Since(_ts))
				if e.stageTok != nil {
					endW(&e.stageTok.Score)
				}
				if err != nil {
					continue
				}
				localBest := 0.0
				fileKept := 0
				for _, sm := range samples {
					if sm.Score > localBest {
						localBest = sm.Score
					}
					if sm.Score >= facts.CoverScoreLine() {
						sm.Source = s.ID
						kept = append(kept, sm)
						fileKept++
					}
				}
				if localBest > bestScore {
					bestScore = localBest
					bestSrc = s
				}
			}
			rep = report(kept)
		}
	}

	loops++
	if bestSrc.ID == "" || len(kept) == 0 {
		kept = topKeepsWith(kept, sources)
		rep = report(kept)
		// Refused rides with Skipped here as it does on the other two
		// refusal paths: without the flag this bare refusal looked like a
		// normal answer to every consumer (the ledger gate, the bench's
		// insufficient metric) — the same missing-flag drift the recordUsage
		// gate fixes on its side.
		// Say whether the budget is why we found nothing. With a default
		// budget, "the corpus lacks it" and "we ran out of budget before we
		// found it" produce the SAME user-facing text, and conflating them
		// makes a recall ceiling look like a corpus gap. Live A/B showed the
		// difference is real: one refusal question answered in the uncapped
		// arm and refused in the capped one.
		//
		// Only the budget case gets a new value. "" keeps its established
		// meaning (candidates exhausted / no loop ran — utility_stop_test.go
		// pins it), and an earlier break (utility, sufficient, budget) already
		// named a more specific reason and must not be overwritten.
		if reason == "" && e.BudgetHit {
			reason = "insufficient_after_budget"
		}
		return deepOutcome{
			Answer: fast.Answer{
				Query: query, Mode: ModeDEEP, LLMCalls: loops, Skipped: true, Refused: true,
				Summary: insufficientSummary(query, tried, sources),
			},
			Cover:         rep,
			Loops:         loops,
			Widened:       widened,
			SelfCorrected: selfCorrected,
			Admitted:      admissionIDs(tried),
			Corpus:        citationCorpus(sources, widenedDocs),
			StopReason:    reason,
		}, nil
	}
	// D2: truncate THEN recompute Cover so res.Cover matches what synthesis sees.
	kept = topKeepsWith(kept, sources)
	rep = report(kept)
	cov, conf, template := deepMetrics(query, srcLabel(bestSrc), kept, rep)
	buildAnswer := func(tmpl string) fast.Answer {
		return fast.Answer{
			Query: query, Mode: ModeDEEP, LLMCalls: loops,
			SourceID: bestSrc.ID, Samples: kept, Coverage: cov,
			Confidence: conf, Summary: e.render(ctx, query, kept, tmpl),
			Skipped: conf < fast.SkipBelowLine(),
		}
	}
	best = buildAnswer(template)
	best.Refused = fast.RefusedOf(e.Synth) || fast.RefusedOfSummary(best.Summary, e.Synth)

	// ReAct 观察轮: ONLY on a genuine refusal (flag or template degradation)
	// → admit one more affinity-guided wave and rebuild ONCE. Gating on
	// low-confidence fired on almost every answer and doubled latency
	// (真机: 长时间无输出的元凶); mediocre-but-cited answers are acceptable.
	// D1: same tried-only exclude as the primary widen gate.
	if best.Refused && widened == 0 && e.Widen != nil && len(affinity) > 0 && !e.budgetHit() {
		keptIDs := map[string]bool{}
		for _, sm := range kept {
			keptIDs[sm.Source] = true
		}
		for k := range lawAffinity(keptIDs, sources) {
			affinity[k] = true
		}
		exclude := widenExclude()
		for id := range keptIDs {
			exclude[id] = true
		}
		endWn := e.begin()
		extra, werr := e.Widen(ctx, query, exclude, e.WidenBudget, affinity)
		if e.stageTok != nil {
			endWn(&e.stageTok.Widen)
		}
		if werr == nil && len(extra) > 0 {
			widenedDocs = append(widenedDocs, extra...)
			for _, s := range extra {
				if e.budgetHit() || cancelled(ctx) {
					break
				}
				if skipZeroHit && !mcs.HasAnyToken(s.Body, qToks) {
					continue
				}
				loops++
				widened++
				tried[s.ID] = true
				_ts := time.Now()
				endW := e.begin()
				samples, err := newSampler().SampleBody(ctx, query, s.Body)
				sampleNS += int64(time.Since(_ts))
				if e.stageTok != nil {
					endW(&e.stageTok.Score)
				}
				if err != nil {
					continue
				}
				localBest := 0.0
				for _, sm := range samples {
					if sm.Score > localBest {
						localBest = sm.Score
					}
					if sm.Score >= facts.CoverScoreLine() {
						sm.Source = s.ID
						kept = append(kept, sm)
					}
				}
				if localBest > bestScore {
					bestScore = localBest
					bestSrc = s
				}
			}
			if widened > 0 {
				kept = topKeepsWith(kept, sources)
				rep = report(kept)
				cov, conf, template = deepMetrics(query, srcLabel(bestSrc), kept, rep)
				best = buildAnswer(template)
				best.Refused = fast.RefusedOf(e.Synth) || fast.RefusedOfSummary(best.Summary, e.Synth)
			}
		}
	}
	return deepOutcome{
		Answer:        best,
		Cover:         rep,
		Loops:         loops,
		Widened:       widened,
		SelfCorrected: selfCorrected,
		Admitted:      admissionIDs(tried),
		Corpus:        citationCorpus(sources, widenedDocs),
		StopReason:    reason,
	}, nil
}

// insufficientSummary is the honest "no answer" answer. The bare
// "深度检索仍证据不足" it replaces spent 38.8s and 45K tokens to tell the
// user nothing they could act on (live case: "什么叫帮信罪" — the defining
// article is 刑法第287条之二 and the corpus holds no 刑法, so this refusal
// was the CORRECT outcome; only the message was useless). Deterministic —
// it names what was searched and the nearest documents, so the user learns
// the shape of the gap (missing source vs wrong wording) and where to add.
func insufficientSummary(query string, tried map[string]bool, sources []source.Source) string {
	var titles []string
	for _, s := range sources {
		if tried[s.ID] && len(titles) < 5 {
			titles = append(titles, srcLabel(s))
		}
	}
	b := "深度检索仍证据不足。"
	if len(titles) > 0 {
		b += "已检索的最近文档（" + strings.Join(titles, "、") + "）中，未找到能回答「" +
			query + "」的原文依据。"
	} else {
		b += "语料中未检索到与「" + query + "」相关的原文依据。"
	}
	b += "这可能是因为：① 问法措辞与语料原文差异较大；② 该问题所需的内容本库未收录。补充相关文档后再问。"
	return b
}

// render prefers the production Synthesizer (synthesize_roi) and degrades to
// the deterministic DEEP template on refusal/error.
func (e *Engine) render(ctx context.Context, query string, kept []mcs.Sample, template string) string {
	if e.Synth != nil {
		// Both branches report their wall time — an early return from the
		// streaming path used to leave deep_synth out of the stage split.
		t0 := time.Now()
		endSyn := e.begin()
		if e.SynthDelta != nil || e.ReasoningDelta != nil {
			if rs, ok := e.Synth.(fast.ReasoningSynthesizer); ok {
				if s, err := rs.SynthesizeStreamFull(ctx, query, kept, e.SynthDelta, e.ReasoningDelta); err == nil && strings.TrimSpace(s) != "" {
					if e.stageTok != nil {
						endSyn(&e.stageTok.Synth)
					}
					e.stage("deep_synth", t0, nil)
					return s
				}
			} else if e.SynthDelta != nil {
				if ss, ok := e.Synth.(fast.StreamSynthesizer); ok {
					if s, err := ss.SynthesizeStream(ctx, query, kept, e.SynthDelta); err == nil && strings.TrimSpace(s) != "" {
						if e.stageTok != nil {
							endSyn(&e.stageTok.Synth)
						}
						e.stage("deep_synth", t0, nil)
						return s
					}
				}
			}
		}
		s, err := e.Synth.Synthesize(ctx, query, kept)
		if e.stageTok != nil {
			endSyn(&e.stageTok.Synth)
		}
		e.stage("deep_synth", t0, nil)
		if err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return template
}

// stage reports one DEEP-loop stage's wall time to the observability hook.
// Pure telemetry: nil (the default, and every gate) costs nothing. DEEP's
// sampling time is accumulated across the admission/widen/self-correct
// SampleBody calls by the caller before being reported once.
func (e *Engine) stage(name string, t0 time.Time, detail any) {
	if e.Stages != nil {
		e.Stages(name, time.Since(t0), detail)
	}
}

// Package deep is the DEEP tier: confidence-gated escalation from FAST,
// multi-source evidence refinement, conflict detection between clusters, and
// the citation delivery face ([?] legend for unresolved refs). Low confidence
// must escalate; conflict pairs must be discoverable; citations must resolve
// to source offsets.
//
// Refinement is DETERMINISTIC: widen the keywords, rank-admit a candidate set,
// then refine window by window (sample → oracle score → topKeeps → synthesise).
// The ReAct TOOL loop named in design-plan §4 — keyword_search / read_window /
// expand_clusters / query_kb — is declared there but has never been wired
// (all four identifiers are absent from the tree). Do not read "multi-source
// evidence refinement" as an agent loop; it is not one. When a real case
// appears that deterministic refinement cannot answer, that is the trigger to
// build the tools, and this comment is the place to say so.
//
// 2026-10-04 拆分（纯搬运，无语义改动）：本文件只留引擎核心——Ask 三族、
// 判级 afterBase、阈值与预算判定；DEEP 循环本体在 deeploop.go，证据窗口
// 整理与度量在 evidence.go，簇间冲突在 conflicts.go，引用交付面在
// citations.go。
package deep

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/willove/cumulus/internal/abstain"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/fast"
	"github.com/willove/cumulus/internal/graph"
	"github.com/willove/cumulus/internal/kb"
	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// EscalateBelow is the confidence line under which FAST upgrades to DEEP
// (plan D5: 置信不足 → DEEP / ReAct).
//
// The value lives in package fast because fast cannot import deep (deep
// already imports fast for the shared skip line), so this direction is the
// only one that avoids an import cycle. It used to be a second literal
// 0.35 — which meant that editing one and not the other silently
// desynchronised the FAST skip floor from the DEEP escalation line
// whenever CLUS_ESCALATE_BELOW was unset. One constant, one default.
//
// The runtime value both sides read is CLUS_ESCALATE_BELOW; see
// escalateBelowLine below and fast.SkipBelowLine, which apply identical
// bounds and fall back here.
const EscalateBelow = fast.SkipBelow

// escalateBelowLine is the runtime escalation line: the historical constant
// unless CLUS_ESCALATE_BELOW overrides it (calibration knob, 2026-09-29).
//
// Why the knob exists: 95 archived FAST rows measure the answer-confidence
// distribution at 0.45–0.91 — nothing ever lands under the 0.35 line, so on
// single-fact queries the confidence arm of the escalate condition is dead
// and the whole mid-band leaks through as served FAST answers. The measured
// correct-rate by band: <0.65 → 0/9, 0.65–0.75 → 9/31, 0.75–0.85 → 20/41,
// ≥0.85 → 13/14. DEEP performs ~0.80 on the same corpora, so escalating the
// sub-0.85 band trades tokens for correctness. The default stays 0.35
// (byte-identical) until the paired A/B earns a new one.
func escalateBelowLine() float64 {
	v := strings.TrimSpace(os.Getenv("CLUS_ESCALATE_BELOW"))
	if v == "" {
		return EscalateBelow
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 0.95 {
		return EscalateBelow
	}
	return f
}

// MaxLoops bounds the DEEP tool loop (Sirchmunk max_loops analogue).
// 4, down from 6: measured answerable queries converge on the utility stop
// long before the cap (a cross-law criminal query finished in 2 loops), so
// the cap only ever bound the grind — the unreachable queries that spent
// 130s / 76K tokens re-scoring files earlier rounds had cleared. The cut
// halves that class without touching the converging ones. Overridable per
// deployment via CLUS_DEEP_LOOPS.
const MaxLoops = 4

// WidenBudget is the widening pass's own file allowance, independent of
// MaxLoops (the initial admission must not starve exploration). Exported
// because it is part of the observable DEEP cost model — the eval scoreboard
// binds it into the config fingerprint so two runs with different budgets are
// not reported as the same configuration. 3, down from 4: same measurement
// as MaxLoops (answerable queries converge on the utility stop; the cap only
// bound the unreachable-query grind). CLUS_DEEP_WIDEN overrides.
const WidenBudget = 3

// CorrectBudget is the self-correction file allowance (D4): admission
// usually spends MaxLoops on the first wave; missing-fact re-sampling must
// not share that clock or the weakest-requirement pass never runs. Exported
// for the same reason as WidenBudget. 2, down from 3 — same measurement as
// MaxLoops; CLUS_DEEP_CORRECT overrides.
const CorrectBudget = 2

// maxKeepWindows is the synthesis budget: only the top-scored windows are
// handed to the synthesizer / returned in the answer. 8: measured DEEP
// answers carry 2-4 scored windows in practice, so the cap binds only on
// multi-file runs — where cutting it trades evidence completeness for a
// fraction of a second of prefill on a now-thinking-disabled synthesis call.
// CLUS_DEEP_KEEP_WINDOWS overrides per deployment.
const maxKeepWindows = 8

// Mode labels.
const (
	ModeFAST = "FAST"
	ModeDEEP = "DEEP"
)

// Result is a DEEP (or escalated) answer with citations.
type Result struct {
	Answer     fast.Answer `json:"answer"`
	Escalated  bool        `json:"escalated"`
	Mode       string      `json:"mode"`
	Loops      int         `json:"loops"`
	Citations  CitationSet `json:"citations"`
	Conflicts  []Conflict  `json:"conflicts,omitempty"`
	ClusterID  string      `json:"cluster_id,omitempty"`
	ClusterVer int         `json:"cluster_version,omitempty"`
	Reused     bool        `json:"reused"`
	Sampled    int         `json:"sampled"`
	Persisted  bool        `json:"persisted"`
	Merged     bool        `json:"merged"`
	// The persist judge's verdict, copied from the kb Result: the cluster
	// stamp is the durable record; these make it visible to the response
	// surface and eval (a fail-open error used to vanish here).
	Judged    bool                 `json:"judged"`
	JudgeOK   bool                 `json:"judge_ok"`
	JudgeWhy  string               `json:"judge_why,omitempty"`
	Neighbors []graph.ExpandResult `json:"neighbors,omitempty"`
	// Cover is the per-fact multi-hop coverage report (oracle annotations
	// vector when the scorer annotates covers).
	Cover facts.Report `json:"cover"`
	// SelfCorrected marks a weakest-requirement re-sample pass.
	SelfCorrected bool `json:"self_corrected"`
	// Widened counts files admitted mid-search by the widening pass (Sirchmunk
	// ReAct 对齐): exploration may grow the candidate set, bounded.
	Widened int `json:"widened,omitempty"`
	// Session echoes the chat session id when the caller passed one.
	Session string `json:"session,omitempty"`
	// Model names the chat model that served this query ("" on the offline
	// stub). 消费追溯维度：监控、会话卡与持久台账都按它记账。
	Model string `json:"model,omitempty"`
	// Budget accounting: LatencyMS is always filled; Tokens carries the
	// upstream-reported total (0 on the offline stub path — the CLI fills it
	// from the chat client after Ask returns). BudgetHit marks an independent
	// token-budget stop (judge tokens never enter TokenBudget).
	Tokens    int64 `json:"tokens,omitempty"`
	LatencyMS int64 `json:"latency_ms,omitempty"`
	// LatencyUS is microsecond precision. LatencyMS truncates, so a sub-
	// millisecond offline FAST query reads as 0 — useless for a warm/cold
	// comparison. This is the field ops tooling should read.
	LatencyUS int64 `json:"latency_us,omitempty"`
	BudgetHit bool  `json:"budget_hit,omitempty"`
	// Admitted lists source IDs this Ask actually scored (admission + widen
	// + self-correct). ir-rag 1.5: not-retrieved gold outside this set is a
	// Remark-1 ceiling, not a synthesis failure.
	Admitted []string `json:"admitted,omitempty"`
	// AbstainP is the zero-LLM abstention head's fail probability when wired
	// (ir-rag 3.1). Recorded only; thresholds are operator-owned.
	AbstainP float64 `json:"abstain_p,omitempty"`
	// AbstainAction is "" | "deep" | "refuse" — what the head recommended.
	AbstainAction string `json:"abstain_action,omitempty"`
	// StopReason reports why the DEEP admission loop stopped: "sufficient"
	// (coverage complete at/over the score bar), "utility" (per-round
	// pessimistic exit — p_fail high and not improving across rounds, opt-in
	// via CLUS_EARLY_ABSTAIN; skips self-correction and widening too),
	// "insufficient_after_budget" — the refusal happened because the budget
	// ran out, not because the corpus lacked the answer. Same user-facing
	// text as a plain refusal, so it is named explicitly; see the refusal path.
	// "budget" (MaxLoops or TokenBudget cap), "" (candidates exhausted, or no
	// loop ran — FAST-tier answers and pre-DEEP early refuses, which
	// AbstainEarly/AbstainAction already describe).
	StopReason string `json:"stop_reason,omitempty"`
	// AbstainEarly marks a pre-DEEP refusal (FAST had zero usable evidence
	// and p_fail cleared EarlyAbove): DEEP was skipped to save budget.
	AbstainEarly bool `json:"abstain_early,omitempty"`
	// StagesTokens attributes search tokens per paying stage when a Meter is
	// wired: rewrite (history fold), fast (the FAST tier's score+analyze+
	// synth, one bucket), decompose, rank (admission ordering), score (DEEP
	// window scoring), widen, synth. Judge spend never enters these — the
	// budget's own exclusion rule — and the buckets are attribution, not
	// billing: they exist so a fat stage is visible before it is optimized.
	StagesTokens *StageTokens `json:"stages_tokens,omitempty"`
}

// StageTokens is the per-stage token attribution carried on Result.
type StageTokens struct {
	Rewrite int64 `json:"rewrite,omitempty"`
	Fast    int64 `json:"fast,omitempty"`
	Rank    int64 `json:"rank,omitempty"`
	Score   int64 `json:"score,omitempty"`
	Widen   int64 `json:"widen,omitempty"`
	Synth   int64 `json:"synth,omitempty"`
}

// Engine runs FAST and escalates into DEEP when confidence is thin.
type Engine struct {
	KB        *kb.Engine
	Conflicts ConflictStore
	// Scorer rates evidence windows in the DEEP loop. nil = offline
	// KeywordScorer (gate carrier); production wires llm.Scorer (D6).
	Scorer mcs.Scorer
	// Synth renders DEEP summaries (synthesize_roi). nil = deterministic
	// template; production wires llm.Synthesizer.
	Synth fast.Synthesizer
	// SampleContext is the session fallback for the sampler (see
	// fast.Engine.SampleContext): when the raw query keeps no window in an
	// admitted document, retry once with the thread's recent questions
	// folded in. Binary — fires only when the primary pass found nothing.
	SampleContext string
	// DocWeights is the per-request usage-weight snapshot (ledger ∪ session
	// stack). The loop reads it for the session-bridge early exit: an
	// anchored document (≥0.6) yielding bridged evidence ends the crawl —
	// the thread's document answered.
	DocWeights map[string]float64
	// History + HistoryRewriter fold follow-up context into a standalone
	// query before retrieval (history_rewrite contract; nil = raw query).
	History         []string
	HistoryRewriter HistoryRewriter
	Sources         []source.Source
	EscalateBelow   float64
	// Verbose, when set, receives per-step diagnostic lines (serve -verbose /
	// CLUS_VERBOSE). nil → silent.
	Verbose func(format string, a ...any)
	// OnFile fires after each file's windows are sampled (admission order) —
	// the SSE face forwards it so the UI shows live progress.
	OnFile func(key string, best float64, windows int)
	// RankAdmission orders the sources before the DEEP loop explores them.
	// affinity carries the law prefixes of already-answered sources (same-law
	// statutes answer in clusters — 同法亲缘准入). nil → caller order.
	RankAdmission func(ctx context.Context, query string, sources []source.Source, affinity map[string]bool) ([]source.Source, error)
	// Widen re-admits candidate files mid-search under the same contract
	// (Sirchmunk ReAct 对齐；nil 关闭扩征，离线门可用).
	Widen func(ctx context.Context, query string, exclude map[string]bool, m int, affinity map[string]bool) ([]source.Source, error)
	// fxMemoQuery / fxMemo back Engine.decompose.
	//
	// The memo is keyed by the EFFECTIVE query, not assumed per-request: an
	// Engine is reused across queries in real call paths (and lazy_test.go
	// relies on it), so an unkeyed memo fed the first query's requirements to
	// the second — caught by TestAskLazyLoadsCorpusWhenEscalating. Keying on
	// the query makes reuse safe regardless of the Engine's lifetime.
	fxMemoQuery string
	fxMemo      []facts.Fact
	fxMemoSet   bool
	// TokenBudget is an independent stop (LENS Def 3 / Remark 2): when > 0
	// and TokensUsed is wired, the DEEP loop checks remaining budget before
	// scoring each admitted file. Judge tokens never enter this budget.
	TokenBudget int64
	TokensUsed  func() int64
	// Stages, when set, receives DEEP-loop stage wall times
	// ("deep_sample" accumulated across admission/widen/self-correct
	// SampleBody calls, "deep_synth" per synthesis) plus a stage detail
	// payload (nil when none): deep_sample carries the admitted window count
	// and the still-uncovered facts — the signal that drove the loop.
	// Pure observability — nil (the default, and every gate) changes
	// nothing. See fast.Engine.Stages for why the split exists.
	Stages func(stage string, d time.Duration, detail any)
	// Meter, when set, snapshots the process's cumulative SEARCH-token
	// counter (chat.TotalTokens in production; judge spend excluded by the
	// stack's own base snapshot). StageTokens on Result attributes each
	// paying stage's delta; nil = no attribution (offline/hermetic runs).
	Meter func() int64
	// stageTok is the per-Ask scratch the metered call sites accumulate into;
	// reset at every Ask/AskLazy entry so a reused engine never mixes queries.
	stageTok *StageTokens
	// SynthDelta streams the synthesis answer delta by delta (see
	// fast.Engine.SynthDelta); nil = non-streaming synthesis.
	SynthDelta func(chunk string)
	// ReasoningDelta streams the synthesizer's chain-of-thought (see
	// fast.Engine.ReasoningDelta); nil = the reasoning stays dropped.
	ReasoningDelta func(chunk string)
	// Loop budgets (defaults MaxLoops/WidenBudget/CorrectBudget; serve
	// overrides from CLUS_DEEP_LOOPS/CLUS_DEEP_WIDEN/CLUS_DEEP_CORRECT).
	// Measured DEEP queries ran ~14 rounds (6 admission + 4 widen + 3
	// correct), and rounds past the useful ones burn scorer calls on files
	// earlier rounds already cleared. Fields, not constants, so the cut is
	// per-deployment configurable and the e2e gates still exercise the
	// constants via New().
	MaxLoops      int
	WidenBudget   int
	CorrectBudget int
	// BudgetHit reports the last Ask stopped because TokenBudget was spent.
	BudgetHit bool
	// Abstain is an optional zero-LLM failure head (ir-rag 3.1 / RCS idea).
	// nil = off (no behavior change). When set, features from the current
	// search state produce p_fail; "deep" forces escalation, "refuse" marks
	// the answer refused (and must not persist knowledge).
	Abstain *abstain.Head
	// QuerySim is an optional two-call complementary-query generator
	// (Self-Index A.2.1 / ir-rag 2.4). nil = self-correction uses only
	// MissingQueries. Non-nil: complements join the resample pool after
	// Jaccard filtering against origin + tried fact queries.
	QuerySim QuerySimulator
}

// QuerySimulator produces complementary phrasings without seeing the raw
// original in the second call (A.2.1 isolation). Implemented by
// llm.QuerySimulator in production; offline stubs in tests.
type QuerySimulator interface {
	Complement(ctx context.Context, origin string, tried []string) ([]string, error)
}

// admissionIDs returns the sorted-stable list of source IDs the loop tried.
func admissionIDs(tried map[string]bool) []string {
	if len(tried) == 0 {
		return nil
	}
	out := make([]string, 0, len(tried))
	for id := range tried {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// HistoryRewriter rewrites a follow-up query against history.
type HistoryRewriter interface {
	Rewrite(ctx context.Context, history []string, query string) (string, error)
}

// effectiveQuery returns the standalone query (history-folded when a
// rewriter is wired; failures degrade to the raw query).
func (e *Engine) effectiveQuery(ctx context.Context, query string) string {
	if e.HistoryRewriter == nil || len(e.History) == 0 {
		return query
	}
	if q, err := e.HistoryRewriter.Rewrite(ctx, e.History, query); err == nil && strings.TrimSpace(q) != "" {
		return q
	}
	return query
}

func New(k *kb.Engine, conflicts ConflictStore) *Engine {
	return &Engine{
		KB: k, Conflicts: conflicts, EscalateBelow: escalateBelowLine(),
		MaxLoops: MaxLoops, WidenBudget: WidenBudget, CorrectBudget: CorrectBudget,
		// The per-query cap is ON by default (see DefaultTokenBudget for the
		// derivation). A caller that wants the old uncapped loop sets
		// TokenBudget = 0 explicitly; the env override still wins on top.
		TokenBudget: DefaultTokenBudget,
	}
}

// envFloat reads a positive float env override with a default.
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

// DocWorkers is how many candidate documents the DEEP pass may sample at once.
//
// Default 1 = the historical serial loop, output byte-for-byte. Opt-in because
// a cold query scores ~10 candidate documents back to back, and each one ends
// in a blocking evaluate_sample round-trip; the serial chain was the largest
// single term in the observed 40s+ cold latency (perf-plan §2 P0-3).
//
// Concurrency does NOT make the DEEP rounds themselves concurrent: round N+1
// consumes round N's TopSeeds, so ~2 waves is the floor. This cap only widens
// the candidate set within one round.
func DocWorkers() int {
	if v := os.Getenv("CLUS_DEEP_DOC_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// bridgeFloorScore mirrors fast.bridgeFloorScore (CLUS_BRIDGE_FLOOR, default
// 4): bridged evidence weaker than this does not end the loop.
var bridgeFloorScore = envFloat("CLUS_BRIDGE_FLOOR", 4)

// nonePass reports whether no sample cleared the admission floor.
func nonePass(samples []mcs.Sample) bool {
	for _, sm := range samples {
		if sm.Score >= 4 {
			return false
		}
	}
	return true
}

// rankAdmission orders sources before exploration (nil → untouched order).
// affinity = law prefixes of sources already answered (FAST hit + kept), so
// the ranker can put same-law articles first (同法亲缘).
func (e *Engine) rankAdmission(ctx context.Context, query string, sources []source.Source, affinity map[string]bool) []source.Source {
	if e.RankAdmission == nil {
		return sources
	}
	end := e.begin()
	out, err := e.RankAdmission(ctx, query, sources, affinity)
	if e.stageTok != nil {
		end(&e.stageTok.Rank)
	}
	if err == nil && len(out) > 0 {
		return out
	}
	return sources
}

// lawAffinity collects the document-family prefixes (business_key before
// '-') of the sources the pipeline already answered from.
func lawAffinity(ids map[string]bool, sources []source.Source) map[string]bool {
	aff := map[string]bool{}
	if len(ids) == 0 {
		return aff
	}
	for _, s := range sources {
		if !ids[s.ID] {
			continue
		}
		if i := strings.Index(s.BusinessKey, "-"); i > 0 {
			aff[s.BusinessKey[:i]] = true
		}
	}
	return aff
}

// cancelled reports whether the caller went away. The DEEP loops consult it
// before every paying stage: serve's SSE path cancels through r.Context(), and
// without this a disconnected client kept burning scorer/synthesizer calls
// until the loop ran out.
func cancelled(ctx context.Context) bool {
	return ctx.Err() != nil
}

func (e *Engine) scorer() mcs.Scorer {
	if e.Scorer != nil {
		return e.Scorer
	}
	return mcs.KeywordScorer{}
}

// decompose resolves the query's atomic evidence requirements exactly once per
// Engine and memoizes the result, because thresholdFor (in askEffective) and the
// oracle hints + coverage report (in runDeep) must agree on the same K — two
// independent decompositions would let the stop line and the coverage report
// decompose is the deterministic requirement baseline with a per-query
// memo. (The LLM decomposer swap was retired 2026-09-30: ab-decompose
// measured negative — ev_rec 1:0 against, ~6% tokens — and R5 says archived
// verdicts leave the codebase.)
func (e *Engine) decompose(_ context.Context, query string) []facts.Fact {
	if e.fxMemoSet && e.fxMemoQuery == query {
		return e.fxMemo
	}
	fx := facts.Build(query)
	e.fxMemoQuery, e.fxMemo, e.fxMemoSet = query, fx, true
	return fx
}

// gammaStep raises the escalation line per extra atomic fact (B10): a
// multi-fact comparison must not stop on a single-fact-quality answer.
const gammaStep = 0.05

// thresholdFor modulates the escalation line by intent shape (B10 γ(I)):
// single-fact lookups stop at the base line, multi-fact comparisons demand
// proportionally more. The cap is RELATIVE to the base (base + 3 γ-steps,
// bounded by 0.95 so DEEP stays reachable): the old absolute 0.6 cap never
// bound at the 0.35 default (0.35+0.15 < 0.6), and an absolute cap under a
// raised base would silently LOWER the line below it.
func (e *Engine) thresholdFor(fx []facts.Fact) float64 {
	thr := e.EscalateBelow
	if thr <= 0 {
		thr = EscalateBelow
	}
	cap := thr + 3*gammaStep
	if cap > 0.95 {
		cap = 0.95
	}
	extra := len(fx) - 1
	if extra > 3 {
		extra = 3
	}
	thr += gammaStep * float64(extra)
	if thr > cap {
		thr = cap
	}
	return thr
}

// Ask runs the confidence-gated path: insufficient confidence escalates.
func (e *Engine) Ask(ctx context.Context, query string, sources []source.Source) (res Result, err error) {
	started := time.Now()
	e.stageTok = &StageTokens{}
	defer func() {
		d := time.Since(started)
		res.LatencyMS = d.Milliseconds()
		res.LatencyUS = d.Microseconds()
	}()
	end := e.begin()
	q := e.effectiveQuery(ctx, query)
	if e.stageTok != nil {
		end(&e.stageTok.Rewrite)
	}
	return e.askEffective(ctx, q, sources)
}

// askEffective is the Ask body on a query already folded out of session
// history. AskLazy rewrites once up front — the narrow-reuse attempt needs
// the rewritten query too — so its fall-through to the full path must not
// rewrite again: every rewrite is a real LLM call, and rewriting a rewrite
// pays twice for a twice-distorted question.
func (e *Engine) askEffective(ctx context.Context, query string, sources []source.Source) (res Result, err error) {
	started := time.Now()
	defer func() {
		d := time.Since(started)
		res.LatencyMS = d.Milliseconds()
		res.LatencyUS = d.Microseconds()
	}()
	if e.Sources == nil {
		e.Sources = sources
	}
	fx := e.decompose(ctx, query)
	thr := e.thresholdFor(fx) // B10 γ(I): multi-fact intents stop stricter
	// Deferred FAST synthesis (CLUS_FAST_DEFER_SYNTH, default ON — opt out
	// with =0; flipped from opt-in in 9247333): the FAST
	// tier runs inside KB.Ask below, and a thin-confidence answer there is
	// certain to escalate — its render is a duplicate the DEEP tier pays
	// again (stages_tokens, 2026-09-28: fast bucket 14.6k of a 29.8k query,
	// ~half of it that render). The line handed down is the SAME threshold
	// this engine escalates on, so "deferred" and "escalated" cannot drift
	// apart; the non-escalating path owes BackfillSynth (see afterBase).
	if e.KB != nil && e.KB.Fast != nil && os.Getenv("CLUS_FAST_DEFER_SYNTH") != "0" {
		e.KB.Fast.DeferBelow = thr
		// The cover arm must read the SAME ruler the escalation below reads:
		// fx is handed down so fast.Search evaluates facts.ReportFor(fx, kept)
		// against the very samples afterBase re-evaluates — a rewritten
		// sampleQuery can lexically cover the sentence while a fact stays
		// open (live: 闯红灯 142s run paid a 64.8s render the escalation then
		// discarded). Arming stays K>1 as measured: at K=1 an armed cover arm
		// defers the fresh answers the L2 learning path persists on, and the
		// offline query_seq gates starve (e2e B/P4/sixmod, 2026-10-04) — the
		// K=1 rewrite-and-uncovered corner keeps its render; that trade is
		// the standing measured decision, unchanged here.
		e.KB.Fast.DeferThinCover = len(fx) > 1
		e.KB.Fast.DeferFacts = fx
	}

	// FILENAME_ONLY tier (D5 附档): name/extension lookups answer before any
	// retrieval, with 0 LLM calls.
	if ans, ok := fast.MatchFilename(query, sources); ok {
		return Result{Answer: ans, Mode: fast.ModeFilenameOnly}, nil
	}

	// Try L2 reuse first (0-sample path).
	end := e.begin()
	base, err := e.KB.Ask(ctx, query, sources)
	if e.stageTok != nil {
		end(&e.stageTok.Fast)
	}
	if err != nil {
		return Result{}, err
	}
	return e.afterBase(ctx, started, query, base, sources, thr, fx, nil)
}

// SourceLoader lazily materializes the full candidate corpus (a warm
// reuse must not pay the full-corpus read).
type SourceLoader func(ctx context.Context) ([]source.Source, error)

// AskLazy is Ask with a lazy full-corpus load. The warm-prior reuse attempt
// runs FIRST against only the sources the candidate cluster anchors on; a
// hit returns without ever calling load. A miss (or any escalation) loads
// the corpus and continues on the normal path.
func (e *Engine) AskLazy(ctx context.Context, query string, load SourceLoader) (res Result, err error) {
	started := time.Now()
	e.stageTok = &StageTokens{}
	defer func() {
		d := time.Since(started)
		res.LatencyMS = d.Milliseconds()
		res.LatencyUS = d.Microseconds()
	}()
	if load == nil {
		return Result{}, fmt.Errorf("deep: AskLazy requires a loader")
	}
	end := e.begin()
	query = e.effectiveQuery(ctx, query)
	if e.stageTok != nil {
		end(&e.stageTok.Rewrite)
	}
	fx := e.decompose(ctx, query)
	thr := e.thresholdFor(fx)

	if e.KB != nil {
		if base, narrow, ok, err := e.KB.TryReuseNarrow(ctx, query); err != nil {
			return Result{}, err
		} else if ok {
			e.Sources = nil // the narrow hit never materialized the corpus
			return e.afterBase(ctx, started, query, base, narrow, thr, fx, load)
		}
	}
	all, err := load(ctx)
	if err != nil {
		return Result{}, err
	}
	if e.Sources == nil {
		e.Sources = all
	}
	// query was folded once at the top of AskLazy; askEffective takes it as-is.
	return e.askEffective(ctx, query, all)
}

// afterBase is the shared post-reuse path: citations, conflicts, cover, the
// abstain gates and the DEEP escalation. `citeCorpus` is the source set
// citation refs resolve against (the full corpus, or the narrow anchor set
// on a warm hit); `load` materializes the full corpus when DEEP actually
// escalates (nil = caller already provided it).
func (e *Engine) afterBase(ctx context.Context, started time.Time, query string, base kb.Result, citeCorpus []source.Source, thr float64, fx []facts.Fact, load SourceLoader) (res Result, err error) {
	defer func() {
		d := time.Since(started)
		res.LatencyMS = d.Milliseconds()
		res.LatencyUS = d.Microseconds()
		if e.Meter != nil && e.stageTok != nil {
			res.StagesTokens = e.stageTok
		}
	}()
	e.BudgetHit = false
	res = Result{
		Answer:     base.Answer,
		ClusterID:  base.ClusterID,
		ClusterVer: base.ClusterVer,
		Reused:     base.Reused,
		Sampled:    base.Sampled,
		Persisted:  base.Persisted,
		Merged:     base.Merged,
		Neighbors:  base.Neighbors,
		Mode:       ModeFAST,
		// The persist judge's verdict, same as the DEEP path copies it —
		// the FAST path used to drop it, so a recorded verdict was
		// invisible on every FAST answer.
		Judged:   base.Judged,
		JudgeOK:  base.JudgeOK,
		JudgeWhy: base.JudgeWhy,
	}
	res.Citations = BuildCitations(query, base.Answer, citeCorpus)
	res.Conflicts = e.conflictsFor(ctx, base.ClusterID)
	res.Cover = facts.ReportFor(fx, base.Answer.Samples)

	// Tier exits: non-search intents never escalate.
	switch base.Answer.Mode {
	case fast.ModeChat, fast.ModeDocSummary:
		res.Mode = base.Answer.Mode
		return res, nil
	}

	// Single-fact paraphrases may miss lexical coverage despite a validated prior.
	// A conflicts edge is an escalation condition in its own right (D5: "或命中
	// conflicts 边"): a contested prior must be re-searched, not served as a
	// normal FAST answer.
	need := base.Answer.Skipped || base.Answer.Refused || base.Answer.Confidence < thr || len(base.Answer.Samples) == 0 ||
		len(res.Conflicts) > 0 ||
		(!res.Cover.Complete && (!base.Reused || len(fx) > 1))
	// Session-bridged FAST answers stand: the evidence came from the thread's
	// own document via the sampler fallback, and no amount of DEEP crawling
	// finds wording the statute does not contain.
	if base.Answer.Bridged {
		need = false
	}
	// Zero-LLM abstention head (ir-rag 3.1): structural features only; nil
	// head keeps the historical escalate/refuse gates unchanged.
	if e.Abstain != nil {
		top := 0.0
		for _, sm := range base.Answer.Samples {
			if sm.Score > top {
				top = sm.Score
			}
		}
		f := abstain.FromAnswer(query, len(citeCorpus), len(base.Answer.Samples), top,
			len(res.Cover.Missing), base.Answer.Confidence,
			base.Answer.Skipped, base.Answer.Refused)
		p, act := e.Abstain.Decide(f)
		res.AbstainP, res.AbstainAction = p, act
		if act == "deep" {
			need = true
		}
		if act == "refuse" && !need {
			// Head already sure this is a fail — do not trust a thin FAST hit.
			need = true
		}
		// 早弃权（ir-rag 3.1 / RCS 原意「少烧钱」）：FAST 已**完全没有**可用
		// 证据（无样本且被跳过）且 p_fail 达到 EarlyAbove 时，在 DEEP 之前
		// 就拒——DEEP 的 14 轮 + 扩征在这类题上是纯燃烧（真机：≈96s /
		// ≈19k tokens 换一个必然的拒答）。反之（哪怕有一个低分样本）仍然
		// 升级：DEEP 的补采样/扩征确实救回过一些题，不能一刀切。
		if e.Abstain.EarlyAbove > 0 && len(base.Answer.Samples) == 0 && base.Answer.Skipped {
			if e.Abstain.PFail(f) >= e.Abstain.EarlyAbove {
				res.Answer.Refused = true
				res.Answer.Skipped = true
				if strings.TrimSpace(res.Answer.Summary) == "" {
					// Early abstain is the cheapest honest "no", so its
					// message has to carry the same diagnosis the DEEP
					// bare refusal now carries (live case: "什么叫帮信罪"
					// — 刑法 not in the corpus): what is missing and the
					// likely shape of the gap, so the user can act on it.
					res.Answer.Summary = "证据不足，暂不作答。语料中未找到与「" + query +
						"」直接相关的原文依据；所需内容可能未被本库收录，补充相关文档后再问。"
				}
				res.AbstainEarly = true
				res.Citations.Legend = legend(res.Citations, false)
				return res, nil
			}
		}
	}
	if !need {
		// The standing-FAST path owes a deferred answer its synthesis before
		// it is served (deferred ⇒ conf < thr ⇒ need=true, so this is
		// reachable only if a future edit moves a line — the backfill keeps
		// that edit from serving an empty summary). res.Answer is a COPY of
		// base.Answer made above, so the debt is paid into the copy.
		if res.Answer.SynthDeferred && e.KB != nil && e.KB.Fast != nil {
			for _, s := range citeCorpus {
				if s.ID == res.Answer.SourceID {
					endB := e.begin()
					e.KB.Fast.BackfillSynth(ctx, &res.Answer, s)
					if e.stageTok != nil {
						endB(&e.stageTok.Fast)
					}
					break
				}
			}
		}
		res.Citations.Legend = legend(res.Citations, false)
		if base.Answer.SourceID != "" {
			res.Admitted = []string{base.Answer.SourceID}
		}
		return res, nil
	}

	// a narrow reuse hit that still needs DEEP must now pay for the
	// corpus — load it exactly once, here (nil corpus = lazy path).
	// Escalation has to pay for the corpus. The narrow anchor set a warm reuse
	// hit leaves in citeCorpus is not a DEEP corpus: sampling, self-correction
	// and widening would all be confined to the anchors. So load whenever a
	// loader exists — a nil corpus only means nobody has loaded it yet.
	deepCorpus := citeCorpus
	if load != nil {
		if deepCorpus, err = load(ctx); err != nil {
			return Result{}, err
		}
		if e.Sources == nil {
			e.Sources = deepCorpus
		}
	}

	res.Escalated = true
	res.Mode = ModeDEEP
	// 文档族亲缘: the FAST answer's source votes for its family — answers
	// cluster within a family (11/13 failures were same-family near-misses).
	affinity := lawAffinity(map[string]bool{base.Answer.SourceID: base.Answer.SourceID != ""}, deepCorpus)
	out, err := e.runDeep(ctx, query, deepCorpus, affinity)
	if err != nil {
		return Result{}, err
	}
	res.Loops = out.Loops
	res.StopReason = out.StopReason
	res.Answer = out.Answer
	res.Cover = out.Cover
	res.SelfCorrected = out.SelfCorrected
	res.Widened = out.Widened
	res.Admitted = out.Admitted
	res.Citations = BuildCitations(query, out.Answer, out.Corpus)
	// Mark unresolved refs when DEEP still cannot pin a quote.
	unresolved := false
	for _, r := range res.Citations.Refs {
		if !r.Resolved {
			unresolved = true
			break
		}
	}
	res.Citations.Legend = legend(res.Citations, unresolved)

	// Abstention after a full search: refuse means "do not treat this as
	// knowledge" — skip Persist so G-pollute stays clean.
	if e.Abstain != nil {
		top := 0.0
		for _, sm := range out.Answer.Samples {
			if sm.Score > top {
				top = sm.Score
			}
		}
		f := abstain.FromAnswer(query, len(deepCorpus), len(out.Answer.Samples), top,
			len(out.Cover.Missing), out.Answer.Confidence,
			out.Answer.Skipped, out.Answer.Refused)
		p, act := e.Abstain.Decide(f)
		res.AbstainP, res.AbstainAction = p, act
		if act == "refuse" {
			res.Answer.Refused = true
			if strings.TrimSpace(res.Answer.Summary) == "" || res.Answer.Skipped {
				res.Answer.Summary = "证据不足，暂不作答"
				res.Answer.Skipped = true
			}
			res.BudgetHit = e.BudgetHit
			return res, nil
		}
	}

	sub, err := e.KB.Persist(ctx, out.Answer, deepCorpus)
	if err != nil {
		return Result{}, err
	}
	res.ClusterID = sub.ClusterID
	res.ClusterVer = sub.ClusterVer
	res.Persisted = sub.Persisted
	res.Merged = sub.Merged
	// The persist judge's verdict rides along: the record channel is the
	// cluster stamp, but the response/eval must also SEE it (a fail-open
	// error used to vanish here entirely).
	res.Judged, res.JudgeOK, res.JudgeWhy = sub.Judged, sub.JudgeOK, sub.JudgeWhy
	res.Reused = false
	res.Sampled = len(out.Answer.Samples)
	res.BudgetHit = e.BudgetHit
	return res, nil
}

// budgetHit reports whether the independent search-token budget is spent, and
// records it. Every stage that spends tokens must consult this — initial
// admission, self-correction, widening and the query simulator all call the
// model, so a gate that guards only the first loop still lets the budget be
// blown afterwards.
// begin returns the stage-close function for a metered span: call it after the
// paying call returns and hand it the bucket the spend belongs in. With no
// Meter wired the close is a no-op, so every gate keeps its byte-for-byte
// offline behaviour.
func (e *Engine) begin() func(*int64) {
	if e.Meter == nil {
		return func(*int64) {}
	}
	before := e.Meter()
	return func(slot *int64) { *slot += e.Meter() - before }
}

// budgetAtRisk reports whether this query is consuming its token budget.
//
// It is the only thing that arms the second early-stop arm, so with TokenBudget
// unset it is always false and the loop behaves exactly as before — the budget
// stays opt-in, but it is now something the stop condition READS rather than a
// number sitting in a config file that nothing consults.
func (e *Engine) budgetAtRisk() bool {
	if e.TokenBudget <= 0 || e.TokensUsed == nil {
		return false
	}
	return float64(e.TokensUsed()) >= budgetRiskFraction*float64(e.TokenBudget)
}

func (e *Engine) budgetHit() bool {
	if e.TokenBudget <= 0 || e.TokensUsed == nil {
		return false
	}
	if e.TokensUsed() < e.TokenBudget {
		return false
	}
	e.BudgetHit = true
	return true
}

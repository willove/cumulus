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
	"github.com/willove/cumulus/internal/cluster"
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

// Ref is one citation into a source (delivery face).
type Ref struct {
	Index    int    `json:"index"`
	SourceID string `json:"source_id"`
	Title    string `json:"title"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Quote    string `json:"quote"`
	Span     string `json:"span"` // structure label, e.g. p2 / §连接池
	Resolved bool   `json:"resolved"`
}

// CitationSet is the answer's evidence delivery: numbered refs + legend.
type CitationSet struct {
	Refs   []Ref  `json:"refs"`
	Legend string `json:"legend"`
}

// Conflict is a contested link between two clusters (clus_conflicts).
type Conflict struct {
	ID     string    `json:"_id"`
	A      string    `json:"a"`
	B      string    `json:"b"`
	Group  string    `json:"group"`
	Reason string    `json:"reason"`
	Saved  time.Time `json:"saved"` // write timestamp (diagnostic read faces surface it)
}

// ConflictStore persists conflict edges.
type ConflictStore interface {
	Save(ctx context.Context, c Conflict) error
	Between(ctx context.Context, a, b string) ([]Conflict, error)
	All(ctx context.Context) ([]Conflict, error)
}

// MemoryConflict is an in-memory ConflictStore.
type MemoryConflict struct{ m map[string]Conflict }

func NewMemoryConflict() *MemoryConflict { return &MemoryConflict{m: map[string]Conflict{}} }

func (s *MemoryConflict) Save(_ context.Context, c Conflict) error {
	if c.ID == "" {
		c.ID = "x:" + c.A + "-" + c.B + "-" + c.Group
	}
	s.m[c.ID] = c
	return nil
}

func (s *MemoryConflict) Between(_ context.Context, a, b string) ([]Conflict, error) {
	var out []Conflict
	for _, c := range s.m {
		if (c.A == a && c.B == b) || (c.A == b && c.B == a) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryConflict) All(_ context.Context) ([]Conflict, error) {
	out := make([]Conflict, 0, len(s.m))
	for _, c := range s.m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

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
	// SampleBody calls, "deep_synth" per synthesis). Pure observability —
	// nil (the default, and every gate) changes nothing. See
	// fast.Engine.Stages for why the split exists.
	Stages func(stage string, d time.Duration)
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
		// K>1: the measured escalation trigger for multi-question queries is
		// cover-incompleteness, not thin confidence — arm the cover arm too,
		// or the defer never fires on exactly the queries that escalate.
		e.KB.Fast.DeferThinCover = len(fx) > 1
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

// srcLabel is the deterministic template's source label: the title, falling
// back to the id. Kept identical to the pre-refactor inline expression so the
// fallback text never changes.
func srcLabel(s source.Source) string {
	if s.Title != "" {
		return s.Title
	}
	return s.ID
}

// deepSynthBonus is the DEEP-tier synthesis calibration bump, applied at
// most once and never past 1.0. Default is now 0 — disabled. MEASURED
// 2026-09-26 (paired single run, same frozen base, 13-query set,
// scripts/bench/results-bonus0.json vs results-head.json): the 0.1 bonus
// bought nothing measurable — total wall 778s → 710s, refusals 4 → 3 with
// it OFF, i.e. no axis improved with it on. It also landed on
// fast.SkipBelow (0.35) and could move an answer across the answered/
// skipped line by itself (mean 4, coverage 0.2 → 0.30 → 0.40), which is
// no way for an unprovenanced constant to behave. Set
// CLUS_DEEP_SYNTH_BONUS=0.1 to restore the old behavior; a repeat run
// (R-E6) would firm the margins up.
func deepSynthBonus() float64 {
	v := strings.TrimSpace(os.Getenv("CLUS_DEEP_SYNTH_BONUS"))
	if v == "" {
		return 0
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return f
	}
	return 0
}

// deepMetrics derives the DEEP answer's coverage, confidence and deterministic
// template from the FINAL kept set. Both the primary build and the post-widen
// rebuild go through here: they used to be two ~30-line copies that had already
// drifted apart (the second re-applied the 0.45 incomplete-cover cap, the first
// did not), so a refused-and-recovered answer was scored by different rules
// than a first-pass one.
//
// The +0.1 synthesis bonus and the 0.45 incomplete-cover cap are the DEEP-tier
// calibration; they live in one place so they can be re-measured together.
func deepMetrics(query, srcTitle string, kept []mcs.Sample, rep facts.Report) (coverage, confidence float64, template string) {
	cov := mcs.Coverage(query, kept)
	mean := 0.0
	for _, sm := range kept {
		mean += sm.Score
	}
	if len(kept) > 0 {
		mean /= float64(len(kept))
	}
	conf := mcs.Confidence(mean, cov)
	if b := deepSynthBonus(); b > 0 && conf < 1 {
		conf = min1(conf + b)
	}
	var b strings.Builder
	b.WriteString("【DEEP 摘要】")
	b.WriteString(query)
	b.WriteString("\n")
	// 证据不足时必须先说结论，再说依据。以前这里直接跳到【来源】贴一段最接近的
	// 条文，只在末尾挂一行内部事实 id（"f1"），于是「拒答」在界面上长成了一个像
	// 答案的摘要——用户看到的是引文，读不出「这句话回答不了你的问题」。
	// 头部前缀保持原样：模板识别（fast.RefusedOfSummary 依赖「摘要】」）不能破。
	if !rep.Complete {
		b.WriteString("⚠ 证据不足：这份语料里没有能直接回答这个问题的依据。以下是最接近的原文片段，它不等于答案；请补充相关文档后再问。\n")
	}
	b.WriteString("【来源】")
	b.WriteString(srcTitle)
	b.WriteString("\n")
	for i, sm := range kept {
		fmt.Fprintf(&b, "[%d] (%s [%d,%d)) %s\n", i+1, sm.Source, sm.Start, sm.End, trim(sm.Content, 200))
	}
	if !rep.Complete {
		b.WriteString("\n【未覆盖需求】")
		b.WriteString(missingTexts(rep))
		// Weakest-requirement floor: open facts cap confidence.
		if conf > 0.45 {
			conf = 0.45
		}
	}
	return cov, conf, b.String()
}

// missingTexts renders uncovered requirements as the questions a reader asked,
// not the internal fact ids ("f1" means nothing to whoever typed the query).
func missingTexts(rep facts.Report) string {
	texts := map[string]string{}
	for _, f := range rep.Facts {
		if f.Query != "" {
			texts[f.ID] = f.Query
		}
	}
	out := make([]string, 0, len(rep.Missing))
	for _, id := range rep.Missing {
		if text, ok := texts[id]; ok {
			out = append(out, text)
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return "（未细分）"
	}
	return strings.Join(out, "; ")
}

// topKeeps sorts kept windows by score and truncates to the synthesis budget.
// Failed observations are dropped; overlapping/adjacent windows on the same
// source are merged first (GrepRAG: information density > rerank).
func topKeeps(kept []mcs.Sample) []mcs.Sample {
	return topKeepsWith(kept, nil)
}

// topKeepsWith is topKeeps plus optional body-aware boundary expansion (A5).
func topKeepsWith(kept []mcs.Sample, sources []source.Source) []mcs.Sample {
	live := kept[:0:0]
	for _, sm := range kept {
		if sm.Failed() {
			continue
		}
		live = append(live, sm)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Score > live[j].Score })
	live = consolidateWindows(live)
	if sources != nil {
		live = expandWindows(live, sources)
		live = consolidateWindows(live)
		// Consolidation can span further than any single window it absorbed,
		// and it has no body in scope to rebuild the text. Re-derive every kept
		// window from its coordinates last: otherwise the span and the text
		// describe different ranges, citations fail to resolve, and the cluster
		// built from them is judged stale on the next read.
		live = resyncContent(live, sources)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Score > live[j].Score })
	keep := maxKeepWindows
	if n, err := strconv.Atoi(os.Getenv("CLUS_DEEP_KEEP_WINDOWS")); err == nil && n > 0 {
		keep = n
	}
	if len(live) > keep {
		live = live[:keep]
	}
	return live
}

// mergeGap is the maximum rune gap between two windows on the same source
// before they stop being "adjacent" (GrepRAG merges overlapping OR adjacent
// slices into one continuous block).
const mergeGap = 1

// consolidateWindows merges overlapping or adjacent windows on the same
// source into continuous spans (GrepRAG structure-aware dedup). Score takes
// the max, covers are unioned. Input need not be sorted; output is by
// (source, start). Content keeps the longest original slice — citations still
// pin (source, start, end).
func consolidateWindows(kept []mcs.Sample) []mcs.Sample {
	if len(kept) <= 1 {
		return kept
	}
	work := append([]mcs.Sample(nil), kept...)
	sort.Slice(work, func(i, j int) bool {
		if work[i].Source != work[j].Source {
			return work[i].Source < work[j].Source
		}
		if work[i].Start != work[j].Start {
			return work[i].Start < work[j].Start
		}
		return work[i].End < work[j].End
	})
	out := work[:0]
	for _, sm := range work {
		if len(out) == 0 {
			out = append(out, sm)
			continue
		}
		last := &out[len(out)-1]
		if last.Source != sm.Source || sm.Start > last.End+mergeGap {
			out = append(out, sm)
			continue
		}
		if sm.End > last.End {
			last.End = sm.End
		}
		if sm.Start < last.Start {
			last.Start = sm.Start
		}
		if sm.Score > last.Score {
			last.Score = sm.Score
			if sm.Arm != "" {
				last.Arm = sm.Arm
			}
		}
		last.Covers = unionStrings(last.Covers, sm.Covers)
		if len(sm.Content) > len(last.Content) {
			last.Content = sm.Content
		}
		if len(sm.Reasoning) > len(last.Reasoning) {
			last.Reasoning = sm.Reasoning
		}
	}
	return out
}

// resyncContent re-derives each window's text from its source coordinates.
// Samples carrying a sampling-method label instead of a document id (the
// FAST/cluster-reuse shape) have no body here and are left alone.
func resyncContent(kept []mcs.Sample, sources []source.Source) []mcs.Sample {
	if len(kept) == 0 || len(sources) == 0 {
		return kept
	}
	byID := make(map[string][]rune, len(sources))
	for _, s := range sources {
		byID[s.ID] = []rune(s.Body)
	}
	for i := range kept {
		body, ok := byID[kept[i].Source]
		if !ok {
			continue
		}
		start, end := kept[i].Start, kept[i].End
		if start < 0 || start >= end || end > len(body) {
			continue
		}
		kept[i].Content = string(body[start:end])
	}
	return kept
}

// expandWindows grows each kept span by expandMargin runes against the live
// body (expand boundaries for readable continuous blocks
// while the span stays the source of truth for citations).
const expandMargin = 24

func expandWindows(kept []mcs.Sample, sources []source.Source) []mcs.Sample {
	if len(kept) == 0 || len(sources) == 0 {
		return kept
	}
	byID := map[string]source.Source{}
	for _, s := range sources {
		byID[s.ID] = s
	}
	out := append([]mcs.Sample(nil), kept...)
	for i := range out {
		src, ok := byID[out[i].Source]
		if !ok {
			continue
		}
		runes := []rune(src.Body)
		start := out[i].Start - expandMargin
		if start < 0 {
			start = 0
		}
		end := out[i].End + expandMargin
		if end > len(runes) {
			end = len(runes)
		}
		if start >= end {
			continue
		}
		if start != out[i].Start || end != out[i].End {
			out[i].Start, out[i].End = start, end
			out[i].Content = string(runes[start:end])
		}
	}
	return out
}

func unionStrings(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// runDeep is the ReAct-shaped loop with per-fact coverage:
// sample sources → evaluate fact coverage → bounded self-correction on the
// weakest (missing) requirements → synthesize. Offline stub is deterministic.
// Returns admitted source IDs (every file the loop scored) for ir-rag 1.5
// not-retrieved decomposition.
// citationCorpus is the source set citations resolve against: the corpus the
// run started from plus whatever widening admitted from outside it. Without
// the extras a widened window resolves to no title and no quote, so the answer
// cites a source it cannot show.
func citationCorpus(corpus, widened []source.Source) []source.Source {
	if len(widened) == 0 {
		return corpus
	}
	out := make([]source.Source, 0, len(corpus)+len(widened))
	out = append(out, corpus...)
	return append(out, widened...)
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

func (e *Engine) runDeep(ctx context.Context, query string, sources []source.Source, affinity map[string]bool) (deepOutcome, error) {
	// Sampling telemetry: every admission/widen/self-correct SampleBody call
	// accumulates here and is reported once on exit (any exit path). DEEP's
	// repeated whole-body window scoring is where this corpus's token burn
	// lives, so it needs its own line in the per-stage split.
	var sampleNS int64
	defer func() {
		if sampleNS > 0 && e.Stages != nil {
			e.Stages("deep_sample", time.Duration(sampleNS))
		}
	}()
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
			e.OnFile(s.BusinessKey, localBest, len(samples))
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
		if e.SynthDelta != nil {
			if ss, ok := e.Synth.(fast.StreamSynthesizer); ok {
				if s, err := ss.SynthesizeStream(ctx, query, kept, e.SynthDelta); err == nil && strings.TrimSpace(s) != "" {
					if e.stageTok != nil {
						endSyn(&e.stageTok.Synth)
					}
					e.stage("deep_synth", t0)
					return s
				}
			}
		}
		s, err := e.Synth.Synthesize(ctx, query, kept)
		if e.stageTok != nil {
			endSyn(&e.stageTok.Synth)
		}
		e.stage("deep_synth", t0)
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
func (e *Engine) stage(name string, t0 time.Time) {
	if e.Stages != nil {
		e.Stages(name, time.Since(t0))
	}
}

func (e *Engine) conflictsFor(ctx context.Context, id string) []Conflict {
	if e.Conflicts == nil || id == "" {
		return nil
	}
	all, err := e.Conflicts.All(ctx)
	if err != nil {
		return nil
	}
	var out []Conflict
	for _, c := range all {
		if c.A == id || c.B == id {
			out = append(out, c)
		}
	}
	return out
}

// DetectConflict records a contested pair (content disagreement proxy: two
// clusters with different source_ids and overlapping queries but divergent
// numeric claims in content — offline heuristic).
func DetectConflict(ctx context.Context, st ConflictStore, a, b cluster.Cluster) (Conflict, error) {
	if a.ID == "" || b.ID == "" || a.ID == b.ID {
		return Conflict{}, fmt.Errorf("deep: need two distinct clusters")
	}
	claimA := claimOf(a)
	claimB := claimOf(b)
	if claimA == "" || claimB == "" || claimA == claimB {
		return Conflict{}, fmt.Errorf("deep: no divergent numeric claims")
	}
	from, to := a.ID, b.ID
	if from > to {
		from, to = to, from
	}
	c := Conflict{
		A: from, B: to, Group: "claim:" + claimA + "_vs_" + claimB,
		Reason: fmt.Sprintf("divergent claims: %s vs %s", claimA, claimB),
	}
	if err := st.Save(ctx, c); err != nil {
		return Conflict{}, err
	}
	// Lifecycle: contested.
	return c, nil
}

// claimOf pulls the divergent-claim number from the evidence windows first
// (raw source text), falling back to the rendered summary — summary offsets
// like "[0,21)" are not claims and must not win.
func claimOf(c cluster.Cluster) string {
	return cluster.NumericClaim(c)
}

// BuildCitations maps answer samples back to source spans, so a citation
// resolves to the original text.
// Each sample resolves against its own source: DEEP keeps multi-source windows
// with sm.Source = doc id, while FAST/cluster-reuse samples carry a sampling
// method label and fall back to the answer's single source. Resolved means the
// window pins back exactly — quote equals the current body slice — so stale
// windows (source updated after sampling) surface as [?] instead of现证.
func BuildCitations(query string, ans fast.Answer, sources []source.Source) CitationSet {
	var refs []Ref
	srcMap := map[string]source.Source{}
	for _, s := range sources {
		srcMap[s.ID] = s
	}
	i := 0
	for _, sm := range cluster.NormalizeEvidence(ans.SourceID, ans.Samples) {
		i++
		id := sm.Source
		src := srcMap[id]
		body := []rune(src.Body)
		r := Ref{
			Index: i, SourceID: id, Title: src.Title,
			Start: sm.Start, End: sm.End,
			Quote: trim(sm.Content, 120),
		}
		r.Resolved = sm.Start >= 0 && sm.Start < sm.End && sm.End <= len(body) &&
			string(body[sm.Start:sm.End]) == sm.Content
		if src.Body != "" {
			for _, sp := range src.Structure {
				if sp.Start <= sm.Start && sm.End <= sp.End {
					r.Span = sp.Label
					break
				}
			}
		}
		// [?] when the window cannot be pinned back.
		if !r.Resolved {
			r.Quote = "[?] " + r.Quote
		}
		refs = append(refs, r)
	}
	return CitationSet{Refs: refs}
}

func legend(cs CitationSet, unresolved bool) string {
	base := "引用编号 [n] 对应下方 refs；(span) 为原文定位标签。"
	if unresolved {
		return base + " [?] 表示引用未能精确回溯原文窗口（源已更新或定位越界），请以原文为准。"
	}
	return base
}

func min1(x float64) float64 {
	if x > 1 {
		return 1
	}
	return x
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Package deep is the DEEP tier: confidence-gated escalation from FAST,
// multi-source evidence refinement (ReAct-shaped tool loop, offline stub),
// conflict detection between clusters, and the citation delivery face ([?]
// legend for unresolved refs). Low confidence must escalate; conflict
// pairs must be discoverable; citations must resolve to source offsets.
package deep

import (
	"context"
	"fmt"
	"os"
	"sort"
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
const EscalateBelow = 0.35

// MaxLoops bounds the DEEP tool loop (Sirchmunk max_loops analogue).
const MaxLoops = 6

// widenBudget is the widening pass's own file allowance, independent of
// MaxLoops (the initial admission must not starve exploration).
const widenBudget = 4

// correctBudget is the self-correction file allowance (D4): admission
// usually spends MaxLoops on the first wave; missing-fact re-sampling must
// not share that clock or the weakest-requirement pass never runs.
const correctBudget = 3

// maxKeepWindows is the synthesis budget: only the top-scored windows are
// handed to the synthesizer / returned in the answer.
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
	ID     string `json:"_id"`
	A      string `json:"a"`
	B      string `json:"b"`
	Group  string `json:"group"`
	Reason string `json:"reason"`
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
	Answer     fast.Answer          `json:"answer"`
	Escalated  bool                 `json:"escalated"`
	Mode       string               `json:"mode"`
	Loops      int                  `json:"loops"`
	Citations  CitationSet          `json:"citations"`
	Conflicts  []Conflict           `json:"conflicts,omitempty"`
	ClusterID  string               `json:"cluster_id,omitempty"`
	ClusterVer int                  `json:"cluster_version,omitempty"`
	Reused     bool                 `json:"reused"`
	Sampled    int                  `json:"sampled"`
	Persisted  bool                 `json:"persisted"`
	Merged     bool                 `json:"merged"`
	Neighbors  []graph.ExpandResult `json:"neighbors,omitempty"`
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
	// Budget accounting: LatencyMS is always filled; Tokens carries the
	// upstream-reported total (0 on the offline stub path — the CLI fills it
	// from the chat client after Ask returns). BudgetHit marks an independent
	// token-budget stop (judge tokens never enter TokenBudget).
	Tokens    int64 `json:"tokens,omitempty"`
	LatencyMS int64 `json:"latency_ms,omitempty"`
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
	// AbstainEarly marks a pre-DEEP refusal (FAST had zero usable evidence
	// and p_fail cleared EarlyAbove): DEEP was skipped to save budget.
	AbstainEarly bool `json:"abstain_early,omitempty"`
}

// Engine runs FAST and escalates into DEEP when confidence is thin.
type Engine struct {
	KB        *kb.Engine
	Conflicts ConflictStore
	// Scorer rates evidence windows in the DEEP loop. nil = offline
	// KeywordScorer (gate carrier); production wires llm.AigateScorer (D6).
	Scorer mcs.Scorer
	// Synth renders DEEP summaries (synthesize_roi). nil = deterministic
	// template; production wires llm.AigateSynthesizer.
	Synth fast.Synthesizer
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
	// TokenBudget is an independent stop (LENS Def 3 / Remark 2): when > 0
	// and TokensUsed is wired, the DEEP loop checks remaining budget before
	// scoring each admitted file. Judge tokens never enter this budget.
	TokenBudget int64
	TokensUsed  func() int64
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
// llm.AigateQuerySimulator in production; offline stubs in tests.
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
	return &Engine{KB: k, Conflicts: conflicts, EscalateBelow: EscalateBelow}
}

// rankAdmission orders sources before exploration (nil → untouched order).
// affinity = law prefixes of sources already answered (FAST hit + kept), so
// the ranker can put same-law articles first (同法亲缘).
func (e *Engine) rankAdmission(ctx context.Context, query string, sources []source.Source, affinity map[string]bool) []source.Source {
	if e.RankAdmission == nil {
		return sources
	}
	out, err := e.RankAdmission(ctx, query, sources, affinity)
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

func (e *Engine) scorer() mcs.Scorer {
	if e.Scorer != nil {
		return e.Scorer
	}
	return mcs.KeywordScorer{}
}

// gammaStep raises the escalation line per extra atomic fact (B10): a
// multi-fact comparison must not stop on a single-fact-quality answer.
const gammaStep = 0.05

// thresholdFor modulates the escalation line by intent shape (B10 γ(I)):
// single-fact lookups stop at the base line, multi-fact comparisons demand
// proportionally more (capped at 0.6 so DEEP stays reachable).
func (e *Engine) thresholdFor(fx []facts.Fact) float64 {
	thr := e.EscalateBelow
	if thr <= 0 {
		thr = EscalateBelow
	}
	extra := len(fx) - 1
	if extra > 3 {
		extra = 3
	}
	thr += gammaStep * float64(extra)
	if thr > 0.6 {
		thr = 0.6
	}
	return thr
}

// Ask runs the confidence-gated path: insufficient confidence escalates.
func (e *Engine) Ask(ctx context.Context, query string, sources []source.Source) (res Result, err error) {
	started := time.Now()
	defer func() { res.LatencyMS = time.Since(started).Milliseconds() }()
	if e.Sources == nil {
		e.Sources = sources
	}
	query = e.effectiveQuery(ctx, query)
	fx := facts.Build(query)
	thr := e.thresholdFor(fx) // B10 γ(I): multi-fact intents stop stricter

	// FILENAME_ONLY tier (D5 附档): name/extension lookups answer before any
	// retrieval, with 0 LLM calls.
	if ans, ok := fast.MatchFilename(query, sources); ok {
		return Result{Answer: ans, Mode: fast.ModeFilenameOnly}, nil
	}

	// Try L2 reuse first (0-sample path).
	base, err := e.KB.Ask(ctx, query, sources)
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
	defer func() { res.LatencyMS = time.Since(started).Milliseconds() }()
	if load == nil {
		return Result{}, fmt.Errorf("deep: AskLazy requires a loader")
	}
	if e.Sources == nil {
		// Keep the engine's cached corpus view honest; the lazy path fills
		// it when (and only when) the corpus is actually needed.
		_ = e.Sources
	}
	query = e.effectiveQuery(ctx, query)
	fx := facts.Build(query)
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
	return e.Ask(ctx, query, all)
}

// afterBase is the shared post-reuse path: citations, conflicts, cover, the
// abstain gates and the DEEP escalation. `citeCorpus` is the source set
// citation refs resolve against (the full corpus, or the narrow anchor set
// on a warm hit); `load` materializes the full corpus when DEEP actually
// escalates (nil = caller already provided it).
func (e *Engine) afterBase(ctx context.Context, started time.Time, query string, base kb.Result, citeCorpus []source.Source, thr float64, fx []facts.Fact, load SourceLoader) (res Result, err error) {
	defer func() { res.LatencyMS = time.Since(started).Milliseconds() }()
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
	need := base.Answer.Skipped || base.Answer.Refused || base.Answer.Confidence < thr || len(base.Answer.Samples) == 0 ||
		(!res.Cover.Complete && (!base.Reused || len(fx) > 1))
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
					res.Answer.Summary = "证据不足，暂不作答"
				}
				res.AbstainEarly = true
				res.Citations.Legend = legend(res.Citations, false)
				return res, nil
			}
		}
	}
	if !need {
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
	deepAns, cover, loops, wid, sc, admitted, cited, err := e.runDeep(ctx, query, deepCorpus, affinity)
	if err != nil {
		return Result{}, err
	}
	res.Loops = loops
	res.Answer = deepAns
	res.Cover = cover
	res.SelfCorrected = sc
	res.Widened = wid
	res.Admitted = admitted
	res.Citations = BuildCitations(query, deepAns, cited)
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
		for _, sm := range deepAns.Samples {
			if sm.Score > top {
				top = sm.Score
			}
		}
		f := abstain.FromAnswer(query, len(deepCorpus), len(deepAns.Samples), top,
			len(cover.Missing), deepAns.Confidence,
			deepAns.Skipped, deepAns.Refused)
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

	sub, err := e.KB.Persist(ctx, deepAns, deepCorpus)
	if err != nil {
		return Result{}, err
	}
	res.ClusterID = sub.ClusterID
	res.ClusterVer = sub.ClusterVer
	res.Persisted = sub.Persisted
	res.Merged = sub.Merged
	res.Reused = false
	res.Sampled = len(deepAns.Samples)
	res.BudgetHit = e.BudgetHit
	return res, nil
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
	if len(live) > maxKeepWindows {
		live = live[:maxKeepWindows]
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

func (e *Engine) runDeep(ctx context.Context, query string, sources []source.Source, affinity map[string]bool) (fast.Answer, facts.Report, int, int, bool, []string, []source.Source, error) {
	if affinity == nil {
		affinity = map[string]bool{}
	}
	fx := facts.Build(query)
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
	newSampler := func() *mcs.Sampler {
		smp := mcs.New(mcs.DefaultConfig(), e.scorer())
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
	var kept []mcs.Sample
	var bestSrc source.Source
	bestScore := -1.0
	rep := report(kept)
	for _, s := range ranked {
		if s.Status != source.StatusActive {
			continue
		}
		// Weakest-requirement stop: the strongest window with full
		// coverage is enough — sampling further admitted files wastes budget
		// and latency (真机: 85s/13944 tokens 空转在已答问题上).
		if rep.Complete && bestScore >= 8 {
			if e.Verbose != nil {
				e.Verbose("early stop: covered, best=%.1f, files=%d", bestScore, loops)
			}
			break
		}
		loops++
		if loops > MaxLoops {
			break
		}
		tried[s.ID] = true
		// Independent token stop (LENS Def 3): check BEFORE scoring this
		// file so an exhausted budget never starts another oracle batch.
		if e.budgetHit() {
			if e.Verbose != nil {
				e.Verbose("token budget hit after %d files", loops)
			}
			break
		}
		samples, err := newSampler().SampleBody(ctx, query, s.Body)
		if err != nil {
			if e.Verbose != nil {
				e.Verbose("file %s: sample error %v", s.BusinessKey, err)
			}
			continue
		}
		localBest := 0.0
		for _, sm := range samples {
			if sm.Score > localBest {
				localBest = sm.Score
			}
			if sm.Score >= 4 {
				sm.Source = s.ID
				kept = append(kept, sm)
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
	}

	// Self-correction (D4): own budget, independent of admission MaxLoops.
	// Prefer files admission never reached, then re-sample tried ones with
	// the missing-fact queries.
	selfCorrected := false
	if !rep.Complete && correctBudget > 0 && !e.budgetHit() {
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
			smp := mcs.New(mcs.DefaultConfig(), e.scorer())
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
			}
		}
	outer_correct:
		for _, mq := range mqs {
			for _, s := range order {
				if correctUsed >= correctBudget || e.budgetHit() {
					break outer_correct
				}
				correctUsed++
				loops++
				tried[s.ID] = true
				samples, err := exploreSampler().SampleBody(ctx, mq, s.Body)
				if err != nil {
					continue
				}
				localBest := 0.0
				for _, sm := range samples {
					if sm.Score > localBest {
						localBest = sm.Score
					}
					if sm.Score >= 4 {
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
	if (facts.NeedContinue(rep, 0, MaxLoops) || bestScore < 6) && e.Widen != nil && !e.budgetHit() {
		keptIDs := map[string]bool{}
		for _, sm := range kept {
			keptIDs[sm.Source] = true
		}
		for k := range lawAffinity(keptIDs, sources) {
			affinity[k] = true
		}
		if extra, err := e.Widen(ctx, query, widenExclude(), widenBudget, affinity); err == nil && len(extra) > 0 {
			widenedDocs = append(widenedDocs, extra...)
			for _, s := range extra {
				if e.budgetHit() {
					break
				}
				loops++
				widened++
				tried[s.ID] = true
				samples, err := newSampler().SampleBody(ctx, query, s.Body)
				if err != nil {
					continue
				}
				localBest := 0.0
				for _, sm := range samples {
					if sm.Score > localBest {
						localBest = sm.Score
					}
					if sm.Score >= 4 {
						sm.Source = s.ID
						kept = append(kept, sm)
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
		return fast.Answer{
			Query: query, Mode: ModeDEEP, LLMCalls: loops, Skipped: true,
			Summary: "深度检索仍证据不足",
		}, rep, loops, widened, selfCorrected, admissionIDs(tried), citationCorpus(sources, widenedDocs), nil
	}
	// D2: truncate THEN recompute Cover so res.Cover matches what synthesis sees.
	kept = topKeepsWith(kept, sources)
	rep = report(kept)
	cov := mcs.Coverage(query, kept)
	mean := 0.0
	for _, sm := range kept {
		mean += sm.Score
	}
	mean /= float64(len(kept))
	conf := mcs.Confidence(mean, cov)
	if conf < 1 {
		conf = min1(conf + 0.1)
	}
	var b strings.Builder
	b.WriteString("【DEEP 摘要】")
	b.WriteString(query)
	b.WriteString("\n【来源】")
	title := bestSrc.Title
	if title == "" {
		title = bestSrc.ID
	}
	b.WriteString(title)
	b.WriteString("\n")
	for i, sm := range kept {
		fmt.Fprintf(&b, "[%d] (%s [%d,%d)) %s\n", i+1, sm.Source, sm.Start, sm.End, trim(sm.Content, 200))
	}
	if !rep.Complete {
		b.WriteString("\n【未覆盖需求】")
		b.WriteString(strings.Join(rep.Missing, ", "))
		// Weakest-requirement floor: open facts cap confidence.
		if conf > 0.45 {
			conf = 0.45
		}
	}
	buildAnswer := func(template string) fast.Answer {
		return fast.Answer{
			Query: query, Mode: ModeDEEP, LLMCalls: loops,
			SourceID: bestSrc.ID, Samples: kept, Coverage: cov,
			Confidence: conf, Summary: e.render(ctx, query, kept, template),
			Skipped: conf < 0.35,
		}
	}
	best = buildAnswer(b.String())
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
		if extra, err := e.Widen(ctx, query, exclude, widenBudget, affinity); err == nil && len(extra) > 0 {
			widenedDocs = append(widenedDocs, extra...)
			for _, s := range extra {
				if e.budgetHit() {
					break
				}
				loops++
				widened++
				tried[s.ID] = true
				samples, err := newSampler().SampleBody(ctx, query, s.Body)
				if err != nil {
					continue
				}
				localBest := 0.0
				for _, sm := range samples {
					if sm.Score > localBest {
						localBest = sm.Score
					}
					if sm.Score >= 4 {
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
				cov = mcs.Coverage(query, kept)
				mean = 0.0
				for _, sm := range kept {
					mean += sm.Score
				}
				mean /= float64(len(kept))
				conf = mcs.Confidence(mean, cov)
				if conf < 1 {
					conf = min1(conf + 0.1)
				}
				if !rep.Complete && conf > 0.45 {
					conf = 0.45
				}
				var b2 strings.Builder
				b2.WriteString("【DEEP 摘要】")
				b2.WriteString(query)
				b2.WriteString("\n【来源】")
				title := bestSrc.Title
				if title == "" {
					title = bestSrc.ID
				}
				b2.WriteString(title)
				b2.WriteString("\n")
				for i, sm := range kept {
					fmt.Fprintf(&b2, "[%d] (%s [%d,%d)) %s\n", i+1, sm.Source, sm.Start, sm.End, trim(sm.Content, 200))
				}
				if !rep.Complete {
					b2.WriteString("\n【未覆盖需求】")
					b2.WriteString(strings.Join(rep.Missing, ", "))
				}
				best = buildAnswer(b2.String())
				best.Refused = fast.RefusedOf(e.Synth) || fast.RefusedOfSummary(best.Summary, e.Synth)
			}
		}
	}
	return best, rep, loops, widened, selfCorrected, admissionIDs(tried), citationCorpus(sources, widenedDocs), nil
}

// render prefers the production Synthesizer (synthesize_roi) and degrades to
// the deterministic DEEP template on refusal/error.
func (e *Engine) render(ctx context.Context, query string, kept []mcs.Sample, template string) string {
	if e.Synth != nil {
		if s, err := e.Synth.Synthesize(ctx, query, kept); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return template
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

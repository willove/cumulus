// Package deep is the DEEP tier: confidence-gated escalation from FAST,
// multi-source evidence refinement (ReAct-shaped tool loop, offline stub),
// conflict detection between clusters, and the citation delivery face ([?]
// legend for unresolved refs). Gate D: low confidence must escalate; conflict
// pairs must be discoverable; citations must resolve to source offsets.
package deep

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cumubase/ask/internal/cluster"
	"github.com/cumubase/ask/internal/facts"
	"github.com/cumubase/ask/internal/fast"
	"github.com/cumubase/ask/internal/graph"
	"github.com/cumubase/ask/internal/kb"
	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

// EscalateBelow is the confidence line under which FAST upgrades to DEEP
// (plan D5: 置信不足 → DEEP / ReAct).
const EscalateBelow = 0.35

// MaxLoops bounds the DEEP tool loop (Sirchmunk max_loops analogue).
const MaxLoops = 6

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

// Conflict is a contested link between two clusters (ask_conflicts).
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
	// Cover is the per-fact multi-hop coverage report (LENS B1/B2).
	Cover facts.Report `json:"cover"`
	// SelfCorrected marks a weakest-requirement re-sample pass.
	SelfCorrected bool `json:"self_corrected"`
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

func (e *Engine) scorer() mcs.Scorer {
	if e.Scorer != nil {
		return e.Scorer
	}
	return mcs.KeywordScorer{}
}

// Ask runs the confidence-gated path (门 D: 置信不足必升级).
func (e *Engine) Ask(ctx context.Context, query string, sources []source.Source) (Result, error) {
	if e.Sources == nil {
		e.Sources = sources
	}
	thr := e.EscalateBelow
	if thr <= 0 {
		thr = EscalateBelow
	}
	query = e.effectiveQuery(ctx, query)

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
	res := Result{
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
	res.Citations = BuildCitations(query, base.Answer, sources)
	res.Conflicts = e.conflictsFor(ctx, base.ClusterID)
	res.Cover = facts.Evaluate(facts.Build(query), base.Answer.Samples)

	// Tier exits: non-search intents never escalate.
	switch base.Answer.Mode {
	case fast.ModeChat, fast.ModeDocSummary:
		res.Mode = base.Answer.Mode
		return res, nil
	}

	// Gate: thin evidence, skipped, or open multi-hop requirement → DEEP.
	need := base.Answer.Skipped || base.Answer.Confidence < thr || len(base.Answer.Samples) == 0 || !res.Cover.Complete
	if !need {
		res.Citations.Legend = legend(res.Citations, false)
		return res, nil
	}

	res.Escalated = true
	res.Mode = ModeDEEP
	deepAns, cover, loops, sc, err := e.runDeep(ctx, query, sources)
	if err != nil {
		return Result{}, err
	}
	res.Loops = loops
	res.Answer = deepAns
	res.Cover = cover
	res.SelfCorrected = sc
	res.Citations = BuildCitations(query, deepAns, sources)
	// Mark unresolved refs when DEEP still cannot pin a quote.
	unresolved := false
	for _, r := range res.Citations.Refs {
		if !r.Resolved {
			unresolved = true
			break
		}
	}
	res.Citations.Legend = legend(res.Citations, unresolved)

	// Persist DEEP result as cluster when solid enough.
	if !deepAns.Skipped && deepAns.SourceID != "" {
		sub, err := e.KB.Ask(ctx, query, sources)
		if err == nil {
			res.ClusterID = sub.ClusterID
			res.Reused = sub.Reused
		}
	}
	return res, nil
}

// runDeep is the ReAct-shaped loop with per-fact coverage (LENS B1/B2):
// sample sources → evaluate fact coverage → bounded self-correction on the
// weakest (missing) requirements → synthesize. Offline stub is deterministic.
func (e *Engine) runDeep(ctx context.Context, query string, sources []source.Source) (fast.Answer, facts.Report, int, bool, error) {
	fx := facts.Build(query)
	var best fast.Answer
	loops := 0
	var kept []mcs.Sample
	var bestSrc source.Source
	bestScore := -1.0
	for _, s := range sources {
		if s.Status != source.StatusActive {
			continue
		}
		loops++
		if loops > MaxLoops {
			break
		}
		smp := mcs.New(mcs.DefaultConfig(), e.scorer())
		samples, err := smp.SampleBody(ctx, query, s.Body)
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
	rep := facts.Evaluate(fx, kept)

	// Self-correction: weakest requirement still open → one bounded re-sample.
	selfCorrected := false
	if facts.NeedContinue(rep, loops, MaxLoops) {
		selfCorrected = true
		for _, mq := range facts.MissingQueries(fx, rep) {
			for _, s := range sources {
				if s.Status != source.StatusActive {
					continue
				}
				loops++
				if loops > MaxLoops {
					break
				}
				smp := mcs.New(mcs.DefaultConfig(), e.scorer())
				samples, err := smp.SampleBody(ctx, mq, s.Body)
				if err != nil {
					continue
				}
				for _, sm := range samples {
					if sm.Score >= 4 {
						sm.Source = s.ID
						kept = append(kept, sm)
					}
				}
			}
		}
		rep = facts.Evaluate(fx, kept)
	}

	loops++
	if bestSrc.ID == "" || len(kept) == 0 {
		return fast.Answer{
			Query: query, Mode: ModeDEEP, LLMCalls: loops, Skipped: true,
			Summary: "深度检索仍证据不足",
		}, rep, loops, selfCorrected, nil
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].Score > kept[j].Score })
	if len(kept) > 8 {
		kept = kept[:8]
	}
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
		// Weakest-requirement floor (B2): open facts cap confidence.
		if conf > 0.45 {
			conf = 0.45
		}
	}
	best = fast.Answer{
		Query: query, Mode: ModeDEEP, LLMCalls: loops,
		SourceID: bestSrc.ID, Samples: kept, Coverage: cov,
		Confidence: conf, Summary: e.render(ctx, query, kept, b.String()),
		Skipped: conf < 0.35,
	}
	return best, rep, loops, selfCorrected, nil
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
	for _, sm := range c.Evidence {
		if n := extractNumber(sm.Content); n != "" {
			return n
		}
	}
	return extractNumber(c.Content)
}

func extractNumber(s string) string {
	var digits []rune
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
			continue
		}
		if len(digits) > 0 {
			return string(digits)
		}
	}
	return string(digits)
}

// BuildCitations maps answer samples back to source spans (门 D: 引用可点回原文).
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
	for _, sm := range ans.Samples {
		i++
		id := sm.Source
		src, ok := srcMap[id]
		if !ok {
			id = ans.SourceID
			src = srcMap[id]
		}
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

// Package fast is the default search tier: intent tiering, greedy keyword
// cascade over L0 sources, context windows around hits, one synthesis call.
// Two LLM-shaped steps in production (analyze + synthesize, both via aigate);
// offline RuleAnalyzer/KeywordScorer/Template keep the path deterministic for
// gates.
package fast

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/prior"
	"github.com/willove/cumulus/internal/source"
)

// Answer modes (D5 tiers + intent exits).
const (
	ModeFAST         = "FAST"
	ModeFilenameOnly = "FILENAME_ONLY"
	ModeChat         = "CHAT"
	ModeDocSummary   = "DOC_SUMMARY"
)

// Answer is the FAST-path result.
type Answer struct {
	Query      string       `json:"query"`
	Confidence float64      `json:"confidence"`
	Summary    string       `json:"summary"`
	Samples    []mcs.Sample `json:"samples"`
	SourceID   string       `json:"source_id"`
	Coverage   float64      `json:"coverage"`
	Mode       string       `json:"mode"`
	LLMCalls   int          `json:"llm_calls"`
	Skipped    bool         `json:"skipped"`
	Refused    bool         `json:"refused,omitempty"` // synthesis refused (insufficient evidence)
}

// Analysis is the low-cost query analysis (fast_analyze contract): intent plus
// two keyword granularities with IDF weights.
type Analysis struct {
	Intent   string             `json:"intent"` // search | chat | doc_summary
	Primary  map[string]float64 `json:"primary"`
	Fallback map[string]float64 `json:"fallback"`
}

// Analyzer classifies a query and extracts the keyword cascade. Production
// wires llm.AigateAnalyzer (fast_analyze prompt); RuleAnalyzer is the offline
// gate carrier.
type Analyzer interface {
	Analyze(ctx context.Context, query string) (Analysis, error)
}

// Synthesizer renders the answer summary from evidence windows
// (synthesize_roi contract: citation-marked, refuse-capable). nil → the
// deterministic template.
type Synthesizer interface {
	Synthesize(ctx context.Context, query string, samples []mcs.Sample) (string, error)
}

// RefusalReporter is the optional half of Synthesizer: it reports whether the
// last synthesis REFUSED to answer (evidence insufficient). KB uses it to
// refuse cluster persistence — a refused answer is not knowledge (Sirchmunk's
// files_read=0 cluster is the cautionary case). Not implemented → false.
type RefusalReporter interface {
	Refused() bool
}

// KeywordExpander yields progressively finer keyword levels
// (keywords_multilevel contract) when the primary cascade misses.
type KeywordExpander interface {
	Expand(ctx context.Context, query string, levels int) ([][]string, error)
}

// Engine runs FAST search over a set of active sources.
// Engine struct — search tier state.
type Engine struct {
	Sampler  *mcs.Sampler
	MaxChars int
	Analyzer Analyzer
	Synth    Synthesizer
	Expander KeywordExpander
	// UsePrior ranks candidates with the multi-signal prior
	// (internal/prior, opt-in; plain IDF cascade by default).
	UsePrior bool
	// PriorHist feeds the prior's history arm from successful evidence
	// (clus_evidence). nil = history arm silent.
	PriorHist *prior.History
	// Verbose, when set, receives per-step diagnostics (serve -verbose /
	// CLUS_VERBOSE). nil → silent. It exists so a degraded step (a failed
	// expander, a refused synthesis) is visible instead of looking like a
	// clean miss.
	Verbose func(format string, a ...any)
}

func New(scorer mcs.Scorer) *Engine {
	return &Engine{
		Sampler:  mcs.New(mcs.EnvConfig(), scorer),
		MaxChars: 15000,
	}
}

// Search analyzes intent, ranks sources with the keyword cascade, then samples
// the best file (mcs). Chat/doc-summary intents exit without retrieval.
func (e *Engine) Search(ctx context.Context, query string, sources []source.Source) (Answer, error) {
	if strings.TrimSpace(query) == "" {
		return Answer{}, fmt.Errorf("fast: empty query")
	}
	an := e.Analyzer
	if an == nil {
		an = RuleAnalyzer{}
	}
	a, err := an.Analyze(ctx, query)
	if err != nil {
		return Answer{}, err
	}
	switch a.Intent {
	case IntentChat:
		return Answer{Query: query, Mode: ModeChat, LLMCalls: 1, Skipped: true,
			Summary: "（闲聊，不检索）"}, nil
	case IntentDocSummary:
		return Answer{Query: query, Mode: ModeDocSummary, LLMCalls: 1, Skipped: true,
			Summary: "整文档意图超出 v1 证据检索范围"}, nil
	}

	// Keyword cascade: primary → fallback → expander levels. calls counts the
	// LLM-shaped steps actually taken, so the D5/§6.2 gate ("FAST 档 LLM 调用
	// 次数 ≤2") is asserted against reality instead of a hardcoded 2 — the
	// expander is a third call and used to be invisible in the accounting.
	calls := 1 // the analyze call above
	ranked := e.rankFields(orderedKeys(a.Primary), sources)
	if len(ranked) == 0 {
		ranked = e.rankFields(orderedKeys(a.Fallback), sources)
	}
	if len(ranked) == 0 && e.Expander != nil {
		if ctx.Err() != nil {
			return Answer{}, ctx.Err()
		}
		levels, xerr := e.Expander.Expand(ctx, query, 3)
		if xerr == nil {
			calls++ // the expander really did call the model
			for _, lv := range levels {
				if len(lv) == 0 {
					continue
				}
				if ranked = rankSources(lv, sources); len(ranked) > 0 {
					break
				}
			}
		}
		// A failed expander is a degraded retrieval, not a silent no-op.
		if xerr != nil && e.Verbose != nil {
			e.Verbose("expander failed, cascade stays at primary/fallback: %v", xerr)
		}
	}
	if len(ranked) == 0 {
		return Answer{Query: query, Mode: ModeFAST, LLMCalls: calls, Skipped: true}, nil
	}
	if ctx.Err() != nil {
		return Answer{}, ctx.Err()
	}
	best := ranked[0].src
	samples, err := e.Sampler.SampleBody(ctx, query, best.Body)
	if err != nil {
		return Answer{}, err
	}
	var kept []mcs.Sample
	total := 0
	for _, sm := range samples {
		if sm.Score < 4 {
			continue
		}
		if total >= e.MaxChars {
			break
		}
		kept = append(kept, sm)
		total += len(sm.Content)
	}
	if len(kept) == 0 {
		// No synthesis happened on this path, so calls must not be incremented:
		// billing a call that never ran is exactly what the ≤2 gate exists to
		// make honest.
		return Answer{
			Query: query, Mode: ModeFAST, LLMCalls: calls, SourceID: best.ID,
			Skipped: true, Summary: "证据不足，未合成",
		}, nil
	}
	cov := mcs.Coverage(query, kept)
	mean := 0.0
	for _, sm := range kept {
		mean += sm.Score
	}
	mean /= float64(len(kept))
	conf := mcs.Confidence(mean, cov)
	summary := e.render(ctx, query, best, kept)
	return Answer{
		Query:      query,
		Mode:       ModeFAST,
		LLMCalls:   calls + 1, // + the synthesis call
		SourceID:   best.ID,
		Samples:    kept,
		Coverage:   cov,
		Confidence: conf,
		Summary:    summary,
		Skipped:    conf < SkipBelow,
		Refused:    RefusedOf(e.Synth) || RefusedOfSummary(summary, e.Synth),
	}, nil
}

// SkipBelow is the FAST confidence floor below which an answer is marked
// skipped. It mirrors deep.EscalateBelow so tuning one line cannot desynchronize
// the FAST skip flag from the DEEP escalation line.
const SkipBelow = 0.35

// AdmitByFields admits up to m active sources matching the given fields
// directly (the widen loop's refinement round: keywords regenerated after a
// failed cascade go straight to the same ranking the cascade uses).
func (e *Engine) AdmitByFields(fields []string, sources []source.Source, exclude map[string]bool, m int) []source.Source {
	if m <= 0 || len(fields) == 0 {
		return nil
	}
	var out []source.Source
	for _, sc := range rankSources(fields, sources) {
		if exclude[sc.src.ID] || sc.src.Status != source.StatusActive {
			continue
		}
		out = append(out, sc.src)
		if len(out) >= m {
			break
		}
	}
	return out
}

// WidenSources re-ranks the FULL corpus by the query's keyword cascade
// (primary → fallback → expander levels) and returns up to m active sources
// not in exclude, plus every field list the cascade tried (the refinement
// round's failure context) — the DEEP loop's mid-search file admission
// (Sirchmunk ReAct 对齐：探索回路可以扩大候选集，而不是在定死的集合里打转).
func (e *Engine) WidenSources(ctx context.Context, query string, sources []source.Source, exclude map[string]bool, m int) ([]source.Source, [][]string, error) {
	if m <= 0 {
		return nil, nil, nil
	}
	an := e.Analyzer
	if an == nil {
		an = RuleAnalyzer{}
	}
	a, err := an.Analyze(ctx, query)
	if err != nil {
		return nil, nil, err
	}
	var out []source.Source
	seen := map[string]bool{}
	add := func(fields []string) bool {
		for _, sc := range rankSources(fields, sources) {
			if exclude[sc.src.ID] || seen[sc.src.ID] || sc.src.Status != source.StatusActive {
				continue // cascade levels overlap: one file once (真机: 重复条文吃预算)
			}
			seen[sc.src.ID] = true
			out = append(out, sc.src)
			if len(out) >= m {
				return true
			}
		}
		return false
	}
	tried := [][]string{}
	candidates := [][]string{orderedKeys(a.Primary), orderedKeys(a.Fallback)}
	if e.Expander != nil {
		if levels, err := e.Expander.Expand(ctx, query, 2); err == nil {
			for _, lv := range levels {
				candidates = append(candidates, lv)
			}
		}
	}
	for _, fields := range candidates {
		if len(fields) == 0 {
			continue
		}
		tried = append(tried, fields)
		if add(fields) {
			return out, tried, nil
		}
	}
	return out, tried, nil
}

// render prefers the production Synthesizer; on refusal/error it degrades to
// the deterministic template, and the tell is readable from the summary: a
// template summary starting with 【DEEP/Fast 摘要】 is scaffolding, not an
// answer, so RefusedOf must report true for it. Without this, a refused LLM
// run leaks a template-answer past the persistence gate (真机抓到:
// 醉酒问题 LLM 拒答→模板代答→仍落簇).
func (e *Engine) render(ctx context.Context, query string, src source.Source, samples []mcs.Sample) string {
	if e.Synth != nil {
		if s, err := e.Synth.Synthesize(ctx, query, samples); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return synthesize(query, src, samples)
}

// templateDegraded reports whether s is the deterministic scaffold rather
// than a synthesizer answer. The templates carry "摘要】" in their header
// ("【摘要】" FAST, "【DEEP 摘要】" DEEP); a real synthesis never does.
func templateDegraded(s string) bool {
	return strings.HasPrefix(s, "【") && strings.Contains(s, "摘要】")
}

// RefusedOfSummary is the summary-side half of the refusal gate, ACTIVE ONLY
// when a production synthesizer is wired (Synth != nil): a deterministic
// template summary returned then means the synth refused/errored and the
// answer path papered over it (真机抓到：醉酒问题 LLM 拒答→模板代答→仍
// 落簇). Offline runs (nil Synth) legitimately build template answers for
// the gates, so the tell does not apply there.
func RefusedOfSummary(summary string, synth Synthesizer) bool {
	if synth == nil {
		return false
	}
	return templateDegraded(summary)
}

// RefusedOf reports whether the wired synthesizer refused its last run
// (cross-package: DEEP persists its answers through the same gate).
func RefusedOf(s Synthesizer) bool {
	if r, ok := s.(RefusalReporter); ok {
		return r.Refused()
	}
	return false
}

// Intent labels (fast_analyze contract).
const (
	IntentSearch     = "search"
	IntentChat       = "chat"
	IntentDocSummary = "doc_summary"
)

// RuleAnalyzer is the offline Analyzer: conservative intent heuristics plus
// mcs.Fields as the keyword cascade. Mechanism carrier, not a quality claim.
type RuleAnalyzer struct{}

var docVerbRe = regexp.MustCompile(`^(请|帮我|给我)?(总结|概括|通读|翻译)`)
var docScopeRe = regexp.MustCompile(`(全文|整篇|整份|这份|文档|一下|the whole)`)

func (RuleAnalyzer) Analyze(_ context.Context, query string) (Analysis, error) {
	q := strings.TrimSpace(strings.ToLower(query))
	if q != "" && len([]rune(q)) <= 8 {
		for _, w := range []string{"你好", "您好", "在吗", "hello", "hi", "thanks", "谢谢", "再见", "拜拜"} {
			if strings.Contains(q, w) {
				return Analysis{Intent: IntentChat}, nil
			}
		}
	}
	if len([]rune(strings.TrimSpace(query))) <= 16 &&
		docVerbRe.MatchString(q) && docScopeRe.MatchString(q) {
		return Analysis{Intent: IntentDocSummary}, nil
	}
	primary := map[string]float64{}
	for _, f := range mcs.Fields(query) {
		primary[f] = 0.5
	}
	return Analysis{Intent: IntentSearch, Primary: primary}, nil
}

var extRe = regexp.MustCompile(`\.(md|txt|pdf|docx|html|jsonl|json)$`)

// MatchFilename is the FILENAME_ONLY tier (D5 附档): name/extension lookups
// answer with 0 LLM calls and no sampling.
func MatchFilename(query string, sources []source.Source) (Answer, bool) {
	q := strings.TrimSpace(strings.Trim(query, "\"'`“”"))
	if q == "" || len(q) > 120 {
		return Answer{}, false
	}
	lowQ := strings.ToLower(q)
	var hits []source.Source
	if extRe.MatchString(lowQ) {
		for _, s := range sources {
			if s.Status != source.StatusActive {
				continue
			}
			blob := strings.ToLower(s.Title + " " + s.SourceURI + " " + s.BusinessKey)
			if strings.Contains(blob, lowQ) {
				hits = append(hits, s)
			}
		}
	} else if len([]rune(q)) <= 24 && !strings.ContainsAny(q, "吗多少什么如何怎么为什么?？") {
		for _, s := range sources {
			if s.Status != source.StatusActive {
				continue
			}
			if strings.EqualFold(s.Title, q) || strings.EqualFold(s.BusinessKey, q) {
				hits = append(hits, s)
			}
		}
	}
	if len(hits) == 0 {
		return Answer{}, false
	}
	var b strings.Builder
	b.WriteString("【文件匹配】")
	for i, s := range hits {
		title := s.Title
		if title == "" {
			title = s.ID
		}
		if i > 0 {
			b.WriteString("；")
		}
		b.WriteString(title)
	}
	return Answer{
		Query: query, Mode: ModeFilenameOnly, LLMCalls: 0,
		SourceID: hits[0].ID, Confidence: 1, Summary: b.String(),
	}, true
}

// rankFields ranks sources for one keyword level. Without UsePrior it is the
// plain TF-IDF-ish cascade. With UsePrior the prior REORDERS the cascade's own
// hits — it is a re-ranker over retrieved candidates, not a candidate
// generator.
//
// That distinction is the fix for a real bug: prior.Rank scores every ACTIVE
// source (its scan arm has a 0.3 floor plus 0.2 for being active), so it never
// returns an empty list. Feeding its output straight through meant
// `if len(out) > 0 { return out }` always fired — the plain cascade, and the
// documented "empty prior result falls back to the plain cascade", were
// unreachable, and a zero-signal query got an arbitrary pick instead of the
// honest skip.
func (e *Engine) rankFields(fields []string, sources []source.Source) []scored {
	plain := rankSources(fields, sources)
	if !e.UsePrior || len(fields) == 0 || len(plain) == 0 {
		return plain
	}
	// FUSE, do not replace. prior.Rank normalises its top file to 1.0 and drops
	// everything past its own topK, so sorting purely by prior score buries a
	// strong cascade hit the prior ranked low. Normalise the cascade's own
	// scores and combine, so the two signals must agree to move a file.
	priorScore := make(map[string]float64)
	for _, f := range prior.Rank(fields, sources, e.PriorHist, 0).Files {
		priorScore[f.SourceID] = f.Score
	}
	maxCascade := 0.0
	for _, sc := range plain {
		if sc.score > maxCascade {
			maxCascade = sc.score
		}
	}
	out := make([]scored, len(plain))
	copy(out, plain)
	for i := range out {
		norm := 0.0
		if maxCascade > 0 {
			norm = out[i].score / maxCascade
		}
		out[i].score = 0.5*norm + 0.5*priorScore[out[i].src.ID]
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

func sourceByID(sources []source.Source, id string) *source.Source {
	for i := range sources {
		if sources[i].ID == id {
			return &sources[i]
		}
	}
	return nil
}

type scored struct {
	src   source.Source
	score float64
}

// orderedKeys returns the cascade's field order: by IDF weight DESCENDING, then
// by name for a deterministic tie-break. The weights used to be dropped
// entirely (plain alphabetical order), which threw away the IDF channel the
// fast_analyze contract promises (§6.3: "意图 + 两级关键词 + IDF 权重") and
// made the two-level cascade order arbitrary in production.
func orderedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]] // highest IDF first
		}
		return out[i] < out[j] // deterministic cascade order
	})
	return out
}

func rankSources(fields []string, sources []source.Source) []scored {
	df := map[string]int{}
	n := 0
	for _, s := range sources {
		if s.Status != source.StatusActive {
			continue
		}
		n++
		low := strings.ToLower(s.Body)
		for _, f := range fields {
			if strings.Contains(low, strings.ToLower(f)) {
				df[f]++
			}
		}
	}
	var out []scored
	for _, s := range sources {
		if s.Status != source.StatusActive {
			continue
		}
		low := strings.ToLower(s.Body)
		sc := 0.0
		for _, f := range fields {
			tf := strings.Count(low, strings.ToLower(f))
			if tf == 0 {
				continue
			}
			idf := 1.0
			if df[f] > 0 && n > 0 {
				idf = 1.0 + math.Log2(float64(n)/float64(df[f]))
			}
			sc += idf * (1.0 + math.Log2(float64(tf)+1))
		}
		if sc > 0 {
			out = append(out, scored{src: s, score: sc})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

func synthesize(query string, src source.Source, samples []mcs.Sample) string {
	var b strings.Builder
	b.WriteString("【摘要】")
	b.WriteString(query)
	b.WriteString("\n【来源】")
	title := src.Title
	if title == "" {
		title = src.ID
	}
	b.WriteString(title)
	b.WriteString("\n")
	for i, sm := range samples {
		span := locate(src, sm.Start, sm.End)
		fmt.Fprintf(&b, "[%d] (%s %s [%d,%d)) %s\n", i+1, span, sm.Source, sm.Start, sm.End, trim(sm.Content, 240))
	}
	return b.String()
}

func locate(src source.Source, start, end int) string {
	for _, sp := range src.Structure {
		if sp.Start <= start && end <= sp.End {
			return sp.Label
		}
	}
	if len(src.Structure) > 0 {
		return src.Structure[0].Label
	}
	return "body"
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

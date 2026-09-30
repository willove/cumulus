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
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	// Bridged marks an answer whose evidence came from the session-context
	// fallback (the question's wording never appears in the statute's
	// windows, but the thread's earlier questions do). Escalating on a
	// bridged answer buys nothing — DEEP cannot find wording that is not
	// there — so the caller lets it stand at FAST.
	Bridged bool `json:"bridged"`
	Refused bool `json:"refused,omitempty"` // synthesis refused (insufficient evidence)
	// SynthDeferred marks an answer whose synthesis was deliberately skipped
	// because its confidence sat below the escalation line the caller wired
	// in (DeferBelow): a DEEP escalation re-synthesizes anyway, so the FAST
	// tier's render is a ~7k-token duplicate (stages_tokens, 2026-09-28: the
	// fast bucket held 14.6k of a 29.8k query, half of it this call). The
	// summary is empty until BackfillSynth runs — which the non-escalating
	// path owes the answer before it is served or persisted.
	SynthDeferred bool `json:"synth_deferred,omitempty"`
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
	// Usage carries query-conditioned document weights (the token×doc
	// affinity ledger + the session evidence stack), set per request before
	// Search. It fuses DIRECTLY into the cascade ranking: routing it through
	// the prior's history arm diluted it twice (arm weight 0.15, then the
	// 0.5 rank fusion) to ~4% of the final score — a whisper no candidate
	// move could hear. Capped by design so no single weight can dominate.
	Usage map[string]float64
	// SampleContext is session context (the thread's recent user questions)
	// used ONLY as a sampling fallback: when the raw query keeps no window
	// in the chosen document — the vocabulary-gap case ("养狗叫得太吵"
	// against a statute that says 饲养动物 — the document is right but the
	// wording never appears in its windows. Binary, not additive: the
	// primary pass working means the context never fires, so the common
	// path is untouched.
	SampleContext string
	// Verbose, when set, receives per-step diagnostics (serve -verbose /
	// CLUS_VERBOSE). nil → silent. It exists so a degraded step (a failed
	// expander, a refused synthesis) is visible instead of looking like a
	// clean miss.
	Verbose func(format string, a ...any)
	// Stages, when set, receives the wall time of each Search stage
	// ("analyze", "cascade", "sample", "synth") as it completes. Pure
	// observability — nil (the default, and every gate) behaves exactly as
	// before. It exists so the "where did the seconds go" question is
	// answered by data instead of anecdotes: the whole compression debate
	// (does a smaller synthesis input even pay?) is unanswerable without a
	// per-stage split. Wired to the monitor tracker by serve.
	Stages func(stage string, d time.Duration)
	// DeferBelow, when > 0, skips this engine's synthesis for answers whose
	// confidence sits below the line — the caller (the DEEP tier, which knows
	// the escalation threshold) wires it, and owes BackfillSynth on any
	// deferred answer it ends up serving anyway. 0 (the default, and every
	// gate) synthesizes exactly as before.
	DeferBelow float64
	// DeferThinCover, when set, additionally defers answers whose whole-query
	// coverage is under the line — armed by the DEEP tier only for K>1 fact
	// decompositions, where the measured escalation path is cover-incompleteness
	// rather than thin confidence (live: the two-question novel query deferred
	// nothing until this arm existed). A fact-complete answer that stands is
	// backfilled by the caller.
	DeferThinCover bool
	// SynthDelta, when set, receives the synthesis answer delta by delta so
	// the HTTP face can stream it to the browser (the user watches the
	// answer being written instead of waiting for the whole generation).
	// nil (the default, and every gate) uses the non-streaming Synthesizer.
	SynthDelta func(chunk string)
}

// StreamSynthesizer is the optional streaming half of Synthesizer: the same
// prompt and parse contract, delivered incrementally.
type StreamSynthesizer interface {
	SynthesizeStream(ctx context.Context, query string, samples []mcs.Sample, onDelta func(string)) (string, error)
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
	stage := func(name string, t0 time.Time) {
		if e.Stages != nil {
			e.Stages(name, time.Since(t0))
		}
	}
	t0 := time.Now()
	a, err := an.Analyze(ctx, query)
	stage("analyze", t0)
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
	t1 := time.Now()
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
	stage("cascade", t1)
	if len(ranked) == 0 {
		return Answer{Query: query, Mode: ModeFAST, LLMCalls: calls, Skipped: true}, nil
	}
	if ctx.Err() != nil {
		return Answer{}, ctx.Err()
	}
	best := ranked[0].src
	t2 := time.Now()
	samples, err := e.Sampler.SampleBody(ctx, query, best.Body)
	stage("sample", t2)
	if err != nil {
		return Answer{}, err
	}
	var kept []mcs.Sample
	total := 0
	primaryBest := 0.0
	for _, sm := range samples {
		if sm.Score > primaryBest {
			primaryBest = sm.Score
		}
		if sm.Score < 4 {
			continue
		}
		if total >= e.MaxChars {
			break
		}
		kept = append(kept, sm)
		total += len(sm.Content)
	}
	// 词汇鸿沟救援：主查询在非锚点文档只采到弱窗口（<7 分）或采空时，对会话
	// 权重前几的文档（本线程引用过的）逐一用「本问 + 会话近几问」重采，取最优。
	// 两道闸：① 最优窗口必须 ≥ bridgeFloorScore——会话里没有对的文档时（锚点
	// 带偏到错误法族），宁可交给正常检索/拒答，也不从错文档弱答；② 主查询在
	// 别的文档采到强窗口时不救（真换了话题或词面确实命中更好的文档）。
	sampleQuery := query
	bridged := false
	if strings.TrimSpace(e.SampleContext) != "" && (len(kept) == 0 || primaryBest < bridgeMinScore) {
		expanded := query + " " + e.SampleContext
		var bDoc source.Source
		var bKept []mcs.Sample
		bBest := 0.0
		for _, cand := range topUsageDocs(e.Usage, sources, bridgeCandidates) {
			s2, err2 := e.Sampler.SampleBody(ctx, expanded, cand.Body)
			if err2 != nil {
				continue
			}
			// ckept must not alias kept's backing array: the primary windows
			// stay live when the bridge fails below the floor, and a winning
			// candidate's windows must survive the next candidate's scan.
			var ckept []mcs.Sample
			ctotal, cBest := 0, 0.0
			for _, sm := range s2 {
				if sm.Score > cBest {
					cBest = sm.Score
				}
				if sm.Score < 4 || ctotal >= e.MaxChars {
					continue
				}
				ckept = append(ckept, sm)
				ctotal += len(sm.Content)
			}
			if cBest > bBest && len(ckept) > 0 {
				bDoc, bKept, bBest = cand, ckept, cBest
			}
		}
		if bBest >= bridgeFloorScore {
			best, kept, total = bDoc, bKept, 0
			for _, sm := range kept {
				total += len(sm.Content)
			}
			sampleQuery = expanded
			bridged = true
		}
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
	cov := mcs.Coverage(sampleQuery, kept)
	mean := 0.0
	for _, sm := range kept {
		mean += sm.Score
	}
	mean /= float64(len(kept))
	conf := mcs.Confidence(mean, cov)
	// Deferred synthesis (CLUS_FAST_DEFER_SYNTH, default off): when the
	// caller wired an escalation line and this answer sits under it — or the
	// caller armed the cover arm and the whole query is not lexically covered
	// — the render is skipped. Escalation is decided by confidence or fact
	// cover, both already in hand, and the DEEP tier synthesizes its own
	// answer over better evidence. Bridged answers never defer: the bridge
	// path stands on its own and must be served complete.
	coverThin := e.DeferThinCover && cov < 0.999
	if !bridged && (coverThin || (e.DeferBelow > 0 && conf < e.DeferBelow)) {
		return Answer{
			Query: query, Mode: ModeFAST, LLMCalls: calls,
			SourceID: best.ID, Samples: kept, Coverage: cov,
			Confidence: conf, Skipped: conf < SkipBelowLine(),
			SynthDeferred: true,
		}, nil
	}
	t3 := time.Now()
	summary := e.render(ctx, query, best, kept)
	stage("synth", t3)
	return Answer{
		Query:      query,
		Mode:       ModeFAST,
		LLMCalls:   calls + 1, // + the synthesis call
		SourceID:   best.ID,
		Samples:    kept,
		Coverage:   cov,
		Confidence: conf,
		Summary:    summary,
		// A bridged answer stands even under the confidence floor: the
		// session's own document carried it, and escalating cannot find
		// wording the statute does not contain.
		Skipped: conf < SkipBelowLine() && !bridged,
		Bridged: bridged,
		Refused: RefusedOf(e.Synth) || RefusedOfSummary(summary, e.Synth),
	}, nil
}

// usageShare is the usage arm's fusion share. 0.12 by default (down from the
// original 0.20): the 3-pass A/B showed even a CORRECT usage boost stretches
// the DEEP crawl by reordering candidates, so the arm is a tiebreaker, not a
// re-ranker. CLUS_USAGE_SHARE tunes it.
var usageShare = func() float64 { return envFloat("CLUS_USAGE_SHARE", 0.12) }

// bridgeMinScore is the primary-pass score under which the session bridge may
// take over: a weaker lexical hit than this in a non-anchor document is noise,
// not a topic change.
const bridgeMinScore = 7

// bridgeFloorScore is the score a bridged window must reach for the bridge to
// count. 4 (= the admission floor) is the default: any window that passes
// admission may bridge. Raising it filters wrong-anchor bridges (the session
// drifted to the wrong statute family) at the cost of more honest refusals —
// measured by the bench sweep (CLUS_BRIDGE_FLOOR), not guessed here.
var bridgeFloorScore = envFloat("CLUS_BRIDGE_FLOOR", 4)

// bridgeCandidates is how many session-weighted documents the bridge tries
// before settling for the best (sampling is in-memory scoring — the cost is
// milliseconds; only the synthesis is an LLM call).
const bridgeCandidates = 3

// envFloat reads a positive float env override with a default.
func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return def
}

// topUsageDocs returns the highest-weighted documents (weight descending),
// capped at n. Empty when nothing carries session/ledger weight.
func topUsageDocs(usage map[string]float64, sources []source.Source, n int) []source.Source {
	if len(usage) == 0 || n <= 0 {
		return nil
	}
	type ws struct {
		s source.Source
		w float64
	}
	all := make([]ws, 0, len(usage))
	for _, s := range sources {
		if w, ok := usage[s.ID]; ok && w > 0 {
			all = append(all, ws{s, w})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].w > all[j].w })
	if len(all) > n {
		all = all[:n]
	}
	out := make([]source.Source, 0, len(all))
	for _, e := range all {
		out = append(out, e.s)
	}
	return out
}

// maxUsageDoc returns the highest usage-weighted document id and its weight
// (the session anchor when one exists), or "" when nothing carries weight.
func maxUsageDoc(usage map[string]float64, sources []source.Source) (string, float64) {
	if len(usage) == 0 {
		return "", 0
	}
	best, bw := "", 0.0
	for _, s := range sources {
		if w, ok := usage[s.ID]; ok && w > bw {
			best, bw = s.ID, w
		}
	}
	return best, bw
}

// SkipBelow is the FAST confidence floor below which an answer is marked
// skipped. It mirrors deep.EscalateBelow so tuning one line cannot desynchronize
// the FAST skip flag from the DEEP escalation line.
const SkipBelow = 0.35

// SkipBelowLine is SkipBelow at runtime: the same CLUS_ESCALATE_BELOW knob
// that moves the DEEP escalation line moves this floor with it (the mirror
// is the whole point — a skipped answer is itself an escalation trigger).
func SkipBelowLine() float64 {
	v := strings.TrimSpace(os.Getenv("CLUS_ESCALATE_BELOW"))
	if v == "" {
		return SkipBelow
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || f > 0.95 {
		return SkipBelow
	}
	return f
}

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
// BackfillSynth renders the deferred synthesis into ans — the debt the
// non-escalating path owes a deferred answer before serving it. It mirrors
// Search's original tail: the same render, the same refusal re-derivation,
// the same LLM-call accounting.
func (e *Engine) BackfillSynth(ctx context.Context, ans *Answer, src source.Source) {
	if !ans.SynthDeferred {
		return
	}
	ans.Summary = e.render(ctx, ans.Query, src, ans.Samples)
	ans.SynthDeferred = false
	ans.LLMCalls++
	ans.Refused = RefusedOf(e.Synth) || RefusedOfSummary(ans.Summary, e.Synth)
}

func (e *Engine) render(ctx context.Context, query string, src source.Source, samples []mcs.Sample) string {
	if e.Synth != nil {
		if e.SynthDelta != nil {
			if ss, ok := e.Synth.(StreamSynthesizer); ok {
				if s, err := ss.SynthesizeStream(ctx, query, samples, e.SynthDelta); err == nil && strings.TrimSpace(s) != "" {
					return s
				}
				// The stream failed mid-way (deltas may already be on the
				// caller's screen): fall through to the non-streaming path
				// and let the caller reconcile the partial text.
			}
		}
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

var docVerbRe = regexp.MustCompile(`^(请|帮我|给我)?(总结|概括|通读|翻译)|^(please\s+)?(summarize|summarise|translate|recap)`)
var docScopeRe = regexp.MustCompile(`(全文|整篇|整份|这份|文档|一下|the whole|this (doc|document|file)|this)`)
var chatGreetWords = []string{"你好", "您好", "在吗", "hello", "hi", "thanks", "谢谢", "再见", "拜拜"}

// wordsOf splits on non-alphanumeric runes — Latin token matching for the
// gates below (substring matching a Latin word eats English questions:
// "hi" inside "which?" is not a greeting, and a question eaten here never
// reaches retrieval at all).
func wordsOf(q string) []string {
	return strings.FieldsFunc(q, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// LooksLikeChat is the deterministic chat gate: a SHORT greeting-shaped
// query. It backs the chat verdict of the production analyzer — see
// llm.AigateAnalyzer.Analyze, which re-verifies with the thinking pass
// when the fast no-think classify says chat but the query is not greeting
// shaped (a real question misread as chat never reaches retrieval at all,
// and costs the user a "（闲聊，不检索）" answer).
func LooksLikeChat(query string) bool {
	q := strings.TrimSpace(strings.ToLower(query))
	if q == "" || len([]rune(q)) > 8 {
		return false
	}
	words := wordsOf(q)
	for _, w := range chatGreetWords {
		// CJK greetings match as substrings (你好/在吗 are distinctive
		// sequences); Latin greetings match whole words only.
		if []rune(w)[0] < 0x80 {
			for _, tok := range words {
				if tok == w {
					return true
				}
			}
			continue
		}
		if strings.Contains(q, w) {
			return true
		}
	}
	return false
}

// LooksLikeDocSummary is the deterministic doc-summary gate: a short
// imperative asked against a whole-document scope. Same cross-check role
// as LooksLikeChat for the doc_summary verdict.
func LooksLikeDocSummary(query string) bool {
	q := strings.TrimSpace(strings.ToLower(query))
	return len([]rune(strings.TrimSpace(query))) <= 16 &&
		docVerbRe.MatchString(q) && docScopeRe.MatchString(q)
}

// questionShaped reports whether the query ASKS something rather than
// naming a document (bare noun phrases go to the FILENAME_ONLY tier).
// The character set covers CJK question characters; Latin questions are
// recognised by their wh-words/auxiliaries — without them an English
// "what is the max connection" (22 runes) looked like a filename lookup
// and never reached keyword retrieval.
func questionShaped(q string) bool {
	if strings.ContainsAny(q, "吗多少什么如何怎么为什么?？") {
		return true
	}
	for _, tok := range wordsOf(strings.ToLower(q)) {
		switch tok {
		case "what", "how", "why", "which", "when", "where", "who", "whom",
			"does", "do", "is", "are", "can", "could", "should", "would", "will":
			return true
		}
	}
	return false
}

func (RuleAnalyzer) Analyze(_ context.Context, query string) (Analysis, error) {
	if LooksLikeChat(query) {
		return Analysis{Intent: IntentChat}, nil
	}
	if LooksLikeDocSummary(query) {
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
	} else if len([]rune(q)) <= 24 && !questionShaped(q) {
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
	if len(fields) == 0 || len(plain) == 0 {
		return plain
	}
	// Usage weights fuse whether or not the prior runs (CLUS_PRIOR off must
	// not silence them); only the five-signal prior arm is opt-in.
	if !e.UsePrior && len(e.Usage) == 0 {
		return plain
	}
	// FUSE, do not replace. prior.Rank normalises its top file to 1.0 and drops
	// everything past its own topK, so sorting purely by prior score buries a
	// strong cascade hit the prior ranked low. Normalise the cascade's own
	// scores and combine, so the two signals must agree to move a file.
	priorScore := make(map[string]float64)
	if e.UsePrior {
		for _, f := range prior.Rank(fields, sources, e.PriorHist, 0).Files {
			priorScore[f.SourceID] = f.Score
		}
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
		// Three fused arms: the cascade's own score (normalized), the
		// five-signal prior, and the query-conditioned usage weights. The
		// 0.20 usage share is a nudge with a ceiling — it reshuffles near
		// ties and lifts remembered documents, it cannot outvote lexical.
		usage := 0.0
		if len(e.Usage) > 0 {
			usage = prior.Saturate(e.Usage[out[i].src.ID])
		}
		out[i].score = 0.45*norm + 0.35*priorScore[out[i].src.ID] + usageShare()*usage
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
		fmt.Fprintf(&b, "[%d] (%s %s [%d,%d)) %s\n", i+1, span, sm.Source, sm.Start, sm.End, trimAround(sm.Content, query, 240))
	}
	return b.String()
}

// trimAround renders the most query-relevant n-rune window of s. The
// small-file full-body path makes one sample span the whole document, so
// a fixed head trim (trim) would render the file's opening boilerplate
// and drop the answer wherever it actually sits — the offline gate's
// "FAST finds the known answer" assertion caught exactly that. The
// window is chosen deterministically by query-token hit count (ties go
// to the earliest window), and ellipses mark what was cut. The LLM
// synthesizer receives the untruncated sample; this only fixes how the
// deterministic template presents it.
func trimAround(s, query string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	toks := mcs.Fields(query)
	if len(toks) == 0 {
		return trim(s, n)
	}
	step := n / 4
	if step < 1 {
		step = 1
	}
	best, bestScore := 0, -1
	for start := 0; start+n <= len(r); start += step {
		score := 0
		win := string(r[start : start+n])
		for _, tk := range toks {
			if strings.Contains(win, tk) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = start, score
			if score == len(toks) {
				break
			}
		}
	}
	out := string(r[best : best+n])
	if best > 0 {
		out = "…" + out
	}
	if best+n < len(r) {
		out += "…"
	}
	return out
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

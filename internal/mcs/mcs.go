// Package mcs is Monte Carlo evidence sampling: treat extraction as a sampling
// problem over the source body, not a chunking problem. Stage 1 stratified +
// fuzzy anchors (explore), stage 2 gaussian around high-score seeds (exploit),
// stage 3 top-k synthesis. Scoring is pluggable — offline stub for gates,
// aigate-backed scorer in production.
package mcs

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Sample is one candidate window of a source body.
type Sample struct {
	Start     int      `json:"start"`
	End       int      `json:"end"`
	Content   string   `json:"content"`
	Source    string   `json:"source"`
	Arm       string   `json:"arm,omitempty"`     // lex | local | global
	Covers    []string `json:"covers,omitempty"`  // fact ids this window supports
	Conflicts []string `json:"conflicts,omitempty"` // evidence ids this window contradicts (v3b c_d)
	Score     float64  `json:"score"`
	Reasoning string   `json:"reasoning"`
}

// ScoreFailed marks an observation failure (LLM error / unparseable) — it must
// never be conflated with "judged irrelevant" (score 0). Failed samples are
// excluded from kept/covers/confidence (ir-rag A3).
const ScoreFailed = -1.0

// Failed reports whether the sample's score is an observation failure.
func (s Sample) Failed() bool { return s.Score < 0 }

// Scorer rates one sample against the query (0-10 scale).
type Scorer interface {
	Score(ctx context.Context, query string, s Sample) (score float64, reasoning string, err error)
}

// FactAware is an optional Scorer extension (oracle vector): the
// scorer sees the query's atomic fact ids and reports which ones the window
// directly supports — one scoring call updates every fact's coverage, so the
// call count does not grow with K.
type FactAware interface {
	ScoreWithFacts(ctx context.Context, query string, facts []string, s Sample) (score float64, reasoning string, covers []string, err error)
}

// BatchScorer is an optional Scorer extension (Jev-Mem v3a): score every
// window of a round in ONE model call. len(results) must equal len(samples)
// and results[i] belongs to samples[i]; a whole-batch failure returns err
// (the sampler then reports every window failed — the same honest
// total-failure contract the per-window path has).
type BatchScorer interface {
	ScoreBatch(ctx context.Context, query string, facts []string, samples []Sample) ([]BatchResult, error)
}

// BatchResult gains Conflicts on the v3b path: the ids ("K1".. digest
// entries, "S1".. batch peers) this window directly contradicts. Novelty and
// Support are parsed for schema honesty but deliberately NOT carried onto
// Sample — nothing consumes them yet, and telemetry-only fields on a struct
// every stage copies would be dead weight until a mechanism earns them.
type BatchResult struct {
	Score     float64
	Reasoning string
	Covers    []string
	Conflicts []string
}

// ConflictBatchScorer is the v3b extension of BatchScorer (Jev-Mem c_d): the
// batched call ALSO sees a digest of the evidence already kept this query, so
// contradiction marks are cross-file, not just within one batch. Results
// carry Conflicts the caller may gate stopping on.
type ConflictBatchScorer interface {
	ScoreBatchConflict(ctx context.Context, query string, facts []string, samples, prior []Sample) ([]BatchResult, error)
}

// ScorerBatch reports whether the batched scoring path is enabled. Opt-in:
// batching changes the prompt shape the model sees (N windows in one call),
// so it earns its default through a paired A/B like every other behavioural
// knob (scripts/paired-ab.sh CLUS_MCS_SCORER_BATCH 0 1 scorebatch).
func ScorerBatch() bool {
	n, _ := envInt("CLUS_MCS_SCORER_BATCH")
	return n == 1
}

// ScorerConflict reports whether the conflict-aware batched path (v3b) is
// enabled. It implies the batched path; on its own (batch off) it is inert —
// the per-window call shape cannot see cross-window contradiction.
func ScorerConflict() bool {
	n, _ := envInt("CLUS_SCORER_CONFLICT")
	return n == 1
}

// KeywordScorer is a deterministic offline stub: score = keyword hit density
// scaled to 0-10. Good enough for localization gates; not a quality claim.
type KeywordScorer struct {
	Keywords []string
}

func (k KeywordScorer) Score(_ context.Context, query string, s Sample) (float64, string, error) {
	kws := k.Keywords
	if len(kws) == 0 {
		kws = Fields(query)
	}
	if len(kws) == 0 {
		return 0, "no keywords", nil
	}
	hits := 0
	low := strings.ToLower(s.Content)
	for _, w := range kws {
		if w != "" && strings.Contains(low, strings.ToLower(w)) {
			hits++
		}
	}
	if hits == 0 {
		return 0, "no keyword overlap", nil
	}
	density := float64(hits) / float64(len(kws))
	// Monotone in density: 1→5 on [0,0.4), 5→8 on [0.4,0.8), 8→8.4 on
	// [0.8,1]. The mid band used to rise to ~14 and drop to 8 at the 0.8
	// break — better coverage scored WORSE (4/5 keywords < 3/5 keywords).
	switch {
	case density >= 0.8:
		return 8 + 2*density - 1.6, "strong keyword coverage", nil
	case density >= 0.4:
		return 5 + 3*(density-0.4)/0.4, "partial keyword coverage", nil
	default:
		return 1 + 10*density, "weak keyword coverage", nil
	}
}

// Fields is a crude CJK/latin tokenizer for the offline stub.
func Fields(text string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) >= 2 {
			out = append(out, strings.ToLower(string(cur)))
		}
		cur = cur[:0]
	}
	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fff {
			cur = append(cur, r)
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	var expanded []string
	for _, f := range out {
		runes := []rune(f)
		if len(runes) >= 2 && isCJK(runes[0]) {
			for i := 0; i+1 < len(runes); i++ {
				expanded = append(expanded, string(runes[i:i+2]))
			}
		} else {
			expanded = append(expanded, f)
		}
	}
	return expanded
}

func isCJK(r rune) bool { return r >= 0x4e00 && r <= 0x9fff }

// Config tunes the sampler.
type Config struct {
	Window          int
	SamplesPerRound int
	Rounds          int
	TopSeeds        int
	Sigma           float64
	SmallFileRunes  int
	MaxEvidence     int
}

func DefaultConfig() Config {
	return Config{
		Window:          240,
		SamplesPerRound: 5,
		Rounds:          2,
		TopSeeds:        3,
		Sigma:           180,
		// Whole-file shortcut threshold (runes). 100_000 aligns with the
		// reference implementation's _FAST_SMALL_FILE_THRESHOLD = 100_000
		// (sirchmunk src/sirchmunk/search.py:1541): a body under 100K chars
		// is read in full instead of Monte-Carlo windowed sampling. On the
		// production corpus (Chinese law, avg ~31.5 KB ≈ 10K runes) this is
		// what keeps a FAST query at ONE evidence-scoring call instead of
		// ~20 — the reference's whole FAST path is 2 LLM calls total
		// (analyze + synthesize), and per-window scoring was the gap.
		// Tradeoff, stated plainly: the in-file localisation the gaussian
		// arm provides is skipped for small bodies — the evidence is the
		// whole body (or its densest budget window), not windows chosen
		// by seed refinement — and the confidence gate plus DEEP
		// re-sampling remain the backstop for a body whose answer the
		// budget window missed. Still overridable via CLUS_MCS_SMALL_FILE.
		SmallFileRunes: 100_000,
		MaxEvidence:    15000,
	}
}

// EnvConfig is DefaultConfig with operator overrides from the environment.
// SSOT §3.3: "参数：samples_per_round、top_seeds、sigma、小文档阈值——配置驱动，
// 进 scenario". Before this, DefaultConfig() was the ONLY source, so a knob the
// design promises could not actually be turned. Every override is optional and
// must be positive; anything unparseable is ignored, so an unconfigured
// process behaves exactly as before.
//
//	CLUS_MCS_WINDOW            window half-width in runes
//	CLUS_MCS_SAMPLES_PER_ROUND slots per proposal round
//	CLUS_MCS_ROUNDS            proposal rounds
//	CLUS_MCS_TOP_SEEDS         seeds carried into stage 2
//	CLUS_MCS_SIGMA             gaussian jitter in runes
//	CLUS_MCS_SMALL_FILE        rune threshold for the whole-file shortcut
//	CLUS_MCS_MAX_EVIDENCE      rune budget over kept windows
func EnvConfig() Config {
	c := DefaultConfig()
	if n, ok := envInt("CLUS_MCS_WINDOW"); ok {
		c.Window = n
	}
	if n, ok := envInt("CLUS_MCS_SAMPLES_PER_ROUND"); ok {
		c.SamplesPerRound = n
	}
	if n, ok := envInt("CLUS_MCS_ROUNDS"); ok {
		c.Rounds = n
	}
	if n, ok := envInt("CLUS_MCS_TOP_SEEDS"); ok {
		c.TopSeeds = n
	}
	if f, ok := envFloat("CLUS_MCS_SIGMA"); ok {
		c.Sigma = f
	}
	if n, ok := envInt("CLUS_MCS_SMALL_FILE"); ok {
		c.SmallFileRunes = n
	}
	if n, ok := envInt("CLUS_MCS_MAX_EVIDENCE"); ok {
		c.MaxEvidence = n
	}
	if c.Window <= 0 {
		c = DefaultConfig() // an unusable window would make every probe empty
	}
	return c
}

func envFloat(key string) (float64, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return f, true
}

func envInt(key string) (int, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// ScorerWorkers is how many windows one scoring round may have in flight.
//
// Default 1 = the historical serial loop, byte-for-byte. Concurrency is
// opt-in (perf-plan §2 P0-2) because a cold query issues ~10 blocking
// evaluate_sample round-trips back to back and serialising them was the
// single largest term in the observed 40s+ cold latency.
//
// The cap is deliberately conservative: an unthrottled fan-out turns a rate
// limit into a retry storm, and the retry storm is worse than the serial wait
// (same failure mode as the embedder's CLUS_EMBED_WORKERS, see minilm/forward.go).
func ScorerWorkers() int {
	if n, ok := envInt("CLUS_MCS_SCORER_WORKERS"); ok {
		return n
	}
	return 1
}

// Sampler runs the three-stage loop.
type Sampler struct {
	Cfg    Config
	Scorer Scorer
	RNG    *rand.Rand
	// FactHints, when set (as "f1:描述" strings), switches scoring to the
	// FactAware oracle path; nil keeps the plain scorer.
	FactHints []string
	// Weights are the arm weights after the last SampleBody run (λ_t;
	// visible for gates and probes).
	Weights map[string]float64
	// ExploreBoost amplifies the global (semantic-blind-spot) arm's share
	// while the caller still has open coverage gaps (ir-rag 1.7: LENS
	// information-directed λ weighting). 0 or 1 = normal arm economics.
	// It is advisory: every arm keeps its one-slot floor.
	ExploreBoost float64
	// Prior is the evidence already kept this query, handed to a
	// ConflictBatchScorer as the Current Evidence Digest (v3b c_d): the
	// scorer marks which of these the round's windows contradict. The
	// sampler itself never mutates it — the DEEP loop owns the kept set and
	// refreshes this before each file.
	Prior []Sample
}

// Arms are the complementary proposal families: lex anchors on the
// query's own words, local neighborhoods (stratified/gaussian), global
// uniform scatter over the whole body — the semantic blind-spot arm.
var Arms = []string{"lex", "local", "global"}

func New(cfg Config, scorer Scorer) *Sampler {
	if cfg.Window <= 0 {
		cfg = DefaultConfig()
	}
	return &Sampler{Cfg: cfg, Scorer: scorer, RNG: rand.New(rand.NewSource(42))}
}

// SampleBody extracts scored windows from body.
func (s *Sampler) SampleBody(ctx context.Context, query, body string) ([]Sample, error) {
	runes := []rune(body)
	if len(runes) == 0 {
		return nil, nil
	}
	if len(runes) <= s.Cfg.SmallFileRunes {
		// The whole body is the evidence, but the budget still binds:
		// cap at MaxEvidence runes exactly like the sampling path's kept
		// list, so a 100K-rune body cannot hand the synthesizer an
		// unbounded prompt. This mirrors the reference implementation
		// bounding evidence to _FAST_MAX_EVIDENCE_CHARS (sirchmunk
		// search.py:1755/1540). The budget window is centred on the
		// query's densest hit region rather than blindly at the head —
		// the reference centres its non-small-file windows on grep hits
		// the same way (search.py:2088 _read_context_windows), and a head
		// cut drops answers that sit past the budget (the gate's
		// "known answer" fixture has its answer at ~16K runes). Start/End
		// stay rune-exact against the live body so citation back-tracking
		// and the warm-prior validation keep holding.
		start, end := 0, len(runes)
		if s.Cfg.MaxEvidence > 0 && end-start > s.Cfg.MaxEvidence {
			start = densestWindow(string(runes), query, s.Cfg.MaxEvidence)
			end = start + s.Cfg.MaxEvidence
		}
		sm := Sample{Start: start, End: end, Content: string(runes[start:end]), Source: "full", Arm: "local"}
		evs, err := s.evalAll(ctx, query, []Sample{sm})
		if err != nil {
			return nil, err
		}
		return evs, nil
	}

	// Round 0: full sweep — stratified grid + query anchors. The spread arm
	// is what keeps anchors from being the only entrance.
	evaluated, err := s.evalAll(ctx, query, s.stage1(runes, query, body))
	if err != nil {
		return nil, err
	}
	seeds := topSeeds(evaluated, s.Cfg.TopSeeds)

	// Rounds 1..N: arm-proportional proposals. λ starts equal and follows
	// each arm's yield of scoreable windows (online weights); every arm
	// keeps at least one slot — weights steer, they never silence.
	λ := equalWeights()
	for r := 0; r < s.Cfg.Rounds; r++ {
		batch := s.allocate(runes, query, seeds, λ)
		if len(batch) == 0 {
			continue
		}
		ev, err := s.evalAll(ctx, query, batch)
		if err != nil {
			return nil, err
		}
		evaluated = append(evaluated, ev...)
		λ = updateLambda(λ, ev)
		seeds = topSeeds(evaluated, s.Cfg.TopSeeds)
	}
	s.Weights = λ

	evaluated = dedup(evaluated)
	sort.Slice(evaluated, func(i, j int) bool {
		if evaluated[i].Score == evaluated[j].Score {
			return evaluated[i].Start < evaluated[j].Start
		}
		return evaluated[i].Score > evaluated[j].Score
	})
	out := make([]Sample, 0, len(evaluated))
	total := 0
	for _, sm := range evaluated {
		if total >= s.Cfg.MaxEvidence {
			break
		}
		out = append(out, sm)
		total += len(sm.Content)
	}
	return out, nil
}

// armBudget splits one round's k slots across the arms by λ, then applies
// ExploreBoost to the global arm. Floors are respected: every arm keeps at
// least one slot, and the total never exceeds k.
func (s *Sampler) armBudget(λ map[string]float64) map[string]int {
	k := s.Cfg.SamplesPerRound
	if k < len(Arms) {
		k = len(Arms)
	}
	budget := map[string]int{}
	for _, arm := range Arms {
		budget[arm] = 1
	}
	rest := k - len(Arms)
	for _, arm := range armsByWeight(λ) {
		if rest <= 0 {
			break
		}
		budget[arm]++
		rest--
	}
	applyExploreBoost(budget, k, s.ExploreBoost)
	return budget
}

// armsByWeight orders arm names by descending weight.
func armsByWeightDesc(λ map[string]float64) []string {
	out := append([]string(nil), Arms...)
	sort.Slice(out, func(i, j int) bool { return λ[out[i]] > λ[out[j]] })
	return out
}

// applyExploreBoost moves up to (boost-1)×(movable slots) slots into global.
// Movable slots are those above the per-arm floor of 1.
func applyExploreBoost(budget map[string]int, k int, boost float64) {
	if boost <= 1 {
		return
	}
	movable := 0
	for _, arm := range Arms {
		movable += budget[arm] - 1
	}
	if movable <= 0 {
		return
	}
	extra := int(float64(movable) * (boost - 1))
	if extra > movable {
		extra = movable
	}
	// Take from the heaviest non-global arms first (stable desc weight).
	for _, arm := range armsByWeightDesc(map[string]float64{
		"lex":    float64(budget["lex"]),
		"local":  float64(budget["local"]),
		"global": float64(budget["global"]),
	}) {
		if extra <= 0 {
			break
		}
		if arm == "global" {
			continue
		}
		for extra > 0 && budget[arm] > 1 {
			budget[arm]--
			budget["global"]++
			extra--
		}
	}
	// The moves above are zero-sum by construction (a slot taken from one arm is
	// given to another, and `extra` is bounded by the movable count), so the
	// total never leaves k — a re-normalising "sum guard" here would be dead
	// code that implies an invariant the code does not actually maintain.
}

// allocate splits one round's budget across the three arms (ExploreBoost
// aware) and proposes the windows.
func (s *Sampler) allocate(runes []rune, query string, seeds []Sample, λ map[string]float64) []Sample {
	budget := s.armBudget(λ)
	var out []Sample
	out = append(out, s.fuzzWindows(runes, query, budget["lex"])...)
	out = append(out, s.gaussian(runes, seeds, budget["local"])...)
	out = append(out, s.globalScatter(runes, budget["global"])...)
	return out
}

// updateLambda nudges the arm weights toward this round's yield (online
// weights): utility = each arm's share of scoreable windows, EMA 0.5,
// renormalized to sum 1.
func updateLambda(λ map[string]float64, batch []Sample) map[string]float64 {
	contrib := map[string]float64{}
	total := 0.0
	for _, sm := range batch {
		if sm.Score >= 4 {
			contrib[sm.Arm]++
			total++
		}
	}
	next := map[string]float64{}
	sum := 0.0
	for _, arm := range Arms {
		u := 0.0
		if total > 0 {
			u = contrib[arm] / total
		}
		next[arm] = 0.5*λ[arm] + 0.5*u
		sum += next[arm]
	}
	if sum <= 0 {
		return equalWeights()
	}
	for _, arm := range Arms {
		next[arm] /= sum
	}
	return next
}

func equalWeights() map[string]float64 {
	λ := map[string]float64{}
	for _, arm := range Arms {
		λ[arm] = 1.0 / float64(len(Arms))
	}
	return λ
}

func armsByWeight(λ map[string]float64) []string {
	sorted := append([]string(nil), Arms...)
	sort.Slice(sorted, func(i, j int) bool { return λ[sorted[i]] > λ[sorted[j]] })
	return sorted
}

// HasAnyToken reports whether body contains at least one of toks (plain
// substring match, the same primitive Fields-based scoring already uses).
// The DEEP admission pre-filter uses it to skip the LLM scorer call for
// files with zero lexical overlap with the query — those calls returned 0
// scores on every measured widen pass.
func HasAnyToken(body string, toks []string) bool {
	if len(toks) == 0 {
		return true // no tokens to judge by: keep the file (not filterable)
	}
	low := strings.ToLower(body)
	for _, t := range toks {
		if t != "" && strings.Contains(low, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

// densestWindow returns the rune offset of the n-rune window of body with
// the most distinct query tokens in it (ties → the earliest window). It is
// the deterministic locator behind the small-file full-body path's evidence
// budget: the whole document is the evidence, but when it exceeds the
// budget the view kept is centred on the query rather than at the head.
// Mirrors the reference implementation centring evidence windows on grep
// hits (sirchmunk search.py:2088). O(windows × tokens) with a quarter-width
// stride, so a 100K-rune body stays well under a millisecond.
func densestWindow(body, query string, n int) int {
	r := []rune(body)
	if len(r) <= n {
		return 0
	}
	toks := Fields(query)
	if len(toks) == 0 {
		return 0
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
	return best
}

func (s *Sampler) stage1(runes []rune, query, body string) []Sample {
	n := len(runes)
	half := s.Cfg.Window
	var out []Sample
	k := s.Cfg.SamplesPerRound
	for i := 0; i < k; i++ {
		center := (2*i + 1) * n / (2 * k)
		out = append(out, window(runes, center, half, "stratified"))
	}
	out = append(out, s.fuzzWindows(runes, query, k)...)
	return out
}

// fuzzWindows centers windows on the query's own words found in the body
// (lex arm). runeIndexOf lands on the FIRST occurrence — dense distractor
// padding pins these windows early, which is exactly the blind the global
// arm covers (LENS: file-level hit ≠ in-file position).
func (s *Sampler) fuzzWindows(runes []rune, query string, limit int) []Sample {
	if limit <= 0 {
		return nil
	}
	half := s.Cfg.Window
	var out []Sample
	for _, w := range Fields(query) {
		if len(out) >= limit {
			break
		}
		if !strings.Contains(strings.ToLower(string(runes)), w) {
			continue
		}
		center := runeIndexOf(runes, w)
		out = append(out, window(runes, center, half, "fuzz"))
	}
	return out
}

func (s *Sampler) gaussian(runes []rune, seeds []Sample, limit int) []Sample {
	if limit <= 0 || len(seeds) == 0 {
		return nil
	}
	var out []Sample
	for i := 0; i < limit; i++ {
		seed := seeds[i%len(seeds)]
		center := (seed.Start + seed.End) / 2
		jitter := int(s.RNG.NormFloat64() * s.Cfg.Sigma)
		out = append(out, window(runes, center+jitter, s.Cfg.Window, "gaussian"))
	}
	return out
}

// globalScatter drops uniform-random windows over the whole body (global
// arm, LENS global proposals): the semantic blind-spot arm that ignores both
// anchors and seeds. RNG is seeded in New, so draws stay reproducible for
// gates.
func (s *Sampler) globalScatter(runes []rune, limit int) []Sample {
	if limit <= 0 {
		return nil
	}
	n := len(runes)
	var out []Sample
	for i := 0; i < limit; i++ {
		out = append(out, window(runes, s.RNG.Intn(n), s.Cfg.Window, "global"))
	}
	return out
}

// evalAll scores every window in a round.
//
// Ordering contract: the returned slice is index-aligned with `in`, so the
// result is byte-identical to the historical serial loop regardless of how
// many scorers ran concurrently. That is what lets ScorerWorkers() stay
// opt-in: turning concurrency on must not move a single byte of gate output.
//
// Concurrency note: a Scorer implementation must be safe for concurrent use
// when ScorerWorkers() > 1. The production scorers are (llm.ChatClient keeps
// only an atomic token counter; KeywordScorer is stateless). A stub used by a
// gate that flips the cap is responsible for its own safety.
//
// `lastErr` is resolved by index, not by completion order, so the aggregate
// "every window failed" error names the same window the serial loop named.
func (s *Sampler) evalAll(ctx context.Context, query string, in []Sample) ([]Sample, error) {
	fa, _ := s.Scorer.(FactAware)
	out := make([]Sample, len(in))
	errs := make([]error, len(in))

	// Batched path (v3a, CLUS_MCS_SCORER_BATCH=1): one call for the whole
	// round instead of one per window. On a batch error every window is an
	// observation failure — falling through to the per-window path would
	// spend N more calls on a scorer that just failed, and silently
	// degrading to unbatched would hide the flag's failure mode from the
	// A/B that gates it.
	//
	// v3b (CLUS_SCORER_CONFLICT=1, implies batch): the conflict-aware call
	// also receives the Prior digest, and its Conflicts marks land on the
	// samples for the caller's stop gate. Selection is highest-first so the
	// flag pair (batch=1, conflict=0) is exactly v3a.
	if len(in) > 1 {
		if cs, ok := s.Scorer.(ConflictBatchScorer); ok && ScorerConflict() {
			res, err := cs.ScoreBatchConflict(ctx, query, s.FactHints, in, s.Prior)
			if err == nil {
				for i := range in {
					sm := in[i]
					sm.Score, sm.Reasoning, sm.Covers, sm.Conflicts = res[i].Score, res[i].Reasoning, res[i].Covers, res[i].Conflicts
					out[i] = sm
				}
				return out, nil
			}
			for i := range in {
				sm := in[i]
				sm.Score, sm.Reasoning, sm.Covers = ScoreFailed, "scorer error: "+err.Error(), nil
				out[i] = sm
				errs[i] = err
			}
			return out, s.batchFailure(in, errs)
		}
		if bs, ok := s.Scorer.(BatchScorer); ok && ScorerBatch() {
			res, err := bs.ScoreBatch(ctx, query, s.FactHints, in)
			if err != nil {
				for i := range in {
					sm := in[i]
					sm.Score, sm.Reasoning, sm.Covers = ScoreFailed, "scorer error: "+err.Error(), nil
					out[i] = sm
					errs[i] = err
				}
				return out, s.batchFailure(in, errs)
			}
			for i := range in {
				sm := in[i]
				sm.Score, sm.Reasoning, sm.Covers = res[i].Score, res[i].Reasoning, res[i].Covers
				out[i] = sm
			}
			return out, nil
		}
	}

	scoreOne := func(i int) {
		sm := in[i]
		var err error
		if fa != nil && len(s.FactHints) > 0 {
			var sc float64
			var why string
			var covers []string
			sc, why, covers, err = fa.ScoreWithFacts(ctx, query, s.FactHints, sm)
			if err != nil {
				// A3: observation failure ≠ judged irrelevant.
				sm.Score, sm.Reasoning, sm.Covers = ScoreFailed, "scorer error: "+err.Error(), nil
			} else {
				sm.Score, sm.Reasoning, sm.Covers = sc, why, covers
			}
		} else {
			var sc float64
			var why string
			sc, why, err = s.Scorer.Score(ctx, query, sm)
			if err != nil {
				sm.Score, sm.Reasoning = ScoreFailed, "scorer error: "+err.Error()
			} else {
				sm.Score, sm.Reasoning = sc, why
			}
		}
		errs[i] = err
		out[i] = sm
	}

	if w := ScorerWorkers(); w > 1 && len(in) > 1 {
		sem := make(chan struct{}, w)
		var wg sync.WaitGroup
		for i := range in {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				scoreOne(i)
			}(i)
		}
		wg.Wait()
	} else {
		for i := range in {
			scoreOne(i)
		}
	}

	// Counted after the join, never inside: a shared counter written from the
	// workers is a data race the -race gate catches immediately.
	// Total failure must not dress up as "no evidence": when every window's
	// scoring failed the scorer itself is down (network, auth, quota), and the
	// honest result is an error the caller can surface — not an all-
	// ScoreFailed page that reads downstream as 证据不足/insufficient and
	// poisons refusals and eval numbers alike.
	return out, s.batchFailure(in, errs)
}

// batchFailure reports the honest total-failure error when EVERY window in
// the batch failed to score (both the per-window and the batched path feed
// it the same per-window error slice). nil when at least one window scored.
func (s *Sampler) batchFailure(in []Sample, errs []error) error {
	failed := 0
	var lastErr error
	for i := len(errs) - 1; i >= 0; i-- {
		if errs[i] != nil {
			failed++
			if lastErr == nil {
				lastErr = errs[i] // highest index wins, as the serial loop did
			}
		}
	}
	if len(in) > 0 && failed == len(in) {
		return fmt.Errorf("mcs: scorer failed for all %d windows: %w", len(in), lastErr)
	}
	return nil
}

func window(runes []rune, center, half int, source string) Sample {
	n := len(runes)
	// CLAMP the CENTER into the body first, then clamp the span. A jittered
	// center past either end used to be handled by widening the window to the
	// WHOLE body (start > end → [0,n)), so one "sample" could silently become
	// the entire document — blowing the MaxEvidence budget and dominating the
	// score ranking. Clamping only the span instead gives an empty window and
	// loses the sample; clamping the center keeps a half-width sliver at the
	// edge, which is what the jitter intended.
	if center < 0 {
		center = 0
	}
	if center > n {
		center = n
	}
	start := center - half
	if start < 0 {
		start = 0
	}
	end := center + half
	if end > n {
		end = n
	}
	if end < start {
		end = start
	}
	arm := "local"
	switch source {
	case "fuzz":
		arm = "lex"
	case "global":
		arm = "global"
	}
	return Sample{
		Start:   start,
		End:     end,
		Content: string(runes[start:end]),
		Source:  source,
		Arm:     arm,
	}
}

func topSeeds(in []Sample, k int) []Sample {
	cp := append([]Sample(nil), in...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Score > cp[j].Score })
	if len(cp) > k {
		cp = cp[:k]
	}
	out := cp[:0]
	for _, sm := range cp {
		if sm.Score >= 4 {
			out = append(out, sm)
		}
	}
	return out
}

func dedup(in []Sample) []Sample {
	seen := map[int]bool{}
	out := in[:0]
	for _, sm := range in {
		if seen[sm.Start] {
			continue
		}
		seen[sm.Start] = true
		out = append(out, sm)
	}
	return out
}

func runeIndexOf(runes []rune, word string) int {
	w := []rune(word)
	if len(w) == 0 {
		return 0
	}
	for i := 0; i+len(w) <= len(runes); i++ {
		match := true
		for j := 0; j < len(w); j++ {
			if runes[i+j] != w[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return len(runes) / 2
}

// Coverage reports how much of the query's fields appear in the top samples.
func Coverage(query string, top []Sample) float64 {
	kws := Fields(query)
	if len(kws) == 0 {
		return 0
	}
	joined := ""
	for _, sm := range top {
		joined += strings.ToLower(sm.Content) + "\n"
	}
	hit := 0
	for _, w := range kws {
		if strings.Contains(joined, strings.ToLower(w)) {
			hit++
		}
	}
	return float64(hit) / float64(len(kws))
}

// Confidence maps mean score and coverage into [0,1] (deterministic).
func Confidence(meanScore, coverage float64) float64 {
	c := 0.5*(meanScore/10.0) + 0.5*coverage
	if c < 0 {
		return 0
	}
	if c > 1 {
		return 1
	}
	return math.Round(c*1000) / 1000
}

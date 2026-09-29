// Package facts implements per-fact evidence targets: decompose a
// query into atomic requirements D_req = {f1..fK}, track per-fact coverage
// from a shared sample pool, and report the weakest-requirement stop signal
// (weakest-requirement stop signal). Offline decomposition is a deterministic heuristic so gates stay
// stable; production may replace Build with an aigate analyzer that returns
// the same []Fact shape.
package facts

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/willove/cumulus/internal/mcs"
)

// Fact is one atomic evidence requirement (LENS Definition 2).
type Fact struct {
	ID      string  `json:"id"`
	Query   string  `json:"query"`
	Covered bool    `json:"covered"`
	Score   float64 `json:"score"`
	// NearMiss is the best support an UNCOVERED fact came close to: the highest
	// keyword-hit ratio seen on any scoreable window even though it stayed
	// below CoverHit (lexical path), or the best sub-threshold sample score
	// (oracle path). It is the weakness signal the weakest-requirement stop
	// actually needs — without it every uncovered fact scores 0 and "weakest
	// first" degenerates into declaration order.
	NearMiss float64     `json:"near_miss,omitempty"`
	SourceID string      `json:"source_id,omitempty"`
	Window   *mcs.Sample `json:"window,omitempty"`
}

// Report is the multi-hop coverage state (weakest-requirement stop signal).
type Report struct {
	Facts    []Fact   `json:"facts"`
	Complete bool     `json:"complete"`
	Missing  []string `json:"missing"`
	Weakest  float64  `json:"weakest"`
	K        int      `json:"k"`
}

// CoverScore is the per-fact hit line on the offline 0-10 sample scale.
const CoverScore = 4.0

// CoverHit is the minimum keyword-overlap share for a window to support a fact.
const CoverHit = 0.5

// Decomposer produces the atomic evidence requirements for a query.
//
// The offline heuristic (Build) is the default and every gate's baseline; a
// production LLM decomposer must return the SAME []string shape so the
// downstream cover/weakest-requirement machinery is unchanged (perf-plan §6).
type Decomposer interface {
	Decompose(ctx context.Context, query string) ([]string, error)
}

// minUnitRunes is the shortest a requirement may be. The measured failure
// ("参与违法怎么办" → ["参", …]) produced one-rune facts whose keyword set
// could never be covered, so the DEEP loop burned its whole budget on a
// phantom requirement.
const minUnitRunes = 4

// maxParts caps the requirement count. splitQuery already truncates to 4;
// the LLM path gets the same ceiling so the two arms stay comparable.
const maxParts = 4

// boundaryRunes are the quote/bracket characters whose imbalance is the
// signature of a mid-title or mid-term cut. Of the 12 K>1 splits measured on
// the real question sets, 2 cut a 《…》 title and 1 cut a straight-quoted
// title mid-token, e.g. "右和" → ["\"右", "\"这本书的出版社是哪家？"].
const boundaryRunes = "\"'“”‘’《》〈〉()（）[]【】{}"

// IsSemanticUnit reports whether one decomposed part is a complete, retrievable
// requirement rather than a fragment of a larger one.
//
// It is deliberately narrow. Its only job is to reject the shapes the measured
// miscuts actually took — too-short fragments and cut-at-a-boundary splits —
// because a false positive here costs one extra retrieval probe, while a false
// negative costs a phantom requirement that the loop can never satisfy.
func IsSemanticUnit(s string) bool {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) < minUnitRunes {
		return false
	}
	// A balanced pair of book/quote brackets is a normal quoted title; an
	// UNBALANCED one means the split ran through the middle of a title.
	var depth int
	for _, r := range s {
		if !strings.ContainsRune(boundaryRunes, r) {
			continue
		}
		switch r {
		case '《', '〈', '（', '(', '[', '【', '{', '“', '‘':
			depth++
		case '》', '〉', '）', ')', ']', '】', '}', '”', '’':
			depth--
		default: // straight and curly single/double quotes are ambiguous
			return false
		}
		if depth < 0 {
			return false
		}
	}
	return depth == 0
}

// coordinationMarkers are the explicit structures that license a K>1 split.
// A requirement list only exists when the QUERY says there is one; without one,
// every split is a guess.
//
// Measured case that made this a gate rather than a nicety (perf-plan §4.9):
// query "发明创造定义" — 发明创造 is a statutory term of art, and the LLM
// decomposer read it as the coordinate pair 发明 / 创造 (the prompt's rule #2
// forbids exactly this, and it complied with neither). The split produced a
// confident early stop (0 DEEP loops) citing the WRONG statute at 51% of the
// tokens. No interrogative, no "分别", no coordination of any kind: for a bare
// term-of-art lookup K=1 is the correct answer by construction.
var coordinationMarkers = []string{
	"分别", "各自", "以及", "同时", "还有", "此外", "另外", "分别指", "分别是什么",
}

// alternativeMarkers are the structures that look like coordination but are not.
// "或者" in a legal question states an either/or CONDITION — splitting it
// produces two requirements where the text states one
// ("数额特别巨大或者有其他特别严重情节的…判什么刑" is a single question).
var alternativeMarkers = []string{"或者", "或是", "还是", "或则"}

// HasCoordination reports whether the query explicitly signals a requirement
// list. An alternative marker anywhere disqualifies it: a query can contain both
// ("…和…分别指什么，或者有例外吗") and the alternative reading dominates.
//
// A query that asks two or more separate questions licenses a list on its own —
// one question per clause — with no marker word. Measured 2026-09-28 (live run
// on the 727k-rune novel): "孙悟空在斜月三星洞跟随谁学艺？学成了哪些本领？"
// has no marker, so a correct two-part model split was vetoed whole — the
// decompose call cost ~837 tokens and produced exactly the two atomic
// requirements, all discarded for K=1. The asymmetry is the license's
// justification: a false LICENSE costs at most K≤4 extra cover checks on parts
// the model itself proposed, while a false VETO wastes the whole call — and
// the bare term-of-art shape the gate exists for ("发明创造定义") carries no
// interrogative at all, so this license cannot reopen that defect.
func HasCoordination(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	for _, m := range alternativeMarkers {
		if strings.Contains(q, m) {
			return false
		}
	}
	for _, m := range coordinationMarkers {
		if strings.Contains(q, m) {
			return true
		}
	}
	return strings.Count(q, "？")+strings.Count(q, "?") > 1
}

// BuildParts turns already-decomposed requirements into Facts, applying the
// semantic-unit gate and the count ceiling. It returns nil when nothing
// survives, which is the caller's signal to fall back to the heuristic Build —
// a decomposer that produces garbage must degrade to today's behaviour, never
// to a search with unreachable requirements.
//
// The query is required because a K>1 result additionally needs the QUERY to
// license a requirement list (HasCoordination). A multi-part result for a query
// with no coordination structure is the measured 发明创造 defect.
func BuildParts(query string, parts []string) []Fact {
	fx, _ := BuildPartsWhy(query, parts)
	return fx
}

// Why names the gate that produced an empty BuildPartsWhy result, for the
// verbose log: from the outside "nothing survived" and "not licensed" are
// indistinguishable, and telling them apart is the first debugging step after
// a fallback fires.
const (
	WhyNone         = ""              // accepted (or trivially empty input)
	WhyUnits        = "units"         // every part failed IsSemanticUnit
	WhyCoordination = "coordination"  // units survived, query licenses no list
)

// BuildPartsWhy is BuildParts with the veto reason spelled out.
func BuildPartsWhy(query string, parts []string) ([]Fact, string) {
	var out []Fact
	for _, p := range parts {
		if len(out) >= maxParts {
			break
		}
		p = strings.TrimSpace(p)
		if !IsSemanticUnit(p) {
			continue
		}
		out = append(out, Fact{ID: "f" + itoa(len(out)+1), Query: p})
	}
	if len(out) > 1 && !HasCoordination(query) {
		return nil, WhyCoordination
	}
	if len(out) == 0 {
		return nil, WhyUnits
	}
	return out, WhyNone
}

// Build decomposes query into atomic facts (LENS D_req).
// Heuristic: conjunction / comparison marks spawn extra facts; otherwise K=1.
//
// MEASURED: on the real question sets (baike 64 / chinalaw 42 / realeval 30)
// this yields K=1 for 124 of 136 queries, and every one of the 12 K>1 splits
// is a miscut inside a book title, a defined term, or an enumeration — the
// model can copy it but not fix it. Production replaces it with a
// decomposer.Decomposer (see Decomposer); this stays as the deterministic
// baseline every gate runs on.
func Build(query string) []Fact {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	parts := splitQuery(q)
	out := make([]Fact, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, Fact{ID: "f" + itoa(i+1), Query: p})
	}
	if len(out) == 0 {
		out = []Fact{{ID: "f1", Query: q}}
	}
	return out
}

// Evaluate marks per-fact coverage from a shared sample pool and returns the
// weakest-requirement report.
func Evaluate(facts []Fact, samples []mcs.Sample) Report {
	rep := Report{Facts: facts, K: len(facts), Weakest: 1}
	if len(facts) == 0 {
		rep.Complete = false
		rep.Weakest = 0
		return rep
	}
	weakest := 1.0
	for i := range facts {
		f := &facts[i]
		kws := mcs.Fields(f.Query)
		var best *mcs.Sample
		bestHit := 0.0
		nearMiss := 0.0
		for j := range samples {
			sm := samples[j]
			if sm.Score < CoverScore {
				continue
			}
			hit := hitRatio(kws, sm.Content)
			if hit > nearMiss {
				nearMiss = hit
			}
			// Coverage needs BOTH the fragment share and a contiguous
			// 3-character core of the fact in the window: the share alone
			// let half a word cover a whole fact (the red-light refusal
			// that looked "covered" on "红灯").
			if hit < CoverHit || !corePresent(f.Query, sm.Content) {
				continue
			}
			if best == nil || hit > bestHit || (hit == bestHit && sm.Score > best.Score) {
				cp := sm
				best = &cp
				bestHit = hit
			}
		}
		if best != nil {
			f.Covered = true
			f.Score = best.Score
			f.SourceID = best.Source
			f.Window = best
		} else {
			f.Covered = false
			f.Score = 0
			f.NearMiss = nearMiss
			f.SourceID = ""
			f.Window = nil
		}
		if f.Score/10 < weakest {
			weakest = f.Score / 10
		}
		if !f.Covered {
			weakest = 0
			rep.Missing = append(rep.Missing, f.ID)
		}
	}
	rep.Weakest = weakest
	rep.Complete = len(rep.Missing) == 0
	rep.Facts = facts
	return rep
}

// hasCovers reports whether any sample carries oracle annotations.
func hasCovers(samples []mcs.Sample) bool {
	for _, sm := range samples {
		if len(sm.Covers) > 0 {
			return true
		}
	}
	return false
}

// EvaluateOracle is the oracle path: per-fact coverage comes from the
// scorer's observation vector (sample.Covers), not keyword overlap — one
// scoring call updated every fact. Facts no window claims stay open.
func EvaluateOracle(facts []Fact, samples []mcs.Sample) Report {
	rep := Report{Facts: facts, K: len(facts), Weakest: 1}
	if len(facts) == 0 {
		rep.Complete = false
		rep.Weakest = 0
		return rep
	}
	weakest := 1.0
	for i := range facts {
		f := &facts[i]
		var best *mcs.Sample
		nearMiss := 0.0
		for j := range samples {
			sm := samples[j]
			if sm.Score > nearMiss {
				nearMiss = sm.Score
			}
			if sm.Score < CoverScore {
				continue
			}
			claimed := false
			for _, id := range sm.Covers {
				if id == f.ID {
					claimed = true
					break
				}
			}
			if !claimed {
				continue
			}
			if best == nil || sm.Score > best.Score {
				cp := sm
				best = &cp
			}
		}
		if best != nil {
			f.Covered = true
			f.Score = best.Score
			f.SourceID = best.Source
			f.Window = best
		} else {
			f.Covered = false
			f.Score = 0
			f.NearMiss = nearMiss
			f.SourceID = ""
			f.Window = nil
			rep.Missing = append(rep.Missing, f.ID)
			weakest = 0
		}
		if cv := f.Score / 10; cv < weakest {
			weakest = cv
		}
	}
	rep.Weakest = weakest
	rep.Complete = len(rep.Missing) == 0
	rep.Facts = facts
	return rep
}

// ReportFor picks the oracle path when the scorer annotated covers and
// falls back to the keyword path for plain scorers (offline stub).
func ReportFor(facts []Fact, samples []mcs.Sample) Report {
	if hasCovers(samples) {
		return EvaluateOracle(facts, samples)
	}
	return Evaluate(facts, samples)
}

// ReportForOracle forces the oracle path: empty covers on every window
// means "covered nothing", not "no annotations". FactAware scorers (aigate)
// always emit the covers field — falling back to keywords would re-mark
// honest misses as covered.
func ReportForOracle(facts []Fact, samples []mcs.Sample) Report {
	return EvaluateOracle(facts, samples)
}

// NeedContinue is the budget-aware continue predicate: keep
// exploring while some requirement is open and the loop budget remains.
func NeedContinue(rep Report, loops, maxLoops int) bool {
	if loops >= maxLoops {
		return false
	}
	return !rep.Complete
}

// MissingQueries returns the fact queries still open (self-correction input),
// WEAKEST FIRST. SSOT §3.5: "NeedContinue/MissingQueries 驱动有界自纠错（最弱
// 需求优先扩窗）".
//
// "Weakest" is the NearMiss signal Evaluate/EvaluateOracle record for an
// uncovered fact (how close the best window came, without clearing CoverHit).
// Sorting on Fact.Score would be a no-op: every UNCOVERED fact has Score 0 by
// construction, so the order would silently fall back to declaration order —
// which is what this function did before the signal existed. Facts with equal
// NearMiss keep declaration order, so the result stays deterministic.
func MissingQueries(facts []Fact, rep Report) []string {
	miss := map[string]bool{}
	for _, id := range rep.Missing {
		miss[id] = true
	}
	type open struct {
		query    string
		nearMiss float64
		order    int
	}
	var opens []open
	for i, f := range facts {
		if miss[f.ID] {
			opens = append(opens, open{query: f.Query, nearMiss: f.NearMiss, order: i})
		}
	}
	sort.SliceStable(opens, func(i, j int) bool {
		if opens[i].nearMiss != opens[j].nearMiss {
			return opens[i].nearMiss < opens[j].nearMiss // least support first
		}
		return opens[i].order < opens[j].order
	})
	out := make([]string, 0, len(opens))
	for _, o := range opens {
		out = append(out, o.query)
	}
	return out
}

// corePresent reports whether the window carries a contiguous span of the
// FACT — at least 3 characters of the query verbatim (two adjacent bigrams
// of a CJK run). mcs.Fields expands a CJK run into overlapping bigrams that
// are all fragments of ONE word, so counting each fragment independently
// let a window holding half a word ("红灯" for "闯红灯", "试用" for
// "试用期") clear the old 0.5 share — a fact looked covered by a sliver of
// its own rarest term. A fact shorter than one span imposes no requirement
// (the share test is the whole test at that size).
func corePresent(fact, content string) bool {
	r := []rune(fact)
	if len(r) < 3 {
		return true
	}
	low := strings.ToLower(content)
	for i := 0; i+2 < len(r); i++ {
		if strings.Contains(low, strings.ToLower(string(r[i:i+3]))) {
			return true
		}
	}
	return false
}

func hitRatio(kws []string, content string) float64 {
	if len(kws) == 0 {
		return 0
	}
	low := strings.ToLower(content)
	hits := 0
	for _, w := range kws {
		if w != "" && strings.Contains(low, strings.ToLower(w)) {
			hits++
		}
	}
	return float64(hits) / float64(len(kws))
}

func splitQuery(q string) []string {
	remaining := q
	var parts []string
	for {
		cutAt, cutLen := findMark(remaining)
		if cutAt < 0 {
			break
		}
		left := strings.TrimSpace(remaining[:cutAt])
		if left != "" {
			parts = append(parts, left)
		}
		remaining = remaining[cutAt+cutLen:]
	}
	remaining = strings.TrimSpace(remaining)
	if remaining != "" {
		parts = append(parts, remaining)
	}
	if len(parts) == 0 {
		return []string{q}
	}
	if len(parts) > 4 {
		parts = parts[:4]
	}
	return parts
}

func findMark(s string) (idx, length int) {
	bestIdx, bestLen := -1, 0
	for _, m := range splitMarks {
		i := strings.Index(strings.ToLower(s), strings.ToLower(m))
		if i < 0 {
			continue
		}
		// A single-character CJK conjunction splits only when BOTH sides
		// carry at least two characters. The old boundary() guard was dead
		// code (it tested a UTF-8 continuation byte against a lead-byte
		// range, so it was invariably true) and single-char marks were
		// splitting inside words: "参与违法怎么办" produced the degenerate
		// facts ["参", …] whose empty keyword set can never be covered, so
		// the DEEP loop burned its whole budget on a phantom requirement.
		// Two characters on each side is the decidable, word-list-free
		// version of "not glued into a longer word" (it also keeps "与否"
		// and trailing "和" whole); marks like "以及" are unambiguous.
		if m == "和" || m == "与" || m == "及" {
			if utf8.RuneCountInString(s[:i]) < 2 || utf8.RuneCountInString(s[i+len(m):]) < 2 {
				continue
			}
		}
		if bestIdx < 0 || i < bestIdx {
			bestIdx, bestLen = i, len(m)
		}
	}
	return bestIdx, bestLen
}

var splitMarks = []string{
	"以及", "并且", "同时", "另外", "此外",
	"相比", "对比", "还是", "或者",
	"和", "与", "及",
	" and ", " and/or ", " or ", " vs ", " versus ", " compared ",
	"；", ";",
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

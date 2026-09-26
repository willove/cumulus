// Package prior is a low-cost, query-conditioned prior over
// candidate evidence regions, fused from multiple cheap signals and factored
// as π_file × π_pos. Nothing here reads raw text through an LLM.
package prior

import (
	"math"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/mcs"
	"github.com/willove/cumulus/internal/source"
)

// Signal names (K0 in LENS Prop.1).
const (
	SigLexical = "lexical"
	SigPath    = "path"
	SigStruct  = "struct"
	SigHistory = "history"
	SigScan    = "scan"
)

// DefaultWeights fuse the signal family (LENS Eq.6).
var DefaultWeights = map[string]float64{
	SigLexical: 0.40,
	SigPath:    0.20,
	SigStruct:  0.15,
	SigHistory: 0.15,
	SigScan:    0.10,
}

// FileScore is π_file(d|q, Dt).
type FileScore struct {
	SourceID string             `json:"source_id"`
	Title    string             `json:"title"`
	Score    float64            `json:"score"`
	Signals  map[string]float64 `json:"signals"`
}

// PosScore is π_pos(s,e|d,q).
type PosScore struct {
	SourceID string  `json:"source_id"`
	Start    int     `json:"start"`
	End      int     `json:"end"`
	Score    float64 `json:"score"`
	Anchor   string  `json:"anchor"`
}

// Belief is the two-level prior (Eq.7).
type Belief struct {
	Files   []FileScore         `json:"files"`
	TopPos  map[string]PosScore `json:"top_pos"`
	Weights map[string]float64  `json:"weights"`
}

// History is a warm-prior input from prior successful searches.
type History struct {
	SourceIDs   []string
	QueryTokens []string
	// DocWeights carries query-conditioned usage weights (the affinity
	// ledger and the session evidence stack), already accumulated over the
	// incoming query's tokens. A document present here is scored by weight;
	// the binary/token fallback below stays for callers that pass only
	// SourceIDs (eval runs, offline stacks).
	DocWeights map[string]float64
}

// Saturate maps an accumulated usage weight onto (0,1) monotonically:
// one token of support → 0.5, two → 0.667, three → 0.75 … Ordering is
// preserved and no accumulation can exceed the arm's 1.0 ceiling.
func Saturate(w float64) float64 { return saturate(w) }

func saturate(w float64) float64 {
	if w <= 0 {
		return 0
	}
	return w / (1 + w)
}

// HistoryFrom merges successful-evidence sources and snippet texts into a
// warm-prior History (历史成功证据 family). Snippets contribute
// their tokens as QueryTokens; source IDs alone already score 1.0 on the
// history arm.
func HistoryFrom(sourceIDs, snippets []string) *History {
	h := &History{SourceIDs: append([]string(nil), sourceIDs...)}
	seen := map[string]bool{}
	for _, sn := range snippets {
		for _, tok := range mcs.Fields(sn) {
			if len(tok) < 2 || seen[tok] {
				continue
			}
			seen[tok] = true
			h.QueryTokens = append(h.QueryTokens, tok)
		}
	}
	if len(h.QueryTokens) > 64 {
		h.QueryTokens = h.QueryTokens[:64]
	}
	return h
}

// Build fuses the five signal families into a query-conditioned prior.
func Build(query string, sources []source.Source, hist *History, topK int) Belief {
	return Rank(mcs.Fields(query), sources, hist, topK)
}

// Rank is Build over caller-supplied fields (the fast cascade feeds it
// LLM-extracted fields rather than the raw query).
func Rank(fields []string, sources []source.Source, hist *History, topK int) Belief {
	if topK <= 0 {
		topK = 10
	}
	nActive := 0
	for _, s := range sources {
		if s.Status == source.StatusActive {
			nActive++
		}
	}
	df := map[string]int{}
	for _, s := range sources {
		if s.Status != source.StatusActive {
			continue
		}
		low := strings.ToLower(s.Body)
		for _, f := range fields {
			if strings.Contains(low, strings.ToLower(f)) {
				df[f]++
			}
		}
	}
	var files []FileScore
	topPos := map[string]PosScore{}
	for _, s := range sources {
		if s.Status != source.StatusActive {
			continue
		}
		sig := map[string]float64{
			SigLexical: lexScore(fields, s, df, nActive),
			SigPath:    pathScore(fields, s),
			SigStruct:  structScore(fields, s),
			SigHistory: historyScore(s, hist),
			SigScan:    scanScore(s),
		}
		fused := 0.0
		for k, v := range sig {
			fused += DefaultWeights[k] * v
		}
		files = append(files, FileScore{SourceID: s.ID, Title: s.Title, Score: fused, Signals: sig})
		topPos[s.ID] = posScore(fields, s)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Score > files[j].Score })
	if len(files) > topK {
		files = files[:topK]
	}
	if len(files) > 0 && files[0].Score > 0 {
		mx := files[0].Score
		for i := range files {
			files[i].Score = files[i].Score / mx
		}
	}
	return Belief{Files: files, TopPos: topPos, Weights: DefaultWeights}
}

// Admitted returns the C_init file set (Prop.1 space compression input).
func (b Belief) Admitted() []string {
	out := make([]string, 0, len(b.Files))
	for _, f := range b.Files {
		out = append(out, f.SourceID)
	}
	return out
}

// Filter restricts a source slice to the admitted C_init set.
func (b Belief) Filter(sources []source.Source) []source.Source {
	keep := map[string]bool{}
	for _, id := range b.Admitted() {
		keep[id] = true
	}
	var out []source.Source
	for _, s := range sources {
		if keep[s.ID] {
			out = append(out, s)
		}
	}
	return out
}

func lexScore(fields []string, s source.Source, df map[string]int, n int) float64 {
	if len(fields) == 0 {
		return 0
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
	return sc
}

func pathScore(fields []string, s source.Source) float64 {
	blob := strings.ToLower(s.Title + " " + s.SourceURI + " " + s.BusinessKey)
	if len(fields) == 0 {
		return 0
	}
	hits := 0
	for _, f := range fields {
		if f != "" && strings.Contains(blob, strings.ToLower(f)) {
			hits++
		}
	}
	return float64(hits) / float64(len(fields))
}

func structScore(fields []string, s source.Source) float64 {
	if len(s.Structure) == 0 {
		// No structure = no structural evidence. This used to return a free
		// 0.2 baseline, so structure-less documents scored HIGHER on this arm
		// than structured documents with no label hit — backwards.
		return 0
	}
	if len(fields) == 0 {
		return 0
	}
	best := 0.0
	for _, sp := range s.Structure {
		lab := strings.ToLower(sp.Label)
		hits := 0
		for _, f := range fields {
			if f != "" && strings.Contains(lab, strings.ToLower(f)) {
				hits++
			}
		}
		r := float64(hits) / float64(len(fields))
		if r > best {
			best = r
		}
	}
	return best
}

func historyScore(s source.Source, hist *History) float64 {
	if hist == nil {
		return 0
	}
	// Query-conditioned usage first: the ledger/session weights for THIS
	// query's tokens. The old global-binary path remains the fallback.
	if w, ok := hist.DocWeights[s.ID]; ok && w > 0 {
		return saturate(w)
	}
	for _, id := range hist.SourceIDs {
		// Exact match only: substring matching made src:abc a history hit for
		// src:abcdef, so an unrelated document inherited a full-strength
		// history signal.
		if id == s.ID {
			return 1.0
		}
	}
	if len(hist.QueryTokens) == 0 {
		return 0
	}
	low := strings.ToLower(s.Body)
	hits := 0
	for _, t := range hist.QueryTokens {
		if t != "" && strings.Contains(low, strings.ToLower(t)) {
			hits++
		}
	}
	return 0.5 * float64(hits) / float64(len(hist.QueryTokens))
}

func scanScore(s source.Source) float64 {
	sc := 0.3
	if s.Version > 1 {
		sc += 0.2
	}
	n := len([]rune(s.Body))
	// The 200..8000-rune band is unprovenanced, and on a long-body corpus
	// (this deployment averages ~10K runes) it never fires — the +0.3 is
	// dead weight there, not a signal. Re-measure per corpus or drop it.
	if n > 200 && n < 8000 {
		sc += 0.3
	}
	if s.Status == source.StatusActive {
		sc += 0.2
	}
	if sc > 1 {
		sc = 1
	}
	return sc
}

func posScore(fields []string, s source.Source) PosScore {
	best := PosScore{SourceID: s.ID, Start: 0, End: minInt(240, len([]rune(s.Body)))}
	if len(s.Structure) == 0 {
		return best
	}
	for _, sp := range s.Structure {
		lab := strings.ToLower(sp.Label)
		seg := ""
		// Span offsets are rune indices (source.BuildStructure); slice runes,
		// not bytes — byte slicing mangles CJK segments.
		body := []rune(s.Body)
		if sp.Start >= 0 && sp.End <= len(body) && sp.Start < sp.End {
			seg = strings.ToLower(string(body[sp.Start:sp.End]))
		}
		hits := 0
		for _, f := range fields {
			if f == "" {
				continue
			}
			fl := strings.ToLower(f)
			if strings.Contains(lab, fl) || strings.Contains(seg, fl) {
				hits++
			}
		}
		r := 0.0
		if len(fields) > 0 {
			r = float64(hits) / float64(len(fields))
		}
		if r > best.Score {
			best = PosScore{SourceID: s.ID, Start: sp.Start, End: sp.End, Score: r, Anchor: sp.Label}
		}
	}
	return best
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Package mcs is Monte Carlo evidence sampling: treat extraction as a sampling
// problem over the source body, not a chunking problem. Stage 1 stratified +
// fuzzy anchors (explore), stage 2 gaussian around high-score seeds (exploit),
// stage 3 top-k synthesis. Scoring is pluggable — offline stub for gates,
// aigate-backed scorer in production.
package mcs

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"strings"
)

// Sample is one candidate window of a source body.
type Sample struct {
	Start     int     `json:"start"`
	End       int     `json:"end"`
	Content   string  `json:"content"`
	Source    string  `json:"source"`
	Score     float64 `json:"score"`
	Reasoning string  `json:"reasoning"`
}

// Scorer rates one sample against the query (0-10 scale).
type Scorer interface {
	Score(ctx context.Context, query string, s Sample) (score float64, reasoning string, err error)
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
	switch {
	case density >= 0.8:
		return 8 + 2*density - 1.6, "strong keyword coverage", nil
	case density >= 0.4:
		return 4 + 10*(density-0.4)/0.4, "partial keyword coverage", nil
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
		SmallFileRunes:  1000,
		MaxEvidence:     15000,
	}
}

// Sampler runs the three-stage loop.
type Sampler struct {
	Cfg    Config
	Scorer Scorer
	RNG    *rand.Rand
}

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
		sm := Sample{Start: 0, End: len(runes), Content: body, Source: "full"}
		sc, why, err := s.Scorer.Score(ctx, query, sm)
		if err != nil {
			return nil, err
		}
		sm.Score, sm.Reasoning = sc, why
		return []Sample{sm}, nil
	}

	all := s.stage1(runes, query, body)
	evaluated, err := s.evalAll(ctx, query, all)
	if err != nil {
		return nil, err
	}
	seeds := topSeeds(evaluated, s.Cfg.TopSeeds)

	for r := 0; r < s.Cfg.Rounds; r++ {
		var batch []Sample
		if r == 0 {
			batch = s.stage1(runes, query, body)
		} else {
			batch = s.gaussian(runes, seeds)
		}
		if len(batch) == 0 {
			continue
		}
		ev, err := s.evalAll(ctx, query, batch)
		if err != nil {
			return nil, err
		}
		evaluated = append(evaluated, ev...)
		seeds = topSeeds(evaluated, s.Cfg.TopSeeds)
	}

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

func (s *Sampler) stage1(runes []rune, query, body string) []Sample {
	n := len(runes)
	half := s.Cfg.Window
	var out []Sample
	k := s.Cfg.SamplesPerRound
	for i := 0; i < k; i++ {
		center := (2*i + 1) * n / (2 * k)
		out = append(out, window(runes, center, half, "stratified"))
	}
	for _, w := range Fields(query) {
		if !strings.Contains(strings.ToLower(body), w) {
			continue
		}
		center := runeIndexOf(runes, w)
		out = append(out, window(runes, center, half, "fuzz"))
	}
	return out
}

func (s *Sampler) gaussian(runes []rune, seeds []Sample) []Sample {
	if len(seeds) == 0 {
		return s.stage1(runes, "", string(runes))
	}
	var out []Sample
	for i := 0; i < s.Cfg.SamplesPerRound; i++ {
		seed := seeds[i%len(seeds)]
		center := (seed.Start + seed.End) / 2
		jitter := int(s.RNG.NormFloat64() * s.Cfg.Sigma)
		out = append(out, window(runes, center+jitter, s.Cfg.Window, "gaussian"))
	}
	return out
}

func (s *Sampler) evalAll(ctx context.Context, query string, in []Sample) ([]Sample, error) {
	out := make([]Sample, 0, len(in))
	for _, sm := range in {
		sc, why, err := s.Scorer.Score(ctx, query, sm)
		if err != nil {
			return nil, err
		}
		sm.Score, sm.Reasoning = sc, why
		out = append(out, sm)
	}
	return out, nil
}

func window(runes []rune, center, half int, source string) Sample {
	n := len(runes)
	start := center - half
	if start < 0 {
		start = 0
	}
	end := center + half
	if end > n {
		end = n
	}
	if start > end {
		start, end = 0, n
	}
	return Sample{
		Start:   start,
		End:     end,
		Content: string(runes[start:end]),
		Source:  source,
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

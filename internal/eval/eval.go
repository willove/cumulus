// Package eval is the LENS evidence-quality protocol (B3): Ev.Rec / Ground /
// Closed-Book reference and a McNemar paired test, so answer quality is not
// judged by EM alone. Offline scoring is deterministic (substring / overlap);
// an aigate judge can replace Correct() without changing the metric shape.
package eval

import (
	"math"
	"strings"
)

// Item is one evaluation question with gold supporting documents.
type Item struct {
	ID     string   `json:"id"`
	Query  string   `json:"query"`
	Answer string   `json:"answer"`       // gold answer string (EM target)
	Gold   []string `json:"gold_sources"` // supporting-fact document IDs / keys
}

// Prediction is one system run on one item.
type Prediction struct {
	Query     string   `json:"query"`
	Answer    string   `json:"answer"`
	SourceIDs []string `json:"source_ids"`
	Resolved  int      `json:"resolved_refs"`
	Refs      int      `json:"refs"`
	Skipped   bool     `json:"skipped"`
}

// ItemScore is the per-item verdict (LENS Table 2 columns).
type ItemScore struct {
	ID       string `json:"id"`
	Correct  bool   `json:"correct"`
	EvRec    bool   `json:"ev_rec"`
	Grounded bool   `json:"grounded"`
}

// Report is the aggregate evidence-quality scorecard.
type Report struct {
	N      int     `json:"n"`
	EM     float64 `json:"em"`
	EvRec  float64 `json:"ev_rec"`
	Ground float64 `json:"ground"`
	Notes  string  `json:"notes,omitempty"`
}

// Score scores one prediction against one gold item.
func Score(it Item, p Prediction) ItemScore {
	gold := map[string]bool{}
	for _, g := range it.Gold {
		if g != "" {
			gold[strings.ToLower(g)] = true
		}
	}
	got := map[string]bool{}
	for _, s := range p.SourceIDs {
		if s != "" {
			got[strings.ToLower(s)] = true
		}
	}
	ev := false
	for g := range gold {
		if got[g] {
			ev = true
			break
		}
	}
	if !ev {
		for g := range gold {
			for id := range got {
				if strings.Contains(id, g) || strings.Contains(g, id) {
					ev = true
					break
				}
			}
			if ev {
				break
			}
		}
	}
	grounded := !p.Skipped && strings.TrimSpace(p.Answer) != "" && p.Refs > 0 && p.Resolved >= p.Refs
	return ItemScore{
		ID:       it.ID,
		Correct:  Correct(it.Answer, p.Answer),
		EvRec:    ev,
		Grounded: grounded,
	}
}

// Correct is a lenient exact-match: gold is a substring of the produced answer.
func Correct(gold, got string) bool {
	g := strings.TrimSpace(gold)
	if g == "" {
		return false
	}
	a := strings.TrimSpace(got)
	if a == "" {
		return false
	}
	if strings.EqualFold(g, a) {
		return true
	}
	return strings.Contains(strings.ToLower(a), strings.ToLower(g))
}

// Aggregate rolls item scores into the LENS scorecard.
func Aggregate(items []ItemScore) Report {
	r := Report{N: len(items)}
	if len(items) == 0 {
		return r
	}
	var c, e, g float64
	for _, s := range items {
		if s.Correct {
			c++
		}
		if s.EvRec {
			e++
		}
		if s.Grounded {
			g++
		}
	}
	n := float64(len(items))
	r.EM = c / n
	r.EvRec = e / n
	r.Ground = g / n
	return r
}

// ClosedBook scores a no-retrieval run. EM still counts; EvRec and Ground are false.
func ClosedBook(it Item, answer string) ItemScore {
	return ItemScore{
		ID:       it.ID,
		Correct:  Correct(it.Answer, answer),
		EvRec:    false,
		Grounded: false,
	}
}

// McNemar is the paired discordant-pair test (LENS §6.6).
type McNemar struct {
	BOnly int     `json:"b_only"`
	COnly int     `json:"c_only"`
	P     float64 `json:"p"`
	N     int     `json:"n"`
}

// Compare runs McNemar on the Correct bit of two equal-length score lists.
func Compare(a, b []ItemScore) McNemar {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	m := McNemar{N: n}
	for i := 0; i < n; i++ {
		if a[i].Correct && !b[i].Correct {
			m.BOnly++
		} else if !a[i].Correct && b[i].Correct {
			m.COnly++
		}
	}
	m.P = exactBinomial(m.BOnly, m.COnly)
	return m
}

func exactBinomial(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1
	}
	k := b
	if c < k {
		k = c
	}
	sum := 0.0
	for i := 0; i <= k; i++ {
		sum += binomPMF(n, i, 0.5)
	}
	p := 2 * sum
	if p > 1 {
		p = 1
	}
	return p
}

func binomPMF(n, k int, p float64) float64 {
	return float64(binom(n, k)) * math.Pow(p, float64(k)) * math.Pow(1-p, float64(n-k))
}

func binom(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	r := 1
	for i := 1; i <= k; i++ {
		r = r * (n - k + i) / i
	}
	return r
}

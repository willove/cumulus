// Package eval is the LENS evidence-quality protocol (B3): Ev.Rec / Ground /
// Closed-Book reference and a McNemar paired test, so answer quality is not
// judged by EM alone. Offline scoring is deterministic (substring / overlap);
// an aigate judge can replace Correct() without changing the metric shape.
package eval

import (
	"crypto/sha256"
	"encoding/hex"
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
	Answered bool   `json:"answered"` // a non-empty answer was produced at all
}

// Frozen binds a scoreboard to the exact artifacts it ran on (LENS A.6):
// items / corpus / config checksums so an ablation row cannot silently change
// the sample set (ir-rag A6).
type Frozen struct {
	ItemsSHA  string `json:"items_sha"`
	CorpusSHA string `json:"corpus_sha,omitempty"`
	ConfigSHA string `json:"config_sha,omitempty"`
	OrderSeed int    `json:"order_seed,omitempty"`
}

// HashBytes returns the sha256 hex digest of b (empty input → empty string).
func HashBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Freeze builds the artifact-binding header for a run.
func Freeze(itemsRaw, corpusRaw, configRaw []byte, orderSeed int) Frozen {
	return Frozen{
		ItemsSHA:  HashBytes(itemsRaw),
		CorpusSHA: HashBytes(corpusRaw),
		ConfigSHA: HashBytes(configRaw),
		OrderSeed: orderSeed,
	}
}

// Report is the aggregate evidence-quality scorecard.
type Report struct {
	N        int      `json:"n"`
	EM       float64  `json:"em"`
	EvRec    float64  `json:"ev_rec"`
	Ground   float64  `json:"ground"`
	Taxonomy Taxonomy `json:"taxonomy"`
	Frozen   *Frozen  `json:"frozen,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

// Taxonomy is the LENS four-way mutually exclusive verdict (§6.1): every item
// lands in exactly one class, so the fields sum to N. RetrievedOnly separates
// "looked in the right place but answered badly" from never finding the
// evidence at all.
type Taxonomy struct {
	Correct       int `json:"correct"`
	RetrievedOnly int `json:"retrieved_but_unanswered"`
	AnsweredWrong int `json:"answered_but_wrong"`
	NotRetrieved  int `json:"not_retrieved"`
}

// Classify maps one item into the failure taxonomy. Correct wins over
// everything; retrieved-but-wrong is its own class; wrong without gold
// evidence splits by whether the system produced an answer at all.
func Classify(s ItemScore) string {
	switch {
	case s.Correct:
		return "correct"
	case s.EvRec:
		return "retrieved_but_unanswered"
	case s.Answered:
		return "answered_but_wrong"
	default:
		return "not_retrieved"
	}
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
		Answered: !p.Skipped && strings.TrimSpace(p.Answer) != "",
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
		switch Classify(s) {
		case "correct":
			r.Taxonomy.Correct++
		case "retrieved_but_unanswered":
			r.Taxonomy.RetrievedOnly++
		case "answered_but_wrong":
			r.Taxonomy.AnsweredWrong++
		case "not_retrieved":
			r.Taxonomy.NotRetrieved++
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

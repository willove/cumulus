// Package eval is the evidence-quality protocol: Ev.Rec / Ground /
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

// Taxonomy is the four-way mutually exclusive verdict: every item
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
	// Ev.Rec is exact membership over CANONICAL keys — never substring
	// containment. The old two-way strings.Contains made gold "law1" count as
	// retrieved by "law10" and "src:x" by "src:xy", so an unrelated document
	// could satisfy evidence retrieval. Canonicalization covers the one
	// legitimate mismatch: gold is a business key ("handbook") while the
	// prediction carries the internal revision id ("src:handbook#1").
	canonGold := map[string]bool{}
	for g := range gold {
		for _, k := range canonicalKeys(g) {
			canonGold[k] = true
		}
	}
	canonGot := map[string]bool{}
	for id := range got {
		for _, k := range canonicalKeys(id) {
			canonGot[k] = true
		}
	}
	ev := false
	for g := range canonGold {
		if canonGot[g] {
			ev = true
			break
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
//
// A NUMERIC gold must sit on a digit boundary. Plain substring matching made
// gold "128" score "1280" and "12800" correct — and gold answers in this suite
// are overwhelmingly numeric claims, so EM was silently inflated on exactly the
// facts that matter most. Non-numeric gold keeps substring semantics (natural
// language answers legitimately wrap the gold phrase).
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
	lowA, lowG := strings.ToLower(a), strings.ToLower(g)
	if isNumeric(lowG) {
		return boundedContains(lowA, lowG)
	}
	return strings.Contains(lowA, lowG)
}

// isNumeric reports whether s is a decimal number (separators allowed).
func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' && r != ',' && r != '-' {
			return false
		}
	}
	return true
}

// boundedContains reports whether needle appears in hay delimited by non-digits
// (or the string bounds) on both sides — so "128" matches "128，超时" and
// "(128)" but not "1280".
func boundedContains(hay, needle string) bool {
	for from := 0; from <= len(hay)-len(needle); {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(needle)
		leftOK := start == 0 || !isDigit(hay[start-1])
		rightOK := end == len(hay) || !isDigit(hay[end])
		if leftOK && rightOK {
			return true
		}
		from = start + 1
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

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

// canonicalKeys returns the forms an identifier may be matched under: itself
// (lowercased) and, for an internal revision identity "src:<key>#<version>",
// the bare business key. Both the gold list and the prediction may carry either
// form, so both are indexed and compared exactly.
func canonicalKeys(id string) []string {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return nil
	}
	out := []string{id}
	if rest, ok := strings.CutPrefix(id, "src:"); ok {
		if i := strings.LastIndex(rest, "#"); i > 0 {
			rest = rest[:i]
		}
		if rest != "" && rest != id {
			out = append(out, rest)
		}
	}
	return out
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

// McNemar is the paired discordant-pair test.
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

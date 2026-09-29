// Package calib is the first brick of the cognitive engine: the minimal
// closed loop that turns hand calibration into a standing mechanism.
//
//     mine (confidence bands × outcomes from the system's own episodes)
//   → propose (the lowest line whose servable band meets the target rate)
//   → decide (apply/reject from a paired self-test: quality gain vs cost)
//   → apply (a store-backed override the engine reads at request time)
//
// The loop mechanizes exactly what was done by hand on 2026-09-29/30 (95
// archived FAST rows mined, 0.35 shown dead, 0.85 proposed, the pair run,
// +10% tokens for zero gain, rejected). The Bitter-Lesson discipline it
// encodes (R1): a hand rule is a legal bridge while compute is billed, but
// every rule must keep a learning takeover point — this package is the one
// for the escalation line.
package calib

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
)

// Episode is one mined query outcome. Today it bridges off eval result rows
// (conf/mode/eval.correct); production episodes plug in the same shape when
// the ledger grows conf/outcome columns.
type Episode struct {
	Conf    float64
	Correct bool
	Mode    string // FAST rows are the servable population the line gates
}

// Band aggregates one 0.05-wide confidence band of FAST-served episodes.
type Band struct {
	Low     float64 `json:"low"`
	High    float64 `json:"high"`
	N       int     `json:"n"`
	Correct int     `json:"correct"`
	Rate    float64 `json:"rate"`
}

// Mine buckets FAST episodes into bands, ascending.
func Mine(eps []Episode) []Band {
	const w = 0.05
	byIdx := map[int]*Band{}
	for _, e := range eps {
		if !strings.HasPrefix(strings.ToUpper(e.Mode), "FAST") {
			continue
		}
		idx := int(e.Conf / w)
		if idx < 0 {
			idx = 0
		}
		if idx > 19 { // 0.05-wide bands: 0.95..1.00 is the top bucket
			idx = 19
		}
		b := byIdx[idx]
		if b == nil {
			b = &Band{Low: float64(idx) * w, High: float64(idx+1) * w}
			byIdx[idx] = b
		}
		b.N++
		if e.Correct {
			b.Correct++
		}
	}
	out := make([]Band, 0, len(byIdx))
	for _, b := range byIdx {
		b.Rate = float64(b.Correct) / float64(b.N)
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Low < out[j].Low })
	return out
}

// Proposal is a recalibration proposal with its evidential basis.
type Proposal struct {
	Current   float64 `json:"current"`
	Proposed  float64 `json:"proposed"`
	Target    float64 `json:"target"`
	ServeN    int     `json:"serve_n"`    // episodes at/above the proposed line
	ServeRate float64 `json:"serve_rate"` // their correct rate
	Bands     []Band  `json:"bands"`
}

// Propose returns the lowest confidence line whose band-and-above FAST
// population meets the target correct rate with at least minN support.
// Escalating the remainder trades tokens for the DEEP tier's rate. ok=false
// ("keep current") when no band qualifies — thin evidence is not a proposal.
func Propose(eps []Episode, current, target float64, minN int) (Proposal, bool) {
	bands := Mine(eps)
	// Descending: the first band (from high conf) where the at-and-above
	// population still meets the target is the lowest safe line.
	for i := len(bands) - 1; i >= 0; i-- {
		n, correct := 0, 0
		for _, b := range bands[i:] {
			n += b.N
			correct += b.Correct
		}
		if n < minN {
			continue
		}
		if rate := float64(correct) / float64(n); rate >= target {
			p := Proposal{Current: current, Proposed: bands[i].Low, Target: target,
				ServeN: n, ServeRate: rate, Bands: bands}
			// A proposal below the current line is a cost cut, not the
			// quality move this loop exists for; keep the higher line.
			if p.Proposed <= current+1e-9 { // band-edge float wobble (0.35000...03)
				return Proposal{Current: current, Proposed: current, Target: target,
					ServeN: n, ServeRate: rate, Bands: bands}, false
			}
			return p, true
		}
	}
	return Proposal{Current: current, Proposed: current, Target: target, Bands: bands}, false
}

// PairStat is the paired self-test outcome the decision reads: arm0 runs the
// current line, arm1 the proposal, same frozen items.
type PairStat struct {
	N        int   `json:"n"`
	CorrectA int   `json:"correct_a"`
	CorrectB int   `json:"correct_b"`
	TokensA  int64 `json:"tokens_a"`
	TokensB  int64 `json:"tokens_b"`
}

// Verdict is the apply/reject decision with its reasons on the record.
type Verdict struct {
	Apply   bool     `json:"apply"`
	Reasons []string `json:"reasons"`
}

// MinGain is how many net correct items a proposal must buy on the pair to
// apply — with n≈30 and the measured judge floor (±5-6/30), one flipped item
// is noise; two is the smallest claim worth paying tokens for.
const MinGain = 2

// MaxTokRatio is the cost ceiling: the pair may cost at most 5% more tokens
// for the quality gain (the 0.85 proposal measured 1.10× for zero gain and
// was rejected on exactly this rule).
const MaxTokRatio = 1.05

// Decide turns a paired self-test into apply/reject. Both conditions must
// hold: quality gain ≥ MinGain AND token ratio ≤ MaxTokRatio.
func Decide(s PairStat) Verdict {
	v := Verdict{}
	gain := s.CorrectB - s.CorrectA
	ratio := 1.0
	if s.TokensA > 0 {
		ratio = float64(s.TokensB) / float64(s.TokensA)
	}
	v.Reasons = append(v.Reasons,
		fmt.Sprintf("correct %d→%d (gain %d, need ≥%d)", s.CorrectA, s.CorrectB, gain, MinGain),
		fmt.Sprintf("tokens ratio %.2f× (ceiling %.2f×)", ratio, MaxTokRatio))
	if gain >= MinGain && ratio <= MaxTokRatio {
		v.Apply = true
		v.Reasons = append(v.Reasons, "APPLY: quality gain clears the bar at acceptable cost")
		return v
	}
	v.Reasons = append(v.Reasons, "REJECT: keep the current line (thin gain or over-budget)")
	return v
}

// Store persists the applied line (KV clus:calib:escalate). The engine's
// escalateBelowLine() consults this BEFORE env/const — the learning takeover
// point; env stays the operator's manual override on top by simple absence
// (delete the key to fall back).
type Store struct {
	c cumulite.Port
}

// CalibKey is the single global applied-line key (per-namespace lines are a
// later refinement once namespaces diverge).
const CalibKey = "clus:calib:escalate"

type calibRecord struct {
	Line      float64   `json:"line"`
	AppliedAt time.Time `json:"applied_at"`
	Because   string    `json:"because,omitempty"`
}

func NewStore(c cumulite.Port) *Store { return &Store{c: c} }

// Save writes the applied line.
func (s *Store) Save(ctx context.Context, line float64, because string) error {
	raw, err := json.Marshal(calibRecord{Line: line, AppliedAt: time.Now().UTC(), Because: because})
	if err != nil {
		return err
	}
	return s.c.KVPut(ctx, CalibKey, raw, 0)
}

// resultRow is the eval result-row shape (conf/mode/eval.correct) the
// episode bridge reads — the same rows every A/B already archives, so the
// loop mines history that exists instead of waiting for new infrastructure.
type resultRow struct {
	ID    string `json:"id"`
	Mode  string `json:"mode"`
	Conf  float64 `json:"conf"`
	Eval  *struct {
		Correct *bool `json:"correct"`
	} `json:"eval"`
	Tokens      int64 `json:"search_tokens"`
	TokensAlias int64 `json:"tokens"`
}

// ReadEpisodes bridges one arm's result rows into episodes.
func ReadEpisodes(path string) ([]Episode, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Episode
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r resultRow
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if r.Eval == nil || r.Eval.Correct == nil {
			continue // unjudged rows carry no outcome signal
		}
		out = append(out, Episode{Conf: r.Conf, Correct: *r.Eval.Correct, Mode: r.Mode})
	}
	return out, sc.Err()
}

// ReadPair bridges both arms' rows into a paired self-test stat (same-id
// intersection, search tokens preferred over the legacy alias).
func ReadPair(aPath, bPath string) (PairStat, error) {
	read := func(path string) (map[string]resultRow, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		out := map[string]resultRow{}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var r resultRow
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			out[r.ID] = r
		}
		return out, sc.Err()
	}
	a, err := read(aPath)
	if err != nil {
		return PairStat{}, err
	}
	b, err := read(bPath)
	if err != nil {
		return PairStat{}, err
	}
	var st PairStat
	for id, ra := range a {
		rb, ok := b[id]
		if !ok || ra.Eval == nil || rb.Eval == nil || ra.Eval.Correct == nil || rb.Eval.Correct == nil {
			continue
		}
		st.N++
		if *ra.Eval.Correct {
			st.CorrectA++
		}
		if *rb.Eval.Correct {
			st.CorrectB++
		}
		st.TokensA += ra.Tokens
		if ra.Tokens == 0 {
			st.TokensA += ra.TokensAlias
		}
		st.TokensB += rb.Tokens
		if rb.Tokens == 0 {
			st.TokensB += rb.TokensAlias
		}
	}
	return st, nil
}

// Load reads the applied line; ok=false keeps env/const in charge.
func (s *Store) Load(ctx context.Context) (line float64, ok bool, err error) {
	raw, err := s.c.KVGet(ctx, CalibKey)
	if err != nil {
		if contract.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if len(raw) == 0 {
		return 0, false, nil
	}
	var r calibRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, false, err
	}
	if r.Line <= 0 || r.Line > 0.95 {
		return 0, false, fmt.Errorf("calib: stored line %v out of range", r.Line)
	}
	return r.Line, true, nil
}

// stabThreshold maps a self-play stability to the episode pseudo-label.
// Phase 0 measured the correct-group at mean 0.62 and the wrong group at
// 0.35 — 0.5 sits in the valley between them.
const stabThreshold = 0.5

// ReadUsage mines production episodes from the clus_usage ledger: FAST rows
// sampled by the self-play probe carry a stab column, which becomes the
// outcome pseudo-label. Unsampled rows (stab -1/absent) carry no outcome and
// are skipped.
func ReadUsage(ctx context.Context, c cumulite.Port, limit int) ([]Episode, error) {
	if limit <= 0 || limit > 4000 {
		limit = 4000
	}
	var out []Episode
	skip := 0
	for len(out) < limit {
		res, err := c.Query(ctx, "clus_usage", contract.Query{Limit: 500, Skip: skip})
		if err != nil {
			if contract.IsNotFound(err) {
				break
			}
			return nil, err
		}
		if len(res.Documents) == 0 {
			break
		}
		skip += len(res.Documents)
		for _, d := range res.Documents {
			mode, _ := d["mode"].(string)
			if !strings.HasPrefix(strings.ToUpper(mode), "FAST") {
				continue
			}
			stab, ok := toFloat(d["stab"])
			if !ok || stab < 0 {
				continue // not sampled (or failed) — no outcome signal
			}
			conf, ok := toFloat(d["conf"])
			if !ok {
				continue
			}
			out = append(out, Episode{Conf: conf, Correct: stab >= stabThreshold, Mode: mode})
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// toFloat reads a JSON number that may have been stored as float64 or int64.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

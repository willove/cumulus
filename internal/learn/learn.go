// Package learn is the thin guiding agent (认知引擎第四砖): one learning
// cycle that mines the system's own episodes for the biggest anomaly, asks
// ONE pinned LLM call to map it onto a whitelisted knob, and hands the rest
// to the deterministic machinery (calib -auto's pair discipline, the
// guardrail's alarm-only rollback).
//
// Sutton's shape on purpose: the intelligence lives in the LOOP (search over
// hypotheses × self-tests), not in a meta-persona — the LLM's only job is
// hypothesis generation from unstructured evidence, the one step pure
// statistics cannot name.
package learn

import (
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

// Knob is one whitelisted takeover point the agent may propose to turn.
type Knob struct {
	Name  string `json:"name"`
	Range string `json:"range"`
	What  string `json:"what"`
}

// Whitelist is the hard rule: the agent turns EXISTING knobs, never invents
// mechanisms (R1: every behavioural constant keeps a learning takeover
// point; this is the exhaustive list of the points the agent may reach).
var Whitelist = []Knob{
	{Name: "CLUS_ESCALATE_BELOW", Range: "0..0.95", What: "FAST→DEEP 升级线：低于线的 FAST 答案升级复核"},
	{Name: "CLUS_SUFFICIENT_SCORE", Range: "0..10", What: "强窗停止线：覆盖完备且最佳窗达线即收队"},
	{Name: "CLUS_COVER_SCORE", Range: "0..10", What: "覆盖/保留线：窗口进证据集与弱停的资格线"},
}

// Whitelisted reports whether the proposed knob is on the list.
func Whitelisted(name string) bool {
	for _, k := range Whitelist {
		if k.Name == name {
			return true
		}
	}
	return false
}

// Anomaly is one ranked failure shape mined from the episode ledger.
type Anomaly struct {
	Name   string  `json:"name"`
	Detail string  `json:"detail"`
	Score  float64 `json:"score"` // cost × frequency — higher ranks first
}

// MineAnomalies is the zero-LLM half: pure statistics over clus_usage.
// Every anomaly it can name is cheap to compute and expensive to ignore.
func MineAnomalies(ctx context.Context, c cumulite.Port, limit int) ([]Anomaly, error) {
	rows, err := scanUsage(ctx, c, limit)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var out []Anomaly
	add := func(name, detail string, score float64) {
		out = append(out, Anomaly{Name: name, Detail: detail, Score: score})
	}

	// Mode / exit mix.
	modes := map[string]int{}
	for _, r := range rows {
		modes[r["mode"].(string)]++
	}
	n := float64(len(rows))
	if fast := modes["FAST"]; fast > 0 && modes["DEEP"] > 0 {
		deep := modes["DEEP"]
		add("deep-share", fmt.Sprintf("DEEP %d/%d queries (%.0f%%) — escalation is the majority path", deep, len(rows), 100*float64(deep)/n), float64(deep)/n)
	}

	// Latency and token tails (the p90 of the ledger rows).
	lats := sortedInt(rows, "latency_ms")
	toks := sortedInt(rows, "tokens")
	if len(lats) > 0 {
		p90 := lats[(9*len(lats))/10]
		add("latency-tail", fmt.Sprintf("p90 latency %.1fs", float64(p90)/1000), float64(p90)/float64(lats[len(lats)-1]+1))
	}
	if len(toks) > 0 {
		p90 := toks[(9*len(toks))/10]
		add("token-tail", fmt.Sprintf("p90 tokens %d", p90), float64(p90)/float64(toks[len(toks)-1]+1))
	}

	// Sampled self-play wobble: FAST rows with a stab below the valley.
	wob, sampled := 0, 0
	for _, r := range rows {
		if r["mode"].(string) != "FAST" {
			continue
		}
		if stab, ok := toF64(r["stab"]); ok && stab >= 0 {
			sampled++
			if stab < 0.5 {
				wob++
			}
		}
	}
	if sampled >= 10 && wob > 0 {
		add("selfplay-wobble", fmt.Sprintf("%d/%d sampled FAST answers wobble (stab<0.5)", wob, sampled), float64(wob)/float64(sampled))
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func scanUsage(ctx context.Context, c cumulite.Port, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 4000 {
		limit = 1000
	}
	var out []map[string]any
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
			if m, _ := d["mode"].(string); m == "" {
				d["mode"] = ""
			}
			out = append(out, d)
		}
	}
	return out, nil
}

func sortedInt(rows []map[string]any, key string) []int64 {
	var xs []int64
	for _, r := range rows {
		if v, ok := toI64(r[key]); ok {
			xs = append(xs, v)
		}
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return xs
}

func toI64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

func toF64(v any) (float64, bool) {
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

// Hypothesis is the one LLM call's output shape.
type Hypothesis struct {
	Action    string `json:"action"`
	Knob      string `json:"knob"`
	Direction string `json:"direction"`
	Anomaly   string `json:"anomaly"`
	Rationale string `json:"rationale"`
}

// ParseHypothesis is exported for frozen prompt regression tests. It enforces
// the iron rules the prompt states: no-action or a whitelisted knob with a
// sane direction — anything else degrades to no-action rather than being
// trusted.
func ParseHypothesis(raw string) Hypothesis {
	h := Hypothesis{Action: "no-action"}
	dec := json.NewDecoder(strings.NewReader(raw))
	var h2 Hypothesis
	if err := dec.Decode(&h2); err != nil {
		return h
	}
	if h2.Action != "tune" {
		return h
	}
	if !Whitelisted(h2.Knob) {
		return h
	}
	if h2.Direction != "up" && h2.Direction != "down" {
		return h
	}
	h2.Action = "tune"
	return h2
}

// Journal is the auditable learning log (clus_learning) — what was tried,
// why, and what happened. Auditability is an iron rule, not a feature.
type Journal struct{ c cumulite.Port }

const journalCollection = "clus_learning"

func NewJournal(c cumulite.Port) *Journal { return &Journal{c: c} }

func (j *Journal) Ensure(ctx context.Context) error {
	return j.c.EnsureCollection(ctx, journalCollection)
}

// Record appends one cycle entry.
func (j *Journal) Record(ctx context.Context, entry map[string]any) error {
	entry["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	_, err := j.c.Insert(ctx, journalCollection, []map[string]any{entry})
	return err
}

// Latest returns the most recent entries (for -dry display and audit).
func (j *Journal) Latest(ctx context.Context, n int) ([]map[string]any, error) {
	rows, err := scanLearning(ctx, j.c, 200)
	if err != nil {
		return nil, err
	}
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	return rows, nil
}

func scanLearning(ctx context.Context, c cumulite.Port, limit int) ([]map[string]any, error) {
	var out []map[string]any
	skip := 0
	for len(out) < limit {
		res, err := c.Query(ctx, journalCollection, contract.Query{Limit: 500, Skip: skip})
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
		out = append(out, res.Documents...)
	}
	return out, nil
}

// BudgetGate: the hard per-cycle token ceiling. A cycle that cannot afford
// its own experiment degrades to no-action — learning never borrows.
const DefaultCycleBudget = 400_000

// BudgetOK reports whether the estimated pair cost fits the cycle budget.
func BudgetOK(estimate, budget int64) bool {
	if budget <= 0 {
		budget = DefaultCycleBudget
	}
	return estimate <= budget
}

// renderKnobs is the whitelist text the prompt consumes.
func RenderKnobs() string {
	var b strings.Builder
	for _, k := range Whitelist {
		fmt.Fprintf(&b, "- %s [%s] %s\n", k.Name, k.Range, k.What)
	}
	return strings.TrimRight(b.String(), "\n")
}

// Env-free file existence check used by the CLI to refuse cycles without a
// guardrail baseline (learning without an alarm wired is not learning).
func GuardrailBaselinePresent(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

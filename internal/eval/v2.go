package eval

// The GUI protocol deliberately does not overwrite rule scores with judge
// verdicts. Legacy ItemScore/RunDoc retain their historical CLI semantics.
import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/willove/cumulus/internal/source"
)

const Protocol = "eval-v2"
const MaxItems = 500
const MaxBytes = 2 << 20
const MaxSnapshotBytes = 16 << 20

// MaxShortReference is the longest reference the rule arm can plausibly match.
// Correct() is a substring / numeric-boundary matcher, so a LENS-style passage
// reference (a whole article) can never appear inside a synthesized answer:
// scoring it would report 0 for every item, which reads as a retrieval failure
// but is really a protocol mismatch. Such a dataset still validates — the judge
// arm and the CLI eval-run protocol are the passage-level instruments — but it
// carries a warning so nobody misreads the rule column.
const MaxShortReference = 200

type Config struct {
	Mode        string `json:"mode"`
	Judge       bool   `json:"judge"`
	ClosedBook  bool   `json:"closed_book"`
	Prior       bool   `json:"prior"`
	L1Pre       bool   `json:"l1pre"`
	ItemTimeout int    `json:"item_timeout_seconds"`
	Timeout     int    `json:"timeout_seconds"`
	TokenBudget int64  `json:"token_budget"`
	Limit       int    `json:"limit"`
}

func DefaultConfig() Config {
	return Config{Mode: "offline", Prior: true, ItemTimeout: 120, Timeout: 1800, TokenBudget: 100000}
}
func (c Config) Validate(live, l1 bool) error {
	if c.Mode != "offline" && c.Mode != "live" {
		return fmt.Errorf("mode must be offline or live")
	}
	if c.Mode == "live" && !live {
		return fmt.Errorf("live model unavailable; configure the server or select offline")
	}
	if c.Mode == "offline" && (c.Judge || c.ClosedBook) {
		return fmt.Errorf("judge and closed_book require live mode")
	}
	if c.L1Pre && !l1 {
		return fmt.Errorf("l1pre unavailable for evaluation")
	}
	if c.ItemTimeout < 1 || c.ItemTimeout > 600 || c.Timeout < 1 || c.Timeout > 7200 || c.TokenBudget < 1 || c.TokenBudget > 10000000 || c.Limit < 0 || c.Limit > MaxItems {
		return fmt.Errorf("invalid limits: item timeout 1..600, timeout 1..7200, token budget 1..10000000, limit 0..500")
	}
	return nil
}

type Issue struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}
type Validation struct {
	Valid    bool    `json:"valid"`
	Items    []Item  `json:"items"`
	Errors   []Issue `json:"errors"`
	Warnings []Issue `json:"warnings"`
	SHA      string  `json:"sha"`
}

func ValidateDataset(content string, corpus []source.Source) Validation {
	v := Validation{Items: []Item{}, Errors: []Issue{}, Warnings: []Issue{}, SHA: HashBytes([]byte(content))}
	add := func(line int, msg string) { v.Errors = append(v.Errors, Issue{line, msg}) }
	if len(content) > MaxBytes {
		add(0, "dataset exceeds 2097152 bytes")
		return v
	}
	keys := map[string]bool{}
	for _, s := range corpus {
		if s.Status == source.StatusActive {
			keys[s.ID] = true
			if s.BusinessKey != "" {
				keys[s.BusinessKey] = true
			}
		}
	}
	seen := map[string]bool{}
	lines := 0
	longest, longestLine, longCount := 0, 0, 0
	for n, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines++
		if lines > MaxItems {
			add(n+1, "dataset exceeds 500 items")
			break
		}
		var it Item
		if err := json.Unmarshal([]byte(line), &it); err != nil {
			add(n+1, "invalid JSON item: "+err.Error())
			continue
		}
		if strings.TrimSpace(it.ID) == "" || strings.TrimSpace(it.Query) == "" || strings.TrimSpace(it.Answer) == "" {
			add(n+1, "id, query and answer are required")
		}
		if seen[it.ID] {
			add(n+1, "duplicate id: "+it.ID)
		}
		seen[it.ID] = true
		if len(it.Gold) == 0 {
			add(n+1, "gold_sources is required for full scoring")
		}
		for _, g := range it.Gold {
			if !keys[g] {
				add(n+1, "gold source does not resolve to an active source: "+g)
			}
		}
		if it.Gold == nil {
			it.Gold = []string{}
		}
		if runes := utf8.RuneCountInString(strings.TrimSpace(it.Answer)); runes > MaxShortReference {
			longCount++
			if runes > longest {
				longest, longestLine = runes, n+1
			}
		}
		v.Items = append(v.Items, it)
	}
	if len(v.Items) == 0 {
		add(0, "dataset contains no items")
	}
	// One aggregated warning: 500 passage items must not produce 500 lines in the
	// wizard. The rule column would read 0 on every one of them.
	if longCount > 0 {
		v.Warnings = append(v.Warnings, Issue{longestLine, fmt.Sprintf("answer is a passage (%d items, longest %d characters): the rule arm matches a short reference by substring, so a passage reference never matches; use a short reference, or read the judge arm / CLI eval-run for passage-level metrics", longCount, longest)})
	}
	v.Valid = len(v.Errors) == 0
	return v
}

type Dataset struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	SHA       string    `json:"sha"`
	Count     int       `json:"count"`
	CreatedAt time.Time `json:"created_at"`
	Items     []Item    `json:"items,omitempty"`
}
type Citation struct {
	SourceID string `json:"source_id"`
	Title    string `json:"title"`
	Quote    string `json:"quote"`
	Resolved bool   `json:"resolved"`
}
type ItemResult struct {
	ID               string       `json:"id"`
	Query            string       `json:"query"`
	Reference        string       `json:"reference"`
	Gold             []string     `json:"gold_sources"`
	Answer           string       `json:"answer"`
	ClosedBookAnswer string       `json:"closed_book_answer"`
	State            string       `json:"state"`
	Error            string       `json:"error"`
	ErrorStage       string       `json:"error_stage"`
	Attempt          int          `json:"attempt"`
	RuleMatch        bool         `json:"rule_match"`
	EvidenceHit      bool         `json:"evidence_hit"`
	CitationResolved bool         `json:"citation_resolved"`
	JudgeCorrect     *bool        `json:"judge_correct"`
	JudgeReason      string       `json:"judge_reason"`
	JudgeError       string       `json:"judge_error"`
	ClosedBookMatch  *bool        `json:"closed_book_match"`
	Mode             string       `json:"mode"`
	LatencyMS        int64        `json:"latency_ms"`
	SearchTokens     int64        `json:"search_tokens"`
	JudgeTokens      int64        `json:"judge_tokens"`
	ClosedBookTokens int64        `json:"closed_book_tokens"`
	CostIncomplete   bool         `json:"cost_incomplete,omitempty"`
	Citations        []Citation   `json:"citations"`
	Attempts         []ItemResult `json:"attempts,omitempty"`
}
type Summary struct {
	N                int      `json:"n"`
	RuleMatch        *float64 `json:"rule_match"`
	EvidenceHit      *float64 `json:"evidence_hit"`
	CitationResolved *float64 `json:"citation_resolved"`
	JudgeCorrect     *float64 `json:"judge_correct"`
	JudgeN           int      `json:"judge_n"`
	ClosedBookMatch  *float64 `json:"closed_book_match"`
	SearchTokens     int64    `json:"search_tokens"`
	JudgeTokens      int64    `json:"judge_tokens"`
	ClosedBookTokens int64    `json:"closed_book_tokens"`
	LatencyMS        int64    `json:"latency_ms"`
}

func Summarize(items []ItemResult) Summary {
	s := Summary{N: len(items)}
	var rule, ev, cite, judge, cb, cbN int
	cost := func(r ItemResult) {
		s.SearchTokens += r.SearchTokens
		s.JudgeTokens += r.JudgeTokens
		s.ClosedBookTokens += r.ClosedBookTokens
		s.LatencyMS += r.LatencyMS
	}
	for _, r := range items {
		cost(r)
		for _, a := range r.Attempts {
			cost(a)
		}
		if r.RuleMatch {
			rule++
		}
		if r.EvidenceHit {
			ev++
		}
		if r.CitationResolved {
			cite++
		}
		if r.JudgeCorrect != nil && r.JudgeError == "" {
			s.JudgeN++
			if *r.JudgeCorrect {
				judge++
			}
		}
		if r.ClosedBookMatch != nil {
			cbN++
			if *r.ClosedBookMatch {
				cb++
			}
		}
	}
	s.RuleMatch = ratio(rule, s.N)
	s.EvidenceHit = ratio(ev, s.N)
	s.CitationResolved = ratio(cite, s.N)
	s.JudgeCorrect = ratio(judge, s.JudgeN)
	s.ClosedBookMatch = ratio(cb, cbN)
	return s
}
func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	v := float64(n) / float64(d)
	return &v
}
func (s Summary) Tokens() int64 { return s.SearchTokens + s.JudgeTokens + s.ClosedBookTokens }

type Run struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	DatasetID   string    `json:"dataset_id"`
	DatasetName string    `json:"dataset_name"`
	Protocol    string    `json:"protocol"`
	State       string    `json:"state"`
	Total       int       `json:"total"`
	Done        int       `json:"done"`
	Failed      int       `json:"failed"`
	CurrentItem string    `json:"current_item"`
	Error       string    `json:"error"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Config      Config    `json:"config"`
	ConfigText  string    `json:"config_text"`
	Frozen      Frozen    `json:"frozen"`
	Summary     Summary   `json:"summary"`
}

func Active(state string) bool {
	return state == "queued" || state == "running" || state == "cancelling"
}

// SafeCSV guards formula injection even behind whitespace or control characters.
func SafeCSV(s string) string {
	t := strings.TrimLeftFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	if t != "" && strings.ContainsRune("=+-@", rune(t[0])) {
		return "'" + s
	}
	return s
}

type ItemChange struct {
	ID           string `json:"id"`
	Query        string `json:"query"`
	LeftCorrect  bool   `json:"left_correct"`
	RightCorrect bool   `json:"right_correct"`
	Change       string `json:"change"`
}
type Comparison struct {
	Comparable        bool                `json:"comparable"`
	Reasons           []string            `json:"reasons"`
	Left              Run                 `json:"left"`
	Right             Run                 `json:"right"`
	Deltas            map[string]*float64 `json:"deltas"`
	Items             []ItemChange        `json:"items"`
	ConfigDifferences map[string][2]any   `json:"config_differences"`
}

func CompareRuns(l, r Run, li, ri []ItemResult) Comparison {
	c := Comparison{Left: l, Right: r, Reasons: []string{}, Items: []ItemChange{}, Deltas: map[string]*float64{}, ConfigDifferences: map[string][2]any{}}
	for _, k := range []string{"rule_match", "evidence_hit", "citation_resolved", "latency_ms", "search_tokens"} {
		c.Deltas[k] = nil
	}
	add := func(s string) { c.Reasons = append(c.Reasons, s) }
	if l.State != "completed" || r.State != "completed" || l.Failed > 0 || r.Failed > 0 {
		add("both runs must be completed without execution failures")
	}
	if l.Protocol != Protocol || r.Protocol != Protocol {
		add("protocol differs")
	}
	if l.Frozen.ItemsSHA != r.Frozen.ItemsSHA {
		add("question datasets differ")
	}
	if l.Frozen.CorpusSHA != r.Frozen.CorpusSHA {
		add("frozen corpora differ")
	}
	if l.Config.Mode != r.Config.Mode || l.Config.Judge != r.Config.Judge || l.Config.ClosedBook != r.Config.ClosedBook {
		add("scoring modes differ")
	}
	lm, rm := map[string]any{}, map[string]any{}
	lb, _ := json.Marshal(l.Config)
	rb, _ := json.Marshal(r.Config)
	_ = json.Unmarshal(lb, &lm)
	_ = json.Unmarshal(rb, &rm)
	for k, v := range lm {
		if v != rm[k] {
			c.ConfigDifferences[k] = [2]any{v, rm[k]}
		}
	}
	// Runtime/model differences are visible independently of retrieval flags.
	// When judging or running a baseline, changing that model also changes the
	// scoring instrument and must not be presented as an apples-to-apples run.
	settings := func(text string) map[string]string {
		out := map[string]string{}
		for _, field := range strings.Split(text, ";") {
			if k, v, ok := strings.Cut(field, "="); ok && k != "config" {
				out[k] = v
			}
		}
		return out
	}
	lr, rr := settings(l.ConfigText), settings(r.ConfigText)
	for k, v := range lr {
		if v != rr[k] {
			c.ConfigDifferences["runtime."+k] = [2]any{v, rr[k]}
		}
	}
	for k, v := range rr {
		if _, ok := lr[k]; !ok {
			c.ConfigDifferences["runtime."+k] = [2]any{nil, v}
		}
	}
	if (l.Config.Judge || l.Config.ClosedBook) && (lr["model"] != rr["model"] || lr["endpoint"] != rr["endpoint"]) {
		add("judge or closed-book model identity differs")
	}
	index := map[string]ItemResult{}
	for _, it := range ri {
		index[it.ID] = it
	}
	if len(li) != len(ri) || len(li) != l.Total || len(ri) != r.Total {
		add("item sets are incomplete or different")
	}
	for _, a := range li {
		b, ok := index[a.ID]
		if !ok || a.Query != b.Query || a.Reference != b.Reference {
			add("question contents differ: " + a.ID)
		}
	}
	if len(c.Reasons) > 0 {
		return c
	}
	c.Comparable = true
	delta := func(a, b *float64) *float64 {
		if a == nil || b == nil {
			return nil
		}
		d := *b - *a
		return &d
	}
	c.Deltas["rule_match"] = delta(l.Summary.RuleMatch, r.Summary.RuleMatch)
	c.Deltas["evidence_hit"] = delta(l.Summary.EvidenceHit, r.Summary.EvidenceHit)
	c.Deltas["citation_resolved"] = delta(l.Summary.CitationResolved, r.Summary.CitationResolved)
	latency := float64(r.Summary.LatencyMS - l.Summary.LatencyMS)
	tokens := float64(r.Summary.SearchTokens - l.Summary.SearchTokens)
	c.Deltas["latency_ms"] = &latency
	c.Deltas["search_tokens"] = &tokens
	for _, a := range li {
		b := index[a.ID]
		change := "unchanged"
		if !a.RuleMatch && b.RuleMatch {
			change = "improved"
		}
		if a.RuleMatch && !b.RuleMatch {
			change = "regressed"
		}
		c.Items = append(c.Items, ItemChange{a.ID, a.Query, a.RuleMatch, b.RuleMatch, change})
	}
	return c
}

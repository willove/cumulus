package eval

// Run persistence (B3): the scoreboard reads eval runs from the store, so a
// run survives the CLI process and the workbench can list/detail it. The
// per-item JSONL stays the operator's artifact; the store keeps the
// comparable aggregate (the scoreboard never needs the raw lines).

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/willove/cumulite"
	"github.com/willove/cumulite/contract"
	"github.com/willove/cumulus/internal/storedoc"
)

// RunDoc is one persisted eval run.
type RunDoc struct {
	ID     string `json:"_id"` // run:<unixmilli>
	Tag    string `json:"tag"`
	At     string `json:"at"`
	N      int    `json:"n"`
	Judged bool   `json:"judged"`

	System     Report         `json:"system"`
	ClosedBook Report         `json:"closed_book"`
	McNemar    McNemar        `json:"mcnemar"`
	Modes      map[string]int `json:"modes"`

	SearchTokens      int64 `json:"search_tokens"`
	JudgeTokens       int64 `json:"judge_tokens"`
	RejectedProposals int   `json:"rejected_proposals"`

	// Frozen binds the row to the exact artifacts it ran on (A.6). It is
	// TYPED, not buried in Extra: an unreadable binding is the same as no
	// binding, and the whole point is that a reader can tell whether two rows
	// are comparable. The raw config string is carried alongside the hash so
	// a human can see WHICH knob differs.
	Frozen *Frozen `json:"frozen,omitempty"`
	// ConfigText is the human-readable config the ConfigSHA was taken over.
	ConfigText string `json:"config_text,omitempty"`

	// Extra carries the cmd-level breakdowns (nr_breakdown) opaque to this
	// package — the scoreboard renders what it understands.
	Extra map[string]any `json:"extra,omitempty"`
}

// CumuStore persists eval runs in clus_evals (namespace-scoped like every
// other suite collection).
type CumuStore struct {
	c    cumulite.Port
	coll string
}

func NewCumuStore(c cumulite.Port, coll string) *CumuStore {
	if coll == "" {
		coll = "clus_evals"
	}
	storedoc.DeclareShape(context.Background(), c, coll, RunDoc{})
	return &CumuStore{c: c, coll: coll}
}

// SaveRun upserts one run by its id.
func (s *CumuStore) SaveRun(ctx context.Context, d RunDoc) error {
	if d.ID == "" {
		// run:<unixmilli> is not unique on its own: two runs inside the same
		// millisecond collided and silently replaced each other's scoreboard
		// row (GetDocument → ReplaceDocument). Suffix until free.
		d.ID = "run:" + fmt.Sprint(time.Now().UnixMilli())
		for n := 1; ; n++ {
			if existing, err := s.c.GetDocument(ctx, s.coll, d.ID); err != nil || existing == nil {
				break
			}
			d.ID = fmt.Sprintf("run:%d-%d", time.Now().UnixMilli(), n)
		}
	}
	if d.At == "" {
		d.At = time.Now().UTC().Format(time.RFC3339)
	}
	// Typed write: RunDoc's json tags ARE the document. The hand-maintained
	// table this replaces is where a new RunDoc field would have vanished
	// silently — the scoreboard's own shape must not drift from the type.
	exists := false
	if existing, err := s.c.GetDocument(ctx, s.coll, d.ID); err == nil && existing != nil {
		exists = true
	}
	return storedoc.WriteStruct(ctx, s.c, s.coll, d.ID, d, exists)
}

// The documented per-library budget for runs is 200 (README: 每个库的运行与
// 题集各上限 200 条). listRunsDefault is what a caller that passed no usable
// limit gets; listRunsMax is the ceiling an over-eager limit is clamped to.
const (
	listRunsDefault = 50
	listRunsMax     = 200
)

// ListRuns returns the newest runs first (bounded).
//
// The 200 cap is real, so a caller asking beyond it must not silently receive
// a QUARTER of what it asked for. It used to collapse every limit > 200 — and
// every non-positive limit — to 50, so `ListRuns(500)` returned 50 rows with no
// error, no log and no way for the caller to tell it had been shortchanged.
// Clamp to the documented ceiling instead: the caller gets as much as the
// budget allows, and a short result is then a true signal that the BUDGET
// (not the caller's request) is the limit.
func (s *CumuStore) ListRuns(ctx context.Context, limit int) ([]RunDoc, error) {
	if limit <= 0 {
		limit = listRunsDefault // "caller did not say" — keep the old default
	}
	if limit > listRunsMax {
		limit = listRunsMax
	}
	// Read a full ceiling's worth regardless of the clamp: the sort/pagination
	// below walks the result, and the old code hard-coded 500 while the
	// effective limit could be 50. Reading 200 is both sufficient and honest.
	res, err := s.c.Query(ctx, s.coll, contract.Query{Limit: listRunsMax})
	if err != nil {
		return nil, err
	}
	out := make([]RunDoc, 0, len(res.Documents))
	for _, d := range res.Documents {
		out = append(out, docToRun(d))
	}
	// Newest first by id (run:<unixmilli> sorts lexicographically in time order).
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetRun returns one run or nil (a missing run is not an error — the
// scoreboard turns it into a 404).
func (s *CumuStore) GetRun(ctx context.Context, id string) (*RunDoc, error) {
	d, err := s.c.GetDocument(ctx, s.coll, id)
	if err != nil {
		if contract.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if d == nil {
		return nil, nil
	}
	r := docToRun(d)
	return &r, nil
}

func docToRun(d map[string]any) RunDoc {
	r := RunDoc{
		ID:     str(d["_id"]),
		Tag:    str(d["tag"]),
		At:     str(d["at"]),
		N:      intf(d["n"]),
		Judged: boolf(d["judged"]),
	}
	if m, ok := d["system"].(map[string]any); ok {
		r.System = docToReport(m)
	}
	if m, ok := d["closed_book"].(map[string]any); ok {
		r.ClosedBook = docToReport(m)
	}
	if m, ok := d["mcnemar"].(map[string]any); ok {
		r.McNemar = McNemar{BOnly: intf(m["b_only"]), COnly: intf(m["c_only"]), P: floatf(m["p"]), N: intf(m["n"])}
	}
	if m, ok := d["modes"].(map[string]any); ok {
		r.Modes = map[string]int{}
		for k, v := range m {
			r.Modes[k] = intf(v)
		}
	}
	r.SearchTokens = int64f(d["search_tokens"])
	r.JudgeTokens = int64f(d["judge_tokens"])
	r.RejectedProposals = intf(d["rejected_proposals"])
	// A.6 binding, read back as data rather than left in Extra: a reader must be
	// able to tell whether two rows are comparable, and "which knob differs"
	// needs the config text, not just its hash.
	if m, ok := d["frozen"].(map[string]any); ok {
		r.Frozen = &Frozen{
			ItemsSHA:  str(m["items_sha"]),
			CorpusSHA: str(m["corpus_sha"]),
			ConfigSHA: str(m["config_sha"]),
			OrderSeed: intf(m["order_seed"]),
		}
	}
	r.ConfigText = str(d["config_text"])
	if m, ok := d["extra"].(map[string]any); ok {
		r.Extra = m
	}
	return r
}

func docToReport(m map[string]any) Report {
	r := Report{
		N:      intf(m["n"]),
		EM:     floatf(m["em"]),
		EvRec:  floatf(m["ev_rec"]),
		Ground: floatf(m["ground"]),
	}
	if tx, ok := m["taxonomy"].(map[string]any); ok {
		r.Taxonomy = Taxonomy{
			Correct:       intf(tx["correct"]),
			RetrievedOnly: intf(tx["retrieved_but_unanswered"]),
			AnsweredWrong: intf(tx["answered_but_wrong"]),
			NotRetrieved:  intf(tx["not_retrieved"]),
		}
	}
	return r
}

func str(v any) string { s, _ := v.(string); return s }
func boolf(v any) bool { b, _ := v.(bool); return b }
func intf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}
func int64f(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
}
func floatf(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

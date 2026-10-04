// conflicts — 簇间冲突：存储接口、内存实现、检测与查询。
// 从 deep.go 纯搬运（2026-10-04 拆分），无语义改动。
package deep

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/willove/cumulus/internal/cluster"
)

// Conflict is a contested link between two clusters (clus_conflicts).
type Conflict struct {
	ID     string    `json:"_id"`
	A      string    `json:"a"`
	B      string    `json:"b"`
	Group  string    `json:"group"`
	Reason string    `json:"reason"`
	Saved  time.Time `json:"saved"` // write timestamp (diagnostic read faces surface it)
}

// ConflictStore persists conflict edges.
type ConflictStore interface {
	Save(ctx context.Context, c Conflict) error
	Between(ctx context.Context, a, b string) ([]Conflict, error)
	All(ctx context.Context) ([]Conflict, error)
}

// MemoryConflict is an in-memory ConflictStore.
type MemoryConflict struct{ m map[string]Conflict }

func NewMemoryConflict() *MemoryConflict { return &MemoryConflict{m: map[string]Conflict{}} }

func (s *MemoryConflict) Save(_ context.Context, c Conflict) error {
	if c.ID == "" {
		c.ID = "x:" + c.A + "-" + c.B + "-" + c.Group
	}
	s.m[c.ID] = c
	return nil
}

func (s *MemoryConflict) Between(_ context.Context, a, b string) ([]Conflict, error) {
	var out []Conflict
	for _, c := range s.m {
		if (c.A == a && c.B == b) || (c.A == b && c.B == a) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *MemoryConflict) All(_ context.Context) ([]Conflict, error) {
	out := make([]Conflict, 0, len(s.m))
	for _, c := range s.m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (e *Engine) conflictsFor(ctx context.Context, id string) []Conflict {
	if e.Conflicts == nil || id == "" {
		return nil
	}
	all, err := e.Conflicts.All(ctx)
	if err != nil {
		return nil
	}
	var out []Conflict
	for _, c := range all {
		if c.A == id || c.B == id {
			out = append(out, c)
		}
	}
	return out
}

// DetectConflict records a contested pair (content disagreement proxy: two
// clusters with different source_ids and overlapping queries but divergent
// numeric claims in content — offline heuristic).
func DetectConflict(ctx context.Context, st ConflictStore, a, b cluster.Cluster) (Conflict, error) {
	if a.ID == "" || b.ID == "" || a.ID == b.ID {
		return Conflict{}, fmt.Errorf("deep: need two distinct clusters")
	}
	claimA := claimOf(a)
	claimB := claimOf(b)
	if claimA == "" || claimB == "" || claimA == claimB {
		return Conflict{}, fmt.Errorf("deep: no divergent numeric claims")
	}
	from, to := a.ID, b.ID
	if from > to {
		from, to = to, from
	}
	c := Conflict{
		A: from, B: to, Group: "claim:" + claimA + "_vs_" + claimB,
		Reason: fmt.Sprintf("divergent claims: %s vs %s", claimA, claimB),
	}
	if err := st.Save(ctx, c); err != nil {
		return Conflict{}, err
	}
	// Lifecycle: contested.
	return c, nil
}

// claimOf pulls the divergent-claim number from the evidence windows first
// (raw source text), falling back to the rendered summary — summary offsets
// like "[0,21)" are not claims and must not win.
func claimOf(c cluster.Cluster) string {
	return cluster.NumericClaim(c)
}

// Package cluster is the self-evolving knowledge layer (L2). A cluster is a
// reusable cognitive unit produced by a completed search. Identity is stable
// under paraphrase (topic key + merge), never a hash of LLM prose — Sirchmunk's
// sha256(content) fractures one topic into many clusters.
package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cumubase/ask/internal/mcs"
	"github.com/cumubase/ask/internal/source"
)

const (
	LifecycleEmerging   = "emerging"
	LifecycleStable     = "stable"
	LifecycleContested  = "contested"
	LifecycleDeprecated = "deprecated"
)

const (
	// MaxQueriesPerCluster is the FIFO cap on retained queries (Sirchmunk 5).
	MaxQueriesPerCluster = 5
	// DefaultSplitCap is G-id: at most this many clusters for one topic key
	// under paraphrase, before merge is forced.
	DefaultSplitCap = 1
)

// Embedder turns text into a vector for reuse search. Offline Local stub is
// deterministic; production wires aigate behind the same interface.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float64, error)
	Dims() int
}

// Local is a deterministic offline hash embedder (not semantic — gate carrier).
type Local struct{ N int }

// Dims implements Embedder.
func (l Local) Dims() int {
	if l.N <= 0 {
		return 64
	}
	return l.N
}

// Embed implements Embedder.
func (l Local) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, 0, len(texts))
	for _, t := range texts {
		out = append(out, hashVec(t, l.Dims()))
	}
	return out, nil
}

func hashVec(text string, dims int) []float64 {
	v := make([]float64, dims)
	// Unigram fingerprint — paraphrase- and reorder-stable under TopicKey.
	for _, tok := range Unigrams(text) {
		sum := sha256.Sum256([]byte(tok))
		idx := int(uint32(sum[0])<<24|uint32(sum[1])<<16|uint32(sum[2])<<8|uint32(sum[3])) % dims
		if idx < 0 {
			idx = -idx
		}
		v[idx] += 1
	}
	var n float64
	for _, x := range v {
		n += x * x
	}
	if n > 0 {
		n = sqrt(n)
		for i := range v {
			v[i] /= n
		}
	}
	return v
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Newton's method, few iterations — avoids importing math just for this.
	r := x
	for i := 0; i < 12; i++ {
		r = 0.5 * (r + x/r)
	}
	return r
}

// Cosine returns similarity in [-1,1].
func Cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt(na) * sqrt(nb))
}

// interrogative tails that must not split one topic ("是多少" vs "是多大").
var interrogativeRe = regexp.MustCompile(`(是多少|是多大|是多小|是啥|是什么|怎么样|如何|怎么|为什么|吗|呢|了|啊|呀)+\s*$`)

// Unigrams is an order-independent fingerprint of a query: CJK chars as single
// runes + latin/number words. Paraphrases and reorders collide here (G-id).
func Unigrams(query string) []string {
	q := strings.TrimSpace(interrogativeRe.ReplaceAllString(query, ""))
	seen := map[string]bool{}
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			w := strings.ToLower(string(cur))
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
		cur = cur[:0]
	}
	for _, r := range q {
		if r >= 0x4e00 && r <= 0x9fff {
			flush()
			s := string(r)
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	sort.Strings(out)
	return out
}

// TopicKey is the stable identity of a question's intent.
func TopicKey(query string) string {
	joined := strings.Join(Unigrams(query), "|")
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:8])
}

// Cluster is one knowledge unit in ask_clusters.
type Cluster struct {
	ID         string          `json:"_id"`
	TopicKey   string          `json:"topic_key"`
	TopicKeys  []string        `json:"topic_keys,omitempty"` // aliases from folded clusters
	Name       string          `json:"name"`
	Content    string          `json:"content"`
	Queries    []string        `json:"queries"`
	Embed      []float64       `json:"embed"`
	Confidence float64         `json:"confidence"`
	Hotness    float64         `json:"hotness"`
	Lifecycle  string          `json:"lifecycle"`
	Version    int             `json:"version"`
	SourceID   string          `json:"source_id"`
	Evidence   []mcs.Sample    `json:"evidence"`
	Flags      map[string]bool `json:"flags,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// New builds a v1 cluster from a finished FAST/DEEP answer.
func New(topicKey, name, content, query, sourceID string, evidence []mcs.Sample, embed []float64, confidence float64) Cluster {
	now := time.Now().UTC()
	tk := topicKey
	if len(tk) > 8 {
		tk = tk[:8]
	}
	id := "C" + tk
	return Cluster{
		ID:         id,
		TopicKey:   topicKey,
		Name:       name,
		Content:    content,
		Queries:    []string{query},
		Embed:      embed,
		Confidence: confidence,
		Hotness:    0.5,
		Lifecycle:  LifecycleEmerging,
		Version:    1,
		SourceID:   sourceID,
		Evidence:   NormalizeEvidence(sourceID, evidence),
		Flags:      map[string]bool{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// NormalizeEvidence copies samples and replaces sampling-method source labels
// with sourceID. Explicit document IDs and all other sample fields are retained.
func NormalizeEvidence(sourceID string, samples []mcs.Sample) []mcs.Sample {
	out := append([]mcs.Sample(nil), samples...)
	for i := range out {
		switch out[i].Source {
		case "", "full", "stratified", "fuzz", "gaussian", "global":
			out[i].Source = sourceID
		}
	}
	return out
}

// Evolve merges a new successful query into the cluster (G-idem: same query is
// a no-op on the query list; hotness caps at 1.0).
func (c *Cluster) Evolve(query string, embed []float64) bool {
	changed := false
	if !containsString(c.Queries, query) {
		c.Queries = append(c.Queries, query)
		if len(c.Queries) > MaxQueriesPerCluster {
			c.Queries = c.Queries[len(c.Queries)-MaxQueriesPerCluster:]
		}
		changed = true
	}
	if c.Hotness < 1.0 {
		c.Hotness += 0.1
		if c.Hotness > 1.0 {
			c.Hotness = 1.0
		}
		changed = true
	}
	// Query-driven embedding: recompute from the retained query set.
	if embed != nil {
		c.Embed = embed
		changed = true
	}
	c.Version++
	c.UpdatedAt = time.Now().UTC()
	return changed
}

// ShouldReuse reports whether query embedding is close enough to reuse
// (G-pollute callers must still check answer-question relevance).
func ShouldReuse(c *Cluster, queryEmbed []float64, theta float64) bool {
	if c.Lifecycle == LifecycleDeprecated {
		return false
	}
	return Cosine(c.Embed, queryEmbed) >= theta
}

// CanMerge reports whether a new hit should fold into c rather than spawn a
// sibling (G-merge).
func CanMerge(c *Cluster, queryEmbed []float64, mergeTheta float64) bool {
	if c.Lifecycle == LifecycleDeprecated {
		return false
	}
	return Cosine(c.Embed, queryEmbed) >= mergeTheta
}

func containsString(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Store is the cluster persistence facade (cumudb-backed in production; the
// in-memory map keeps unit tests free of the engine).
type Store interface {
	Save(ctx context.Context, c Cluster) error
	Get(ctx context.Context, id string) (*Cluster, error)
	FindByTopic(ctx context.Context, topicKey string) ([]Cluster, error)
	All(ctx context.Context) ([]Cluster, error)
	Delete(ctx context.Context, id string) error
}

// Memory is an in-memory Store for gates and unit tests.
type Memory struct{ m map[string]Cluster }

func NewMemory() *Memory { return &Memory{m: map[string]Cluster{}} }

func (s *Memory) Save(_ context.Context, c Cluster) error {
	s.m[c.ID] = c
	return nil
}

func (s *Memory) Get(_ context.Context, id string) (*Cluster, error) {
	c, ok := s.m[id]
	if !ok {
		return nil, nil
	}
	cp := c
	return &cp, nil
}

func (s *Memory) FindByTopic(_ context.Context, topicKey string) ([]Cluster, error) {
	var out []Cluster
	for _, c := range s.m {
		if c.TopicKey == topicKey || containsString(c.TopicKeys, topicKey) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Memory) All(_ context.Context) ([]Cluster, error) {
	out := make([]Cluster, 0, len(s.m))
	for _, c := range s.m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Memory) Delete(_ context.Context, id string) error {
	delete(s.m, id)
	return nil
}

// SplitCap returns at most cap clusters for a topic (G-id helper).
func SplitCap(clusters []Cluster, topicKey string, cap int) []Cluster {
	if cap <= 0 {
		cap = DefaultSplitCap
	}
	var out []Cluster
	for _, c := range clusters {
		if c.TopicKey != topicKey && !containsString(c.TopicKeys, topicKey) {
			continue
		}
		out = append(out, c)
		if len(out) >= cap {
			break
		}
	}
	return out
}

// RelevanceGate is the G-pollute check: does this cluster actually answer the
// question? Offline stub = field overlap with cluster content; production
// goes through aigate.
func RelevanceGate(query string, c Cluster, minOverlap float64) bool {
	if minOverlap <= 0 {
		minOverlap = 0.3
	}
	return mcs.Coverage(query, []mcs.Sample{{Content: c.Content + " " + strings.Join(c.Queries, " ")}}) >= minOverlap
}

// Ensure unused import of source stays meaningful for future cites typing.
var _ = source.StatusActive

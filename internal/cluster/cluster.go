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

// Level names for multi-abstraction retrieval keys (Self-Index D.6): one
// evidence unit carries keys at several abstraction levels so a rephrased
// query can hit any of them via max-over-keys — never one vector for the
// whole cluster.
const (
	KeyAnchor    = "anchor"    // 原文锚点短语 (fixed with the evidence span)
	KeyScenario  = "scenario"  // 场景问句
	KeyPrinciple = "principle" // 原理句
	KeyOp        = "op"        // 操作短语
)

// LevelKey is one retrieval key at a named abstraction level. Evidence
// 原文 spans are never rewritten into this field — only key text used for
// matching (L0 keeps the body as source of truth).
type LevelKey struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// Cluster is one knowledge unit in ask_clusters.
type Cluster struct {
	ID        string   `json:"_id"`
	TopicKey  string   `json:"topic_key"`
	TopicKeys []string `json:"topic_keys,omitempty"` // aliases from folded clusters
	// LevelKeys are multi-level retrieval keys (D.6). Identity stays TopicKey;
	// these only feed max-over-keys scoring and are droppable (G-drop).
	LevelKeys []LevelKey `json:"level_keys,omitempty"`
	// KeyEmbeds are per-level-key embeddings (MVR-cache 分段 MaxSim, ir-rag
	// 2.5): MaxSim over segments instead of one vector for the whole cluster.
	// Aligned 1:1 with LevelKeys; nil or length mismatch falls back to Embed.
	KeyEmbeds  [][]float64     `json:"key_embeds,omitempty"`
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
	normEv := NormalizeEvidence(sourceID, evidence)
	return Cluster{
		ID:         id,
		TopicKey:   topicKey,
		LevelKeys:  DeriveLevelKeys(name, content, query, normEv),
		Name:       name,
		Content:    content,
		Queries:    []string{query},
		Embed:      embed,
		Confidence: confidence,
		Hotness:    0.5,
		Lifecycle:  LifecycleEmerging,
		Version:    1,
		SourceID:   sourceID,
		Evidence:   normEv,
		Flags:      map[string]bool{},
		CreatedAt:  now,
		UpdatedAt:  now,
	}
}

// DeriveLevelKeys builds the D.6 key set without an LLM: scenario = the
// query, op = interrogative-stripped query, anchor = top evidence snippet,
// principle = content head. Evidence bodies are only read here as key text
// sources — the stored Evidence spans themselves are not rewritten.
func DeriveLevelKeys(name, content, query string, evidence []mcs.Sample) []LevelKey {
	var out []LevelKey
	seen := map[string]bool{}
	add := func(level, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if r := []rune(text); len(r) > 80 {
			text = string(r[:80])
		}
		k := level + "\x00" + text
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, LevelKey{Level: level, Text: text})
	}
	add(KeyScenario, query)
	add(KeyScenario, name)
	if stripped := strings.TrimSpace(interrogativeRe.ReplaceAllString(query, "")); stripped != "" && stripped != query {
		add(KeyOp, stripped)
	}
	for _, ev := range evidence {
		if ev.Content != "" {
			add(KeyAnchor, ev.Content)
			break
		}
	}
	if lines := strings.Split(content, "\n"); len(lines) > 0 {
		add(KeyPrinciple, lines[0])
	}
	return out
}

// AllKeyTexts flattens level keys + retained queries for max-over-keys.
// TopicKey/TopicKeys are identity hashes and are excluded (not lexical).
func (c *Cluster) AllKeyTexts() []string {
	out := make([]string, 0, len(c.LevelKeys)+len(c.Queries))
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, k := range c.LevelKeys {
		add(k.Text)
	}
	for _, q := range c.Queries {
		add(q)
	}
	add(c.Name)
	return out
}

// LevelKeyTexts returns level key texts in stored order (KeyEmbeds alignment).
func (c *Cluster) LevelKeyTexts() []string {
	out := make([]string, 0, len(c.LevelKeys))
	for _, k := range c.LevelKeys {
		out = append(out, k.Text)
	}
	return out
}

// AttachKeyEmbeds stores per-level-key embeddings (MVR-cache segments).
// Mismatched length is ignored — a misaligned map must not silently score.
func (c *Cluster) AttachKeyEmbeds(embeds [][]float64) {
	if len(embeds) == 0 || len(embeds) != len(c.LevelKeys) {
		return
	}
	for _, v := range embeds {
		if len(v) == 0 {
			return
		}
	}
	c.KeyEmbeds = embeds
}

// MaxKeySim is MaxSim over segment embeddings (MVR-cache); 0 when no
// aligned KeyEmbeds exist.
func (c *Cluster) MaxKeySim(queryEmbed []float64) float64 {
	if len(c.KeyEmbeds) != len(c.LevelKeys) || len(queryEmbed) == 0 {
		return 0
	}
	m := 0.0
	for _, ke := range c.KeyEmbeds {
		if v := Cosine(ke, queryEmbed); v > m {
			m = v
		}
	}
	return m
}

// addLevelKey appends a derived key when missing (Evolve / fold). New keys
// invalidate the aligned KeyEmbeds map until the caller recomputes it.
func (c *Cluster) addLevelKey(level, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	for _, k := range c.LevelKeys {
		if k.Level == level && k.Text == text {
			return
		}
	}
	c.LevelKeys = append(c.LevelKeys, LevelKey{Level: level, Text: text})
	c.KeyEmbeds = nil
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
// a no-op on the query list; hotness caps at 1.0). The new wording is also
// recorded as a scenario key so max-over-keys can hit it later even after
// the query FIFO drops it.
func (c *Cluster) Evolve(query string, embed []float64) bool {
	changed := false
	if !containsString(c.Queries, query) {
		c.Queries = append(c.Queries, query)
		if len(c.Queries) > MaxQueriesPerCluster {
			c.Queries = c.Queries[len(c.Queries)-MaxQueriesPerCluster:]
		}
		c.addLevelKey(KeyScenario, query)
		if stripped := strings.TrimSpace(interrogativeRe.ReplaceAllString(query, "")); stripped != "" && stripped != query {
			c.addLevelKey(KeyOp, stripped)
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

// MaxKeyRel is max-over-keys lexical relevance of a query against every
// level key / retained query (Self-Index s = max_k rel). Returned in [0,1].
func MaxKeyRel(query string, c *Cluster) float64 {
	m := 0.0
	for _, text := range c.AllKeyTexts() {
		if r := mcs.Coverage(query, []mcs.Sample{{Content: text}}); r > m {
			m = r
		}
	}
	return m
}

// ReuseScore is max(query-set embed cosine, segment MaxSim, max-over-keys
// lexical). The arms share a [0,1]-usable scale for Local/hash embeds and
// real embedders alike; taking the max avoids fusion weights (D.6 + MVR).
func ReuseScore(c *Cluster, query string, queryEmbed []float64) float64 {
	s := Cosine(c.Embed, queryEmbed)
	if s < 0 {
		s = 0
	}
	if k := MaxKeyRel(query, c); k > s {
		s = k
	}
	if m := c.MaxKeySim(queryEmbed); m > s {
		return m
	}
	return s
}

// ShouldReuse reports whether the query is close enough to reuse under
// max-over-keys (G-pollute callers must still check answer-question
// relevance). Deprecated clusters never reuse.
func ShouldReuse(c *Cluster, query string, queryEmbed []float64, theta float64) bool {
	if c.Lifecycle == LifecycleDeprecated {
		return false
	}
	return ReuseScore(c, query, queryEmbed) >= theta
}

// CanMerge reports whether a new hit should fold into c rather than spawn a
// sibling (G-merge). Uses the same max-over-keys score as reuse so a
// cross-topic near-duplicate with one strong key can still merge.
func CanMerge(c *Cluster, query string, queryEmbed []float64, mergeTheta float64) bool {
	if c.Lifecycle == LifecycleDeprecated {
		return false
	}
	return ReuseScore(c, query, queryEmbed) >= mergeTheta
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

// AcceptFold is the Self-Index validation pair (ir-rag A2, zero-LLM) that must
// pass before loser key material is folded into winner. Unvalidated evolution
// is net-negative (Self-Index ablation: no acceptance < baseline).
//
//	Specificity — each proposed key as a query must retrieve winner in topK.
//	Separation  — each proposed key sits closer to the winner key set than to
//	                any competitor key set.
//
// Additionally rejects **cross-topic** folds whose numeric claims diverge
// (128 vs 256, 96 vs 192): those are conflicts to surface, not duplicates —
// otherwise G1 write-path near-hits pollute conflict fixtures. Same-topic
// folds still proceed: B8 self-heal must replace a stale claim with the
// fresh one (replace path, not a union of both numbers).
// Faithfulness (LLM, 0–3) is endpoint-only (P5), not run here.
func AcceptFold(winner, loser Cluster, competitors []Cluster, topK int) (bool, string) {
	if topK <= 0 {
		topK = 3
	}
	if winner.TopicKey != loser.TopicKey {
		if wa, wb := NumericClaim(winner), NumericClaim(loser); wa != "" && wb != "" && wa != wb {
			return false, "claims: cross-topic divergent claims " + wa + " vs " + wb
		}
	}
	proposed := proposedKeys(loser)
	if len(proposed) == 0 {
		return true, ""
	}
	winKeys := keySet(winner)
	var compKeys [][]string
	corpus := []Cluster{winner}
	for _, c := range competitors {
		if c.ID == winner.ID || c.ID == loser.ID {
			continue
		}
		corpus = append(corpus, c)
		if ks := keySet(c); len(ks) > 0 {
			compKeys = append(compKeys, ks)
		}
	}
	for _, k := range proposed {
		if k == "" {
			continue
		}
		if !specificityOK(k, winner, corpus, topK) {
			return false, "specificity: key would not retrieve winner within topK"
		}
		if !separationOK(k, winKeys, compKeys) {
			return false, "separation: key closer to competitor keys than to winner"
		}
	}
	return true, ""
}

// NumericClaim pulls the first decimal number from evidence windows (raw
// text), falling back to content. Empty when no claim is present — callers
// treat empty as "compatible".
func NumericClaim(c Cluster) string {
	for _, sm := range c.Evidence {
		if n := firstNumber(sm.Content); n != "" {
			return n
		}
	}
	return firstNumber(c.Content)
}

func firstNumber(s string) string {
	var digits []rune
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
			continue
		}
		if len(digits) > 0 {
			return string(digits)
		}
	}
	return string(digits)
}

func proposedKeys(loser Cluster) []string {
	var ks []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			ks = append(ks, s)
		}
	}
	add(loser.TopicKey)
	for _, k := range loser.TopicKeys {
		add(k)
	}
	for _, q := range loser.Queries {
		add(q)
	}
	for _, lk := range loser.LevelKeys {
		add(lk.Text)
	}
	return ks
}

func keySet(c Cluster) []string {
	var ks []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			ks = append(ks, s)
		}
	}
	add(c.TopicKey)
	for _, k := range c.TopicKeys {
		add(k)
	}
	for _, q := range c.Queries {
		add(q)
	}
	for _, lk := range c.LevelKeys {
		add(lk.Text)
	}
	return ks
}

func specificityOK(key string, winner Cluster, corpus []Cluster, topK int) bool {
	type scored struct {
		id  string
		rel float64
	}
	rank := make([]scored, 0, len(corpus))
	for _, c := range corpus {
		rank = append(rank, scored{id: c.ID, rel: keyRel(key, c)})
	}
	sort.Slice(rank, func(i, j int) bool {
		if rank[i].rel != rank[j].rel {
			return rank[i].rel > rank[j].rel
		}
		return rank[i].id < rank[j].id
	})
	for i, r := range rank {
		if r.id == winner.ID {
			return i < topK
		}
	}
	return false
}

func separationOK(key string, winKeys []string, compKeys [][]string) bool {
	win := maxKeyRel(key, winKeys)
	for _, set := range compKeys {
		if maxKeyRel(key, set) > win {
			return false
		}
	}
	return true
}

func maxKeyRel(key string, keys []string) float64 {
	m := 0.0
	for _, k := range keys {
		if r := keyRel(key, Cluster{Content: k, Queries: []string{k}}); r > m {
			m = r
		}
	}
	return m
}

// keyRel is offline relevance of a key against a cluster's surface text
// (token coverage). Production may swap embedder-based rel; gates use this.
func keyRel(key string, c Cluster) float64 {
	text := c.Content + " " + c.Name + " " + strings.Join(c.Queries, " ") + " " + c.TopicKey + " " + strings.Join(c.TopicKeys, " ")
	for _, lk := range c.LevelKeys {
		text += " " + lk.Text
	}
	return mcs.Coverage(key, []mcs.Sample{{Content: text}})
}

// Ensure unused import of source stays meaningful for future cites typing.
var _ = source.StatusActive

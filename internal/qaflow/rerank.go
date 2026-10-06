package qaflow

import (
	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/retrieval"
)

// KeyEmbedder 是向量面的挂点。和 KeyBelief 一样：绑了才可用，没绑就是
// 没绑——语义层不许静默缺席（cumulus 被 hash-64 假向量打乱 BM25 序的
// 败局，根因就是缺席不可见）。
//
// 注意：这是接口类型的 key。`context.Set(c, KeyEmbedder, 具体实现)` 会
// 被 Go 泛型推断卡住（T 从实参推断成具体类型，与 Key[embed.Embedder]
// 不一致），调用方要显式写 `context.Set[embed.Embedder](...)`。
// 指针具体型的 key（KeyBelief）没这个问题。
var KeyEmbedder = context.NewKey[embed.Embedder]("embed.embedder")

// KeyRerank 记录重排这一步的实际状态：applied（真的重排了）还是
// skipped（为什么没排）。降级必须留痕——degraded 可以，silent 不行。
var KeyRerank = context.NewKey[RerankState]("evidence.rerank")

// RerankState 是重排审计记录。
type RerankState struct {
	Applied bool
	Reason  string // skipped 的原因；applied 时为空
}

// SemanticRerank 是语义重排组件：只要求 embed.embedder 绑着。绑了，
// 检索结果按段落级语义相似重排；撤了，组件停用、BM25 序原样恢复。
type SemanticRerank struct {
	active bool
}

func (SemanticRerank) Name() string       { return "semantic-rerank" }
func (SemanticRerank) Requires() []string { return []string{KeyEmbedder.String()} }

func (s *SemanticRerank) Activate(c *context.Context) error {
	s.active = true
	return nil
}

func (s *SemanticRerank) Deactivate(c *context.Context) error {
	s.active = false
	return nil
}

// Active 供测试与 status 面查询。
func (s *SemanticRerank) Active() bool { return s.active }

// rerankHits 在 BM25 命中之上按语义重排。三条纪律：
//   - 只比前 maxCompare 条（MiniLM 的甜蜜点是小集合高区分）；
//   - embedder 缺席 / 调用失败 / 没有可用的向量 → 保序并记 skipped 原因
//     （degraded, not dropped，且 degradation 可见）；
//   - 成功 → 记 applied。
func rerankHits(c *context.Context, hits []retrieval.Hit, query string, maxCompare int) ([]retrieval.Hit, RerankState) {
	if len(hits) <= 1 {
		return hits, RerankState{Applied: false, Reason: "too few candidates"}
	}
	emb, ok := context.Get(c, KeyEmbedder)
	if !ok || emb == nil {
		return hits, RerankState{Applied: false, Reason: "embedder not bound"}
	}
	head := hits
	if len(head) > maxCompare {
		head = head[:maxCompare]
	}
	texts := make([]string, 0, len(head)+1)
	texts = append(texts, query)
	for _, h := range head {
		texts = append(texts, h.SpanText)
	}
	// 取消传播是 TODO：Retrieve 契约现在只带 cumulus context，embedding
	// 是本地快操作，先用 Background；接 HTTP 面时把 std ctx 透下来。
	vecs, err := emb.Embed(gocontext.Background(), texts)
	if err != nil {
		return hits, RerankState{Applied: false, Reason: "embed failed: " + err.Error()}
	}
	if len(vecs) != len(texts) {
		return hits, RerankState{Applied: false, Reason: "embedder returned wrong count"}
	}
	queryVec := vecs[0]
	passageVecs := vecs[1:]
	head = retrieval.RerankByCosine(head, queryVec, passageVecs)
	// 尾部（超出 maxCompare 的）按原序接回
	out := append(append([]retrieval.Hit(nil), head...), hits[len(head):]...)
	return out, RerankState{Applied: true}
}

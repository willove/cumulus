package qaflow

import (
	"errors"

	gocontext "context"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/ctxmgmt"
)

// KeyEviction 是驱逐账的挂点：合并了谁、丢了谁、为什么。合成前的上下文
// 管理必须可审计——不可见的裁剪和 cumulus 的 MCS 静默不触发是同一类事故。
var KeyEviction = context.NewKey[ctxmgmt.Log]("evidence.eviction")

// EvictStage 是合成前的上下文管理（04 第 4 条：Volt 驱逐策略）：
//   - 按源配额（PerSourceMax）：一家的窗口不许占满预算；
//   - 语义近重合并（DedupCosine，embedder 绑了才做）：cosine ≥ 阈值合并，
//     这是 MiniLM 在研究处方的位置（不是重排——重排已证伪）；
//   - 窗口预算（MaxWindows）：按打分保留前 N。
//
// embedder 缺席时语义合并不做，配额与预算照做——降级可见，不失败。
type EvictStage struct {
	Budget ctxmgmt.Budget
}

func (EvictStage) Name() string { return "context-evict" }
func (EvictStage) Reads() []string {
	return []string{KeyWindows.String(), KeyRewrite.String()}
}
func (EvictStage) Writes() []string {
	return []string{KeyWindows.String(), KeyEviction.String(), KeyCoverage.String()}
}

func (s EvictStage) Run(c *context.Context) error {
	ws, _ := context.Get(c, KeyWindows)
	if len(ws) == 0 {
		return nil
	}
	// 语义合并要向量：embedder 绑了才算（缺席 → 降级）
	var vecs [][]float32
	if s.Budget.DedupCosine > 0 && len(ws) > 1 {
		if emb, ok := context.Get(c, KeyEmbedder); ok && emb != nil {
			texts := make([]string, 0, len(ws))
			for _, w := range ws {
				texts = append(texts, w.Text)
			}
			got, err := emb.Embed(gocontext.Background(), texts)
			if err == nil && len(got) == len(ws) {
				vecs = got
			}
			// 失败不阻塞：配额与预算层面的管理仍然有效
		}
	}
	in := make([]ctxmgmt.Window, 0, len(ws))
	for _, w := range ws {
		in = append(in, ctxmgmt.Window{SourceID: w.SourceID, Title: w.Title, Span: w.Span, Text: w.Text, Score: w.Score})
	}
	kept, log := ctxmgmt.Apply(in, vecs, s.Budget)
	out := make([]EvidenceWindow, 0, len(kept))
	for _, w := range kept {
		out = append(out, EvidenceWindow{SourceID: w.SourceID, Title: w.Title, Span: w.Span, Text: w.Text, Score: w.Score, Substrate: "text"})
	}
	if err := context.Set(c, KeyWindows, out); err != nil {
		return err
	}
	if err := context.Set(c, KeyEviction, log); err != nil {
		return err
	}
	// 驱逐改了窗口集合，覆盖度必须重算（路由读的是驱逐后的事实——
	// 拿着驱逐前的覆盖度做路由是把旧状态当新状态）
	if rw, ok := context.Get(c, KeyRewrite); ok {
		texts := make([]string, 0, len(out))
		for _, w := range out {
			texts = append(texts, w.Text)
		}
		if ci, ok := context.Get(c, KeyCoverage); ok {
			// 复用证据期算出的可达词表（语料没变，词表不变）
			recomputed := recomputeCoverage(rw.Original, ci.Terms, texts)
			recomputed.OOV = ci.OOV
			if err := context.Set(c, KeyCoverage, recomputed); err != nil {
				return err
			}
		}
	}
	return nil
}

func (EvictStage) Verify(c *context.Context) error {
	ws, _ := context.Get(c, KeyWindows)
	for _, w := range ws {
		if w.SourceID == "" || w.Span == "" {
			return errors.New("evict: window missing source id or span — evicted windows must stay backable")
		}
	}
	return nil
}

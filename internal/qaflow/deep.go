package qaflow

import (
	gocontext "context"
	"sort"
	"strings"
	"sync"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/deepcore"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/retrieval"
)

// KeyDeep 是深循环遥测的挂点：跑了几轮、取过多少文档、几条死路、
// 覆盖度轨迹、为什么停。循环内部状态必须可观测——不可观测的循环
// 就是 cumulus 当年"采样基本不触发"那种事故的温床。
var KeyDeep = context.NewKey[deepcore.Telemetry]("evidence.deep")

// DeepOptions 是问答流程里的深循环预算（透传给 deepcore）。
type DeepOptions = deepcore.Options

// BM25DeepEvidence 把倒排索引接成"多轮取证"的取数函数：每轮取一页
// （k*page 个候选），可选语义重排（embedder 绑了才走），窗口进循环，
// 覆盖度达标/预算尽/无新候选三者收口。
//
// 单轮版是 BM25Evidence——行为完全一致（MaxRounds=1 时）。
func BM25DeepEvidence(idx *retrieval.Index, width int, deep DeepOptions) func(*context.Context, Rewrite) ([]EvidenceWindow, error) {
	return func(c *context.Context, r Rewrite) ([]EvidenceWindow, error) {
		k := deepK()
		windows, tele, err := deepcore.Run(gocontext.Background(), r.Original, deep,
			func(_ gocontext.Context, page int) ([]deepcore.Window, error) {
				// 循环内不重排：每页候选只过 BM25。理由不是省事——覆盖度
				// 信号要的是便宜且可重复的词法候选；把每页最多 50 个候选
				// 全送进 MiniLM，一次 300 题的评测就是几万次 CPU 推理，
				// 机器直接被打满（实测过，+680ms/题）。重排是收敛后的
				// 事：最终窗口最多十几条，比一次就够。
				hits := idx.SearchWith(r.Original, k*page, width, nil)
				out := make([]deepcore.Window, 0, len(hits))
				for _, h := range hits {
					out = append(out, deepcore.Window{
						SourceID: h.DocID,
						Span:     h.SpanCoord,
						Text:     h.SpanText,
						Score:    h.Score,
					})
				}
				return out, nil
			},
			func(query string, ws []deepcore.Window) float64 {
				return lexicalCoverage(idx, query, ws)
			},
		)
		// 失败时保留已取窗口：循环的降级是事实（遥测里有轮次与原因），
		// 失败的页带着错误出来，调用方自己决定
		if err := context.Set(c, KeyDeep, tele); err != nil {
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		out := make([]EvidenceWindow, 0, len(windows))
		for _, w := range windows {
			out = append(out, EvidenceWindow{
				SourceID: w.SourceID,
				Span:     w.Span,
				Text:     w.Text,
				Score:    w.Score,
			})
		}
		// 收敛后一次语义重排：最终窗口通常十几条，比一次代价可忽略；
		// 缺席/失败保序留痕（degraded, not dropped, and visible）
		if len(out) > 1 {
			out = rerankWindows(c, out, r.Original)
		}
		return out, nil
	}
}

// rerankWindows 对收敛后的最终窗口做一次语义重排（深循环专用：窗口
// 自带 Text，不用再解坐标）。缺席/失败/向量不全 → 原序返回 + 记原因。
// embedder 缺席在这里不算错——词法序是可用的降级，不是失败。
func rerankWindows(c *context.Context, windows []EvidenceWindow, query string) []EvidenceWindow {
	emb, ok := context.Get(c, KeyEmbedder)
	if !ok || emb == nil {
		if err := context.Set(c, KeyRerank, RerankState{Reason: "embedder not bound"}); err != nil {
			return windows
		}
		return windows
	}
	texts := make([]string, 0, len(windows)+1)
	texts = append(texts, query)
	for _, w := range windows {
		texts = append(texts, w.Text)
	}
	vecs, err := emb.Embed(gocontext.Background(), texts)
	if err != nil || len(vecs) != len(texts) {
		_ = context.Set(c, KeyRerank, RerankState{Reason: "embed failed or wrong count"})
		return windows
	}
	// 按 cosine 稳定重排（同分保原序——防抖动随机化词法序）
	type sw struct {
		w   EvidenceWindow
		cos float64
	}
	items := make([]sw, len(windows))
	for i, w := range windows {
		items[i] = sw{w: w, cos: embed.Cosine(vecs[0], vecs[i+1])}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].cos > items[j].cos })
	out := make([]EvidenceWindow, len(items))
	for i, s := range items {
		out[i] = s.w
	}
	_ = context.Set(c, KeyRerank, RerankState{Applied: true})
	return out
}

// lexicalCoverage 词面覆盖：查询词（去重）有多大比例出现在任一窗口
// 原文里。
//
// 分母只数**语料内可达词**：查询里总带粘连词（"是多少""的"），这些词
// 在语料里根本不存在，没有任何窗口能覆盖它们——把它们算进分母，目标
// 就永远不可达，循环空转预算后停在一个看起来像失败的覆盖度上。语料外
// 词不是噪声，是查询与语料的错配信号，单独计数上报（KeyCoverage）。
//
// 第一版不说语义——语义覆盖等 embedder 就位再做，不装样子。
func lexicalCoverage(idx *retrieval.Index, query string, ws []deepcore.Window) float64 {
	terms := dedupe(retrieval.Fields(query))
	if len(terms) == 0 || len(ws) == 0 {
		return 0
	}
	var all strings.Builder
	for _, w := range ws {
		all.WriteString(w.Text)
		all.WriteByte('\n')
	}
	hay := all.String()
	var covered, reachable int
	var oov []string
	for _, term := range terms {
		if idx == nil || !idx.HasTerm(term) {
			oov = append(oov, term) // 语料外词：不可达，不计分母
			continue
		}
		reachable++
		if strings.Contains(hay, term) {
			covered++
		}
	}
	lastOOV.Store(oov)
	if reachable == 0 {
		return 0
	}
	return float64(covered) / float64(reachable)
}

// lastOOV 是 lexicalCoverage 的语料外词上报通道（单线程 selftest/CLI
// 用；并发场景以后再改成显式返回值——那时覆盖函数签名会带 context 键）。
var lastOOV syncT

type syncT struct {
	mu sync.Mutex
	v  []string
}

func (s *syncT) Store(v []string) {
	s.mu.Lock()
	s.v = v
	s.mu.Unlock()
}

// LastOOV 取最近一次覆盖计算认定的语料外词（ selftest/诊断用）。
func LastOOV() []string {
	lastOOV.mu.Lock()
	defer lastOOV.mu.Unlock()
	return append([]string(nil), lastOOV.v...)
}

// deepK 每页候选数。固定 3：与单轮 BM25 的默认 topk 一致——A/B 时
// 第一轮的条件完全相同，差异全部来自"后面又取了几轮"。
func deepK() int { return 3 }

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

package qaflow

import (
	gocontext "context"
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/deepcore"
	"github.com/willove/cumulus/internal/embed"
	"github.com/willove/cumulus/internal/retrieval"
)

// KeyCoverage 是覆盖度记录（两条检索路都写）：数值 + 语料外词。
// 充足性路由读它当草稿信号——路由要的是可观测事实，不是调用方手填的
// 置信度（那正是 04 落点 1 要修掉的形态）。
var KeyCoverage = context.NewKey[CoverageInfo]("evidence.coverage")

// CoverageInfo 是词面覆盖的完整记录：值 + 语料外词（任何窗口都覆盖不了
// 的词——单独上报，它是查询与语料错配的信号，不是覆盖度的扣分项）。
type CoverageInfo struct {
	Value float64  `json:"value"`
	Terms []string `json:"terms,omitempty"` // 可达词表（语料内出现过的查询词）
	OOV   []string `json:"out_of_corpus_terms,omitempty"`
}

// KeyDeep 是深循环遥测的挂点：跑了几轮、取过多少文档、几条死路、
// 覆盖度轨迹、为什么停。循环内部状态必须可观测——不可观测的循环
// 就是 cumulus 当年"采样基本不触发"那种事故的温床。
var KeyDeep = context.NewKey[deepcore.Telemetry]("evidence.deep")

// DeepOptions 是问答流程里的深循环预算（透传给 deepcore）。
type DeepOptions = deepcore.Options

// DefaultDeep 是与单轮 topk=9 **同预算**的深循环默认档：每轮取 9 条新候选、
// 最多 3 轮（池子 27），最终按覆盖贪心保留 9 条。
//
// 为什么同预算：深循环要和单轮比，就得把"取数总量"对齐——否则赢的是
// 预算不是机制（v1 的错在这里反过来：它比单轮拿得少，输的是预算）。
// 池子（27）大于预算（9）才有"选哪几条"的余地；选择阶段是深循环唯一的
// 真优势所在。
func DefaultDeep() DeepOptions {
	return DeepOptions{MaxRounds: 3, CoverageTarget: 1.0, Budget: 9, PageSize: 9}
}

// BM25DeepEvidence 把倒排索引接成"多轮取证"的取数函数：每轮取一页
// （k*page 个候选），可选语义重排（embedder 绑了才走），窗口进循环，
// 覆盖度达标/预算尽/无新候选三者收口。
//
// 单轮版是 BM25Evidence——行为完全一致（MaxRounds=1 时）。
func BM25DeepEvidence(idx *retrieval.Index, width int, deep DeepOptions) func(*context.Context, Rewrite) ([]EvidenceWindow, error) {
	return func(c *context.Context, r Rewrite) ([]EvidenceWindow, error) {
		windows, tele, err := deepcore.Run(gocontext.Background(), r.Original, deep,
			func(_ gocontext.Context, offset, limit int) ([]deepcore.Window, error) {
				// 循环内不重排：每页候选只过 BM25。理由不是省事——覆盖度
				// 信号要的是便宜且可重复的词法候选；把每页最多 50 个候选
				// 全送进 MiniLM，一次 300 题的评测就是几万次 CPU 推理，
				// 机器直接被打满（实测过，+680ms/题）。重排是收敛后的
				// 事：最终窗口最多十几条，比一次就够。
				//
				// 分页语义：取 offset+limit 条再跳过前 offset 条——第 r 轮
				// 拿到的是"下一段新候选"，不是"前 3r 条"（后者会让池子
				// 永远等于 top-k，选择阶段无从发挥）。
				hits := idx.SearchWith(r.Effective(), offset+limit, width, nil)
				if offset >= len(hits) {
					return nil, nil
				}
				hits = hits[offset:]
				out := make([]deepcore.Window, 0, len(hits))
				for _, h := range hits {
					out = append(out, deepcore.Window{
						SourceID: h.DocID,
						Title:    h.Title, // 转换链第三处：deep 的 Hit→Window 不许蒸发标题
						// （真跑踩过：升级路径回填的窗口全程无标题）
						Span:  h.SpanCoord,
						Text:  h.SpanText,
						Score: h.Score,
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
				Title:    w.Title,
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
		texts := make([]string, 0, len(out))
		for _, w := range out {
			texts = append(texts, w.Text)
		}
		if err := context.Set(c, KeyCoverage, coverageOf(idx, r.Original, texts)); err != nil {
			return nil, err
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
	texts := make([]string, 0, len(ws))
	for _, w := range ws {
		texts = append(texts, w.Text)
	}
	return coverageOf(idx, query, texts).Value
}

// coverageOf 算覆盖并把语料外词一并记下（语料外词单独上报）。
func coverageOf(idx *retrieval.Index, query string, texts []string) CoverageInfo {
	terms := dedupe(retrieval.Fields(query))
	if len(terms) == 0 || len(texts) == 0 {
		return CoverageInfo{}
	}
	var all strings.Builder
	for _, w := range texts {
		all.WriteString(w)
		all.WriteByte('\n')
	}
	hay := all.String()
	var covered, reachable int
	var oov, reach []string
	for _, term := range terms {
		if idx == nil || !idx.HasTerm(term) {
			oov = append(oov, term) // 语料外词：不可达，不计分母
			continue
		}
		reach = append(reach, term)
		reachable++
		if strings.Contains(hay, term) {
			covered++
		}
	}
	if reachable == 0 {
		return CoverageInfo{OOV: oov}
	}
	return CoverageInfo{Value: float64(covered) / float64(reachable), Terms: reach, OOV: oov}
}

// recomputeCoverage 用既有可达词表重算覆盖度（驱逐后调用；不需要索引——
// 语料没变，词表不变）。
func recomputeCoverage(query string, terms []string, texts []string) CoverageInfo {
	if len(terms) == 0 || len(texts) == 0 {
		return CoverageInfo{}
	}
	var all strings.Builder
	for _, w := range texts {
		all.WriteString(w)
		all.WriteByte('\n')
	}
	hay := all.String()
	covered := 0
	for _, term := range terms {
		if strings.Contains(hay, term) {
			covered++
		}
	}
	return CoverageInfo{Value: float64(covered) / float64(len(terms)), Terms: terms}
}

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

// textsOf 取窗口文本（覆盖度只关心原文，不关心坐标）。
func textsOf(ws []EvidenceWindow) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Text)
	}
	return out
}

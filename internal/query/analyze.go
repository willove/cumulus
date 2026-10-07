// Package query 是查询侧的理解层：意图分层、IDF 加权关键词、词汇鸿沟
// 的兜底扩展。
//
// 搬 cumulus 的四件（internal/fast 的 Analysis 契约 + KeywordExpander +
// prior 的用法），按 cumulus-next 的文法落成 stage 1 的部件：
//
//  1. 意图分层：search / chat——问答面只 search，但分层是后续文档总结
//     类任务的挂钩处；
//  2. IDF 加权主关键词级：查询词按语料内 IDF 定权——**常见泛词降权、
//     稀有实体词着重**（"闯红灯如何处罚"里"处罚"满库都是、降；"驾驶"
//     "信号灯"稀有著重）。这不是新发明，是 BM25 的 idf 本该做的事，
//     只是查询侧把它显式化，好让后续阶段（覆盖度、重排、兜底）用同一
//     份权；
//  3. 多级 fallback：主级（加权后的稀有权词）落空时，退回更宽的级；
//  4. LLM 关键词扩展：命中稀薄（词汇鸿沟）时调模型把口语问句翻成语料
//     语言的检索词——"养狗叫得太吵" → "饲养动物 噪声"。
package query

import (
	"sort"
	"strings"
)

// Term 是一个查询词及其权。
type Term struct {
	Text     string
	Weight   float64
	InCorpus bool // 语料内是否有它（OOV 词权重为 0，但它是鸿沟信号）
	DF       int  // 语料内文档频率（权重依据）
}

// Analysis 是查询理解的产物。
type Analysis struct {
	Intent     string             // search / chat
	Primary    map[string]float64 // 主关键词级（语词 → 权；OOV 词不入）
	Fallback   []string           // 兜底级：降权的泛词与被舍弃的宽匹配词
	OOV        []string           // 语料外词（词汇鸿沟的直接信号）
	Score      float64            // 主级的总权重（稀薄度代理：越低越是鸿沟）
	TotalTerms int                // 内容词总数（去胶水；OOV 占比的分母）
}

// OOVShare 语料外内容词占比——词汇鸿沟的直接度量（>0.6 基本是口语对
// 书面语全党外）。比总分更能说明"库里没有这套话"。
func (a Analysis) OOVShare() float64 {
	if a.TotalTerms == 0 {
		return 0
	}
	return float64(len(a.OOV)) / float64(a.TotalTerms)
}

// CorpusTerms 是分析需要的语料事实（BM25 索引就有：df）。
type CorpusTerms interface {
	// HasTerm / DFOf 由索引提供；nil = 不知道语料（分析退化）
	HasTerm(term string) bool
	DFOf(term string) int
}

// Analyze 规则版查询分析（离线门禁 carrier）：意图按问句形态，权重按
// 语料内 IDF。权重口径：
//   - 语料外词（OOV）：权重 0，进 OOV 列表——它不是检索词，是鸿沟信号；
//   - 库内泛词（df ≥ 库的 10%）：权重 0.3——满库都是，降权；
//   - 库内常见词（df ≥ 1%）：权重 1.0；
//   - 库内稀有词（df < 1% 或 df ≤ 3）：权重 2.0——实体感，着重。
//
// 权重是默认值不是魔数——调它要走评测对照（evolution-log 的判决）。
// stopwords 是查询胶水：疑问词、语气词、常见功能词。它们在语料里的
// IDF 可能很高（法律文本从不说"怎么"）——纯 IDF 会把它们当实体词着重，
// 真跑踩过：闯红灯怎么处罚，"怎么"权重 2.0 压过内容词。胶水就是胶水，
// 权重 0，不进主级也不进兜底级。
var stopwords = map[string]bool{
	"怎么": true, "如何": true, "什么": true, "为什么": true, "吗": true, "呢": true,
	"多少": true, "几": true, "谁": true, "哪些": true, "是不是": true, "能不能": true,
	"可以": true, "应该": true, "请问": true, "告诉": true, "一下": true, "的": true,
	"了": true, "着": true, "和": true, "与": true, "或": true, "及": true,
}

// glueChars 是胶水字符集：二元组只要沾一个就降权。"多少年"切出的
// 少年/限多在法条里稀有（未成年人保护法刷屏），纯 IDF 会给它们 2.0，
// 压过真正的"专利"——真跑踩过（问"专利期限多少年"返回少年相关法）。
// 二元组层面的认真，靠字符集兜底；LLM 分析器是彻底解，那是 cumulus
// 的生产路径，rule 版先把这个洞堵上。
const glueChars = "的吗呢吧啊什怎多极少几谁和与或及于在被把就都还很"

func isGlueTerm(term string) bool {
	for _, r := range term {
		if strings.ContainsRune(glueChars, r) {
			return true
		}
	}
	return false
}

func Analyze(q string, corpus CorpusTerms, docCount int) Analysis {
	a := Analysis{Intent: intentOf(q), Primary: map[string]float64{}}
	for _, term := range termsOf(q) {
		if stopwords[term] {
			continue // 胶水：不是检索词，断言的份量都没有
		}
		if isGlueTerm(term) {
			a.Fallback = append(a.Fallback, term) // 沾胶水的二元组：降权进兜底级
			a.TotalTerms++
			continue
		}
		if corpus == nil || !corpus.HasTerm(term) {
			a.OOV = append(a.OOV, term)
			continue
		}
		df := corpus.DFOf(term)
		w := weightOf(df, docCount)
		if w < 1.0 {
			a.Fallback = append(a.Fallback, term) // 降权词同时是兜底级成员
			continue
		}
		a.Primary[term] = w
		a.Score += w
	}
	a.TotalTerms = len(a.Primary) + len(a.OOV)
	return a
}

func weightOf(df, docCount int) float64 {
	if docCount <= 0 {
		return 1
	}
	switch {
	case df <= 3:
		return 2.0 // 稀有：实体感，着重
	case df*10 >= docCount:
		return 0.3 // 满库都是：泛词，降权
	default:
		return 1.0
	}
}

func intentOf(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return "chat"
	}
	// 疑问/指令形态 → search；陈述 → chat（问答面当前只走 search，
	// 分层留给以后的任务类型）
	for _, w := range []string{"吗", "呢", "什么", "怎么", "如何", "哪些", "多少", "几", "谁", "？", "?", "；", ";"} {
		if strings.Contains(q, w) {
			return "search"
		}
	}
	return "chat"
}

// termsOf 查询分词：复用 retrieval 的二元组口径（同 fast.Fields），
// 去重保序。
func termsOf(q string) []string {
	raw := fields(q)
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// SortedTerms 主关键词按权排序（诊断与提示词用）。
func (a Analysis) SortedTerms() []Term {
	out := make([]Term, 0, len(a.Primary))
	for t, w := range a.Primary {
		out = append(out, Term{Text: t, Weight: w})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Weight > out[j].Weight })
	return out
}

// Thin 判定主关键词级是否稀薄（鸿沟信号）：库里能用的强化词少于
// minTerms 个，或总权重低于 floor。稀薄 → 触发 LLM 扩展兜底。
func (a Analysis) Thin(minTerms int, floor float64) bool {
	return len(a.Primary) < minTerms || a.Score < floor || a.OOVShare() >= 0.6
}

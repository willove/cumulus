// Package docgen —— 知识文档生成：把散落的证据整理成**一篇可核对的文档**。
//
// 为什么它是个人知识库的主要产物：问答只回答"这一次问的"，而**文档沉淀"这一整块
// 知识"**。用户真正想要的是"关于这个主题，我该知道什么"，而不是"这一问的答案"。
//
// 三条纪律（都是从问答链搬过来的，不重新发明）：
//
//  1. **每条论断都要挂引用**：claim 必须指回一条证据窗口（source_id + span）。
//     挂不上引用的内容**不许写进文档**——它进 `gaps`（缺口），并标成"语料里没有依据"。
//     理由与链上"引用必须能映射回窗口"同源：**没有出处的句子不该出现在可信文档里**。
//  2. **文档进语料**：生成后写进同一个集合，所以**下一轮问答能引用它**
//     （整理一次、问答受益）。这是它比"摘要"有用的地方。
//  3. **诚实的覆盖度**：文档带 windows_used / facts_covered / sections 计数。
//     读者一眼能看出"这篇覆盖了 4/9 条证据"，而不是假装完整。
package docgen

import (
	gocontext "context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/retrieval"
)

// Document 是一篇生成的知识文档。
type Document struct {
	ID       string    `json:"id"`       // 写入语料后的文档 id（内容哈希）
	Topic    string    `json:"topic"`    // 主题（原样的问句）
	Title    string    `json:"title"`    // 文档标题
	Body     string    `json:"body"`     // **进语料的正文**（人读 + 可检索）
	Sections []Section `json:"sections"` // 结构化条目（展示与核对用）
	Gaps     []Gap     `json:"gaps,omitempty"`
	Coverage Coverage  `json:"coverage"`

	// Version 是**同主题文档的版本号**（首次生成=1；再生成覆盖并 +1）。
	// Note 是增量说明（新增/移除几条）；Sources 是本版依据的源文档 id。
	Version int      `json:"version"`
	Note    string   `json:"note,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// Section 是文档的一节（一个子问题）。
type Section struct {
	Heading string  `json:"heading"`
	Claims  []Claim `json:"claims"`
}

// Claim 是节里的一条论断。**SourceID/Span 必填**（纪律 1：没有出处的不写）。
type Claim struct {
	Text     string `json:"text"`
	SourceID string `json:"source_id"`
	Span     string `json:"span"`
}

// Gap 是"语料里没有依据"的部分——**写进文档比藏起来更有用**（读者知道边界在哪）。
type Gap struct {
	Heading string `json:"heading"` // 问过但语料里查不到的小问题
	Note    string `json:"note"`
}

// Coverage 是诚实的覆盖度（纪律 3）。
type Coverage struct {
	WindowsTotal int `json:"windows_total"` // 取到的证据条数
	WindowsUsed  int `json:"windows_used"`  // 真的被引用了几条
	Sections     int `json:"sections"`
	Gaps         int `json:"gaps"`
	FactsCovered int `json:"facts_covered,omitempty"` // 问句事实被覆盖的比例（分母 facts_total）
	FactsTotal   int `json:"facts_total,omitempty"`
}

// Generator 生成知识文档。**可选能力**：没有 LLM 就没有它（服务端 /v1/status 可见）。
type Generator struct {
	Client llm.Completer
	// MaxWindows 参与生成的证据上限（0 = 默认 12）。文档质量随证据量增长，
	// 但一次调用塞太多会稀释注意力（实测：超过 12 条后模型开始复述而不是归纳）。
	MaxWindows int
}

const systemPrompt = `你是知识文档写作器。给你一个主题与若干条证据，请写出这个主题下的知识文档。

三条硬规则：
1. **每条论断必须挂引用**：用 [编号] 标出它来自哪条证据。挂不上引用的内容不要写。
2. **只写证据里有的**：不要补充你的世界知识。证据里没有的，写进 gaps。
3. **不要写套话**：不要"随着……的发展"这类句子。

输出严格 JSON，不要解释：
{"title":"...","sections":[{"heading":"小问题","claims":[{"text":"论断","source":1}]}],"gaps":[{"heading":"语料里查不到的小问题","note":"说明缺什么"}]}`

type llmAnswer struct {
	Title    string `json:"title"`
	Sections []struct {
		Heading string `json:"heading"`
		Claims  []struct {
			Text   string `json:"text"`
			Source int    `json:"source"`
		} `json:"claims"`
	} `json:"sections"`
	Gaps []struct {
		Heading string `json:"heading"`
		Note    string `json:"note"`
	} `json:"gaps"`
}

// Generate 生成一篇文档。
//
// 失败语义与链上其它调用一致：**要么给完整文档，要么什么都不给**（半截文档比没有
// 文档更坏——它会被当成可信资料存进语料）。
func (g *Generator) Generate(ctx gocontext.Context, topic string, ws []retrieval.Hit) (*Document, error) {
	if g == nil || g.Client == nil {
		return nil, fmt.Errorf("docgen: 无 LLM（生成能力缺席）")
	}
	if strings.TrimSpace(topic) == "" {
		return nil, fmt.Errorf("docgen: 空主题")
	}
	limit := g.MaxWindows
	if limit <= 0 {
		limit = 12
	}
	if len(ws) < limit {
		limit = len(ws)
	}
	if limit == 0 {
		return nil, fmt.Errorf("docgen: 没有任何证据（先检索到东西再生成文档）")
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "主题：%s\n\n证据：\n", topic)
	for i := 0; i < limit; i++ {
		fmt.Fprintf(&sb, "[%d] %s\n\n", i+1, trim(ws[i].SpanText, 600))
	}
	resp, err := g.Client.Complete(ctx, llm.Request{System: systemPrompt, Prompt: sb.String(), MaxTokens: 2000})
	if err != nil {
		return nil, fmt.Errorf("docgen: complete: %w", err)
	}
	raw := llm.LastJSONObject(resp.Text)
	if raw == "" {
		return nil, fmt.Errorf("docgen: 回包没有 JSON（raw=%q）", trim(resp.Text, 160))
	}
	return assemble(raw, topic, ws[:limit])
}

// assemble 把模型回包整理成文档，并把**引用校验**做完（纪律 1 的落点）。
func assemble(raw, topic string, ws []retrieval.Hit) (*Document, error) {
	var a llmAnswer
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, fmt.Errorf("docgen: 回包不合约定: %w", err)
	}
	doc := &Document{Topic: topic, Title: strings.TrimSpace(a.Title)}
	if doc.Title == "" {
		doc.Title = topic // 没有标题就用主题（不留空标题）
	}
	used := map[string]bool{}
	for _, sec := range a.Sections {
		if strings.TrimSpace(sec.Heading) == "" || len(sec.Claims) == 0 {
			continue // 空节不留（标题或论断为空的节在正文里会变成孤零零一个标题）
		}
		out := Section{Heading: sec.Heading}
		for _, c := range sec.Claims {
			text := strings.TrimSpace(c.Text)
			if text == "" {
				continue
			}
			// 引用校验：编号必须落在本轮证据里（越界的引用是**幻觉**，丢弃该条）
			if c.Source < 1 || c.Source > len(ws) {
				continue
			}
			h := ws[c.Source-1]
			out.Claims = append(out.Claims, Claim{Text: text, SourceID: h.DocID, Span: h.SpanCoord})
			used[h.DocID] = true
		}
		if len(out.Claims) == 0 {
			continue // 全被校验丢弃的节不留
		}
		doc.Sections = append(doc.Sections, out)
	}
	for _, gp := range a.Gaps {
		if strings.TrimSpace(gp.Heading) == "" {
			continue
		}
		doc.Gaps = append(doc.Gaps, Gap{Heading: gp.Heading, Note: gp.Note})
	}
	if len(doc.Sections) == 0 {
		return nil, fmt.Errorf("docgen: 没有一条能挂上引用的论断（不产出半截文档）")
	}
	doc.Version = 1
	doc.Sources = SourcesOf(doc)
	doc.Body = renderBody(doc)
	doc.Coverage = Coverage{
		WindowsTotal: len(ws),
		WindowsUsed:  len(used),
		Sections:     len(doc.Sections),
		Gaps:         len(doc.Gaps),
	}
	// 事实覆盖：主题被拆成的事实里，有多少在正文里被回答了。**分母摆出来**，
	// 读者才知道"这篇覆盖了几个方面"。
	covered, total := factCoverage(topic, doc)
	doc.Coverage.FactsCovered, doc.Coverage.FactsTotal = covered, total
	return doc, nil
}

// factCoverage 回答了主题里多少个事实（关键词覆盖判定，不是模型判断）。
//
// 口径要粗且诚实：只数"事实的核心词有没有出现在某条论断里"。它**高估**覆盖率
// （出现了词不等于回答了问题），所以文档里把它标成"事实覆盖"，不叫"完整度"。
func factCoverage(topic string, doc *Document) (covered, total int) {
	fx := facts.Decompose(topic)
	if len(fx) == 0 {
		return 0, 0
	}
	var joined strings.Builder
	for _, sec := range doc.Sections {
		for _, c := range sec.Claims {
			joined.WriteString(c.Text)
			joined.WriteString("\n")
		}
	}
	for _, f := range fx {
		total++
		// 用事实自己的问句里的内容词做覆盖判定（口径粗，见函数注释）
		for _, kw := range fieldsOf(f.Query) {
			if strings.Contains(joined.String(), kw) {
				covered++
				break
			}
		}
	}
	return covered, total
}

// fieldsOf 取一段文本的检索口径词（与检索层同源：Fields 拆二元组 + 整词）。
func fieldsOf(s string) []string {
	out := facts.Fields(s)
	seen := map[string]bool{}
	uniq := out[:0]
	for _, x := range out {
		if !seen[x] {
			seen[x] = true
			uniq = append(uniq, x)
		}
	}
	return uniq
}

// renderBody 渲染成**进语料的正文**：人读要顺，检索要能命中（标题 + 论断 + 来源行）。
func renderBody(doc *Document) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", doc.Title)
	for _, sec := range doc.Sections {
		fmt.Fprintf(&sb, "## %s\n\n", sec.Heading)
		for _, c := range sec.Claims {
			fmt.Fprintf(&sb, "- %s [%s]\n", c.Text, c.SourceID)
		}
		sb.WriteString("\n")
	}
	if len(doc.Gaps) > 0 {
		sb.WriteString("## 语料里没有依据的部分\n\n")
		for _, gp := range doc.Gaps {
			fmt.Fprintf(&sb, "- %s", gp.Heading)
			if gp.Note != "" {
				fmt.Fprintf(&sb, "（%s）", gp.Note)
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

func trim(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Marker 是生成文档正文里的来源标记（HTML 注释：人读不可见，检索可见）。
//
// 为什么是注释而不是 YAML 头：正文要**原样可读**（贴进任何地方都成立），
// 而机器只需要"这一行在"。检索层据此识别生成文档。
const Marker = "<!-- cumulus:generated -->"

// IsGenerated 判断正文是不是生成文档（**这是唯一口径**：不靠 id 前缀、不靠
// 外部账本——那两样都会与正文漂移）。
func IsGenerated(body string) bool {
	return strings.Contains(body, Marker)
}

// BoostFor 是生成文档在检索里的权重系数。
//
// 实测依据：生成文档是**源文档的转述**——词面高度重叠、篇幅更长，BM25 打分
// 天然比不过更短更密的源文档。后果是"整理了一次、问答受益"变成空话：库里多
// 了一篇文档，但问答永远引用源。
//
// 压到 0.85 不是"藏起来"：命中生成文档时它仍可被引用，而**先答不上来的问题，
// 转述文档恰恰是更好的答案**（它已经把多篇源拼在一起，并标出了缺口）。
// 真正的对手是"引用一堆碎片源"——那种答案更难读。
const BoostGenerated = 0.85

// ApplyMarker 给正文加来源标记（写库前调；幂等）。
func ApplyMarker(body string) string {
	if IsGenerated(body) {
		return body
	}
	return Marker + "\n" + body
}

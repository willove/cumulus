package docgen

// revision.go —— 同一主题的**增量更新**：再生成一次是"更新这一篇"，不是"再堆一篇"。
//
// 为什么必须有它（真跑推演）：生成文档是**可重复生成**的产物。用户补了新资料、
// 改了一句源文档、想换个主题问法，都会再生成一次。若每次都写新文档，语料里会出现
// 三五篇内容高度重叠的同主题文档——**检索互相竞争、读者分不清哪篇是最新的**，
// 而"整理一次、问答受益"会变成"整理越多、越难查到"。
//
// 三条纪律：
//
//  1. **同主题 = 同一篇**：文档 id 由**主题键**决定（不是内容哈希），所以再生成
//     是覆盖而不是新增。
//  2. **版本与差异要摆出来**：v2 不是"悄悄换掉"——文档里写明"第 2 版 · 新增 3 条
//     · 移除 1 条"。读者能判断要不要信新版本。
//  3. **回退也要看得见**：新一版比旧的**少**了内容（源被删/检索退化），那是**信息损失**，
//     要显式说出来，不能只报"已更新"。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// TopicKey 是主题的稳定键：同主题的历次生成都落到它上面。
//
// 用**归一化主题**（去空白、小写、全角标点转半角）而不是原文：同一件事的两种问法
// 应该收敛到同一篇文档，而不是各生成一篇。
func TopicKey(topic string) string {
	sum := sha256.Sum256([]byte(NormalizeTopic(topic)))
	return hex.EncodeToString(sum[:])[:12]
}

// NormalizeTopic 是主题归一化（去空白 + 小写 + 全角转半角）。
func NormalizeTopic(topic string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(topic)) {
		switch {
		case r == '　':
			r = ' ' // 全角空格
		case isFullWidthPunct(r):
			// 全角标点 → 半角。**必须查表，不能用算术**：全角区到半角不是常数
			// 偏移（'？'U+FF1F→'?'U+003F 差 0x1EC0，'！'U+FF01→'!'U+0021 差 0x1FE0）。
			// 我第一版用 r-'！'+'!'，症状是"同一主题的两种问法生成两篇文档"——
			// 恰好是这个功能要防的事，所以归一化必须**测到**。
			r = halfWidthPunct[r]
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// fullWidthPunct 是全角标点 → 半角的**查表**（不靠算术，见 NormalizeTopic）。
var halfWidthPunct = map[rune]rune{
	'！': '!', '＂': '"', '＃': '#', '＄': '$', '％': '%', '＆': '&', '＇': '\'',
	'（': '(', '）': ')', '＊': '*', '＋': '+', '，': ',', '－': '-', '．': '.',
	'／': '/', '：': ':', '；': ';', '＜': '<', '＝': '=', '＞': '>', '？': '?',
	'＠': '@', '［': '[', '＼': '\\', '］': ']', '＾': '^', '＿': '_', '｀': '`',
	'｛': '{', '｜': '|', '｝': '}', '～': '~',
}

func isFullWidthPunct(r rune) bool { _, ok := halfWidthPunct[r]; return ok }

// DocID 是同主题文档的固定 id（覆盖写的前提）。
func DocID(topic string) string { return "gen-" + TopicKey(topic) }

// IsGenID 判断是不是生成文档的 id（检索期 boost 用它，O(1)）。
func IsGenID(id string) bool { return strings.HasPrefix(id, "gen-") }

// Revision 是一次更新的**账**：版本号、依据的源、增删了什么。
type Revision struct {
	Version int
	// First 标记这是**首次生成**（没有上一版可比）。Note() 必须与"内容没变"
	// 区分开：前者不说差异，后者要说"只是重新生成"——两者含义完全不同。
	First   bool
	Added   []string // 新增的论断（正文原句）
	Removed []string // 这一版没有的论断（来自旧版）
	// SourcesChanged 是源集合是否变了（没变 = 只是重新组织，没变信息）。
	SourcesChanged bool
}

// Revise 比较新旧文档，算出这一版的增量说明。
//
// 口径用**论断正文**（不是 span）：用户关心的是"说法变了没有"，坐标变了不算内容变。
func Revise(oldDoc *Document, newDoc *Document, oldSources, newSources []string) Revision {
	return ReviseWith(oldDoc, newDoc, oldSources, newSources, nil)
}

// ReviseWith 是带**语义判据**的版本比较（judge 可为 nil = 只用词面口径）。
func ReviseWith(oldDoc *Document, newDoc *Document, oldSources, newSources []string, judge EquivalentJudge) Revision {
	rev := Revision{Version: 1}
	if oldDoc == nil {
		rev.Version, rev.First = 1, true
		rev.SourcesChanged = true
		return rev
	}
	rev.Version = oldDoc.Version + 1
	// **比较按实质，不按字面**：同一件事两次生成，模型措辞会有差异（"滞纳金"
	// vs "百分之五十的滞纳金"）。字面比较会得出"新增 4 条、移除 5 条"的**假差异**，
	// 而那正是真跑第一次看到的读数——版本说明一旦撒谎，读者就不再信它。
	// 口径：去标点与空白后**互为子串**即视为同一条（粗但方向保守：宁可少报差异，
	// 不可谎报增删）。
	oldTexts, newTexts := claimTexts(oldDoc), claimTexts(newDoc)
	for _, t := range newTexts {
		if !anySameTo(t, oldTexts, judge) {
			rev.Added = append(rev.Added, t)
		}
	}
	for _, t := range oldTexts {
		if !anySameTo(t, newTexts, judge) {
			rev.Removed = append(rev.Removed, t)
		}
	}
	rev.SourcesChanged = !sameSet(oldSources, newSources)
	return rev
}

// Note 是一行人读的增量说明（写进正文；为空说明"内容没变"）。
//
// 显式区分三种情况，因为它们对读者的意义完全不同：
//   - 新增 3 条：确实有新东西；
//   - 移除 1 条：**信息损失**，读者要警觉（源可能被删了）；
//   - 什么都没变：只是重新生成，不必当成"更新"。
func (r Revision) Note() string {
	if r.First {
		return "" // 首次生成：没有"上一版"，不报差异
	}
	var parts []string
	if len(r.Added) > 0 {
		parts = append(parts, fmt.Sprintf("新增 %d 条", len(r.Added)))
	}
	if len(r.Removed) > 0 {
		parts = append(parts, fmt.Sprintf("**移除 %d 条**", len(r.Removed)))
	}
	if len(parts) == 0 {
		return "内容与上一版一致（本次只是重新生成）"
	}
	return strings.Join(parts, "、")
}

// Stamp 把版本与增量说明写进正文（第 1 版不写——没有"上一版"可言）。
func Stamp(body string, rev Revision) string {
	if rev.Version <= 1 {
		return body
	}
	header := fmt.Sprintf("<!-- cumulus:generated v%d · %s · %s -->\n",
		rev.Version, rev.Note(), time.Now().Format("2006-01-02"))
	return header + body
}

// anySame 判断 text 是否与列表里某条实质相同。
func anySame(text string, list []string) bool { return anySameTo(text, list, nil) }

// anySameTo 是带语义判据的版本（判据**只用于词面判不出的候选对**，
// 避免每对都调模型）。
func anySameTo(text string, list []string, judge EquivalentJudge) bool {
	for _, x := range list {
		if sameClaim(text, x) {
			return true
		}
	}
	if judge == nil {
		return false
	}
	// 词面判据已经筛过一轮，这里剩下的都是"可能是同义改写"的候选
	for _, x := range list {
		ok, err := judge(text, x)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// claimKey 是论断的比较键：去掉标点、空白、**以及引用标记**。
//
// **引用标记必须剥掉**（真跑踩到）：模型有时把 [1][2] 直接写进 claim 文本，
// 于是同一条论断第二次生成变成"…每年缴纳年费。[1][2]"——字面完全不同，
// 版本说明会报"新增 1 条、移除 7 条"这种**假差异**。
func claimKey(text string) string {
	var b strings.Builder
	for _, r := range stripCiteMarks(text) {
		if isFullWidthPunct(r) {
			r = halfWidthPunct[r]
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// stripCiteMarks 去掉行尾/行中的引用标记（`[1]` `[1,2]` `[1][2]`）。
//
// 只认**纯数字方括号**：`[d1]` 那种是源文档 id（正文渲染时才加），
// 不该被当成编号标记剥掉——剥了会把不同来源的同句论断混成一条。
func stripCiteMarks(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '[' {
			j := i + 1
			ok := j < len(text)
			for j < len(text) && (text[j] >= '0' && text[j] <= '9' || text[j] == ',' || text[j] == ' ') {
				j++
			}
			if ok && j < len(text) && text[j] == ']' && j > i+1 {
				i = j // 跳过整个标记
				continue
			}
		}
		b.WriteByte(text[i])
	}
	return b.String()
}

// sameClaim 两条论断是否实质相同。
//
// 口径（按真跑踩坑逐级退到这里）：字面相同 → 字符集合相似 → **覆盖度重合**。
//
// 为什么必须用覆盖度：真跑量到的"差异"全是同义改写——
//
//	"实用新型年费为每**一百九十元**" vs "为每**一**百九十元"
//	"**请求减缓**年费应提交书面说明" vs "**申请缓缴**时提交书面说明"
//
// 字符集合相似度对这类改写到不了 0.9，于是版本说明报"移除 3 条"——**假差异**。
// 而这些句子说的是同一件事：它们**覆盖同一组检索词**（qaflow/facts 那套覆盖度判据
// 已经在链上用了很久，口径现成且可解释：命中词越多越接近）。
//
// 阈值 0.75：偏低是刻意的——**宁可少报差异，不可谎报增删**。版本说明一旦撒谎，
// 读者就不信它了，而"少报"顶多让人自己看一眼。
func sameClaim(a, b string) bool {
	x, y := claimKey(a), claimKey(b)
	if x == "" || y == "" {
		return false
	}
	if x == y {
		return true
	}
	if overlap(x, y) >= 0.75 {
		return true
	}
	// 兜底：字符集合相似（覆盖度对"用词完全不同但同义"的改写会失效）
	return charSim(x, y) >= 0.9
}

// EquivalentJudge 判两条论断**是否同一件事**（可选能力）。
//
// 为什么要模型判：词面判据有**天花板**，真跑量到的最难一对是
// "请求减缓年费…" vs "申请缓缴年费…"——字面重合低，但说的是同一件事。
// 词面判据抓不住，就别假装抓得住：**它只负责召回候选对，真正定论交给模型**。
//
// 保守方向：模型判"是"→ 算同一条；模型判"否"或**缺席/失败** → 算不同
// （宁可多报差异，不可谎报合并——合并错了会掩盖真实的信息变化）。
type EquivalentJudge func(a, b string) (bool, error)

// overlap 是两个键的**有序二元组覆盖重合度**（facts.Fields 的检索口径）。
//
// 用二元组而不是整词：中文里"申请/缓缴"是两个词，整体重合会低；二元组能捕捉
// "申请缓缴"这种连续片段的真实重叠。
func overlap(x, y string) float64 {
	fx, fy := fieldsOf(x), fieldsOf(y)
	if len(fx) == 0 || len(fy) == 0 {
		return 0
	}
	set := map[string]bool{}
	for _, f := range fy {
		set[f] = true
	}
	hit := 0
	for _, f := range fx {
		if set[f] {
			hit++
		}
	}
	return float64(hit) / float64(len(fx))
}

// charSim 是字符集合相似度（覆盖度失效时的兜底）。
func charSim(x, y string) float64 {
	rx, ry := []rune(x), []rune(y)
	if len(rx) > len(ry) {
		rx, ry = ry, rx
	}
	set := map[rune]bool{}
	for _, r := range ry {
		set[r] = true
	}
	hit := 0
	for _, r := range rx {
		if set[r] {
			hit++
		}
	}
	if len(rx) == 0 {
		return 0
	}
	return float64(hit) / float64(len(rx))
}

func claimTexts(d *Document) []string {
	var out []string
	for _, sec := range d.Sections {
		for _, c := range sec.Claims {
			out = append(out, c.Text)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]bool{}
	for _, x := range a {
		m[x] = true
	}
	for _, x := range b {
		if !m[x] {
			return false
		}
	}
	return true
}

// SourcesOf 是一篇文档依据的源文档 id（去重、保序）。
func SourcesOf(d *Document) []string {
	seen := map[string]bool{}
	var out []string
	for _, sec := range d.Sections {
		for _, c := range sec.Claims {
			if c.SourceID != "" && !seen[c.SourceID] {
				seen[c.SourceID] = true
				out = append(out, c.SourceID)
			}
		}
	}
	return out
}

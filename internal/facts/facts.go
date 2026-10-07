// Package facts 是事实分解与事实覆盖：把问句拆成原子事实，逐个判证据
// 是否真盖到（cumulus internal/facts 的移植）。
//
// 为什么要有：查询词覆盖度（coverageOf）答不出"这问答得全不全"——
// "闯红灯怎么处罚"的词覆盖可以很高（处罚/红灯全在窗里），但"具体怎么
// 罚（金额/记分）"这条事实没盖到。cumulus 的准确定义是**事实点全盖**，
// 这道工序就是它的落地处。
//
// 移植时保留的判据（ cumulus 在真实问集上实测出来的，不重发明）：
//   - K=1 常态：136 问里 124 问 K=1；只有查询自己声明了清单结构（并列
//     标记/多问号）才切分——"发明创造定义"这种专词硬切出"发明/创造"，
//     early stop 引错法还只烧一半 token（实测缺陷，靠 HasCoordination 挡）；
//   - 语义单元门：<4 字的碎片、括号/书名号切穿的半截不是事实
//     （"参与违法怎么办"切出 "参" 的幽灵需求，DEEP 烧完预算也盖不上）；
//   - 覆盖要**同时**满足词面占比 ≥0.5 与**连续 3 字核心在窗内**——单占比
//     让半个词盖掉整个事实（红灯那个"看着已覆盖"的拒答就是这么来的）；
//   - NearMiss：未盖事实的最好接进度（weakest-requirement 停止信号要用）。
package facts

import (
	"github.com/willove/cumulus/internal/query"
	"sort"
	"strings"
	"unicode/utf8"
)

// Fact 是一条原子证据需求。
type Fact struct {
	ID       string   `json:"id"` // f1/f2…（覆盖按 id 报告）
	Query    string   `json:"query"`
	Covered  bool     `json:"covered"`
	Score    float64  `json:"score,omitempty"`     // 最好支撑窗的分数
	NearMiss float64  `json:"near_miss,omitempty"` // 未盖时的最好接进度
	Supports []Window `json:"supports,omitempty"`  // 全部支撑窗（按分降序）
}

// Report 是事实覆盖状态。
type Report struct {
	Facts    []Fact   `json:"facts"`
	Complete bool     `json:"complete"` // 全部盖到
	Missing  []string `json:"missing"`  // 没盖到的事实 id
	Weakest  float64  `json:"weakest"`  // 最弱事实的接进度
	K        int      `json:"k"`
}

// CoverHit 是窗支撑一条事实所需的最小词面占比。
const CoverHit = 0.5

// minUnitRunes 事实最短字数（"参" 这种碎片不算事实）。
const minUnitRunes = 4

// maxParts 事实数上限（切分本身截到 4）。
const maxParts = 4

// boundaryRunes 括号/引号/书名号：不平衡 = 从标题中间切断了。
const boundaryRunes = "\"'“”‘’《》〈〉()（）[]【】{}"

// coordinationMarkers 是查询声明"这是清单"的显式结构。
var coordinationMarkers = []string{
	"分别", "各自", "以及", "同时", "还有", "此外", "另外", "分别指", "分别是什么",
}

// alternativeMarkers 看着像并列其实不是："或者" 在法律问句里是二选一
// 条件，切开造出两条需求而原文只有一条。
var alternativeMarkers = []string{"或者", "或是", "还是", "或则"}

// splitMarks 切分标记（带空格的英文标记前后有空格，中文单字标记两侧
// 至少各两字才切——单字标记会在词中间下刀）。
var splitMarks = []string{
	"以及", "并且", "同时", "另外", "此外",
	"相比", "对比", "还是", "或者",
	"和", "与", "及",
	" and ", " and/or ", " or ", " vs ", " versus ", " compared ",
	"；", ";",
	"？", "?",
}

// Decompose 把查询拆成原子事实（规则版，离线门的基线）。查询自己没声明
// 清单结构时 K=1——宁漏勿切：切错的 phantom 需求比少切一条贵得多。
func Decompose(query string) []Fact {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil
	}
	parts := splitQuery(q)
	out := make([]Fact, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || !IsSemanticUnit(p) {
			continue
		}
		if len(out) >= maxParts {
			break
		}
		out = append(out, Fact{ID: "f" + itoa(len(out)+1), Query: p})
	}
	if len(out) > 1 && !HasCoordination(q) {
		// 查询没声明清单：多部分是猜的（"发明创造定义"缺陷的修法）
		return []Fact{{ID: "f1", Query: q}}
	}
	if len(out) == 0 {
		return []Fact{{ID: "f1", Query: q}}
	}
	return out
}

// IsSemanticUnit 一个部分是完整可检索的需求，而非更大东西的碎片。只挡
// 实测出的错切形状：过短、被括号/书名号切穿。假阳性只多一次探测，假阴
// 性产出一个永远盖不上的幽灵需求。
func IsSemanticUnit(s string) bool {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) < minUnitRunes {
		return false
	}
	depth := 0
	for _, r := range s {
		if !strings.ContainsRune(boundaryRunes, r) {
			continue
		}
		switch r {
		case '《', '〈', '（', '(', '[', '【', '{', '“', '‘':
			depth++
		case '》', '〉', '）', ')', ']', '】', '}', '”', '’':
			depth--
		default: // 直/弯单双引号两可，出现即视为切坏
			return false
		}
		if depth < 0 {
			return false
		}
	}
	return depth == 0
}

// HasCoordination 查询是否显式声明了需求清单。 alternative 标记出现即否
// （"或者" 是条件不是清单）；多个问号各自成立时不需要标记词也算清单。
func HasCoordination(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	for _, m := range alternativeMarkers {
		if strings.Contains(q, m) {
			return false
		}
	}
	for _, m := range coordinationMarkers {
		if strings.Contains(q, m) {
			return true
		}
	}
	return strings.Count(q, "？")+strings.Count(q, "?") > 1
}

// Window 是覆盖判定能看到的最小证据面（qaflow.EvidenceWindow 的投影）。
type Window struct {
	SourceID string
	Span     string
	Text     string
	Score    float64
}

// Evaluate 逐事实判覆盖，返回最弱需求报告。covered = 占比≥0.5 **且**
// 3 字核心在窗内（cumulus 的红灯教训：半个词不许盖掉整个事实）。
func Evaluate(fx []Fact, ws []Window) Report {
	rep := Report{Facts: fx, K: len(fx), Weakest: 0}
	if len(fx) == 0 {
		return rep
	}
	allCovered := true
	weakest := 1.0
	for i := range fx {
		f := &fx[i]
		kws := contentFields(f.Query) // 内容词才算分母（胶水二元组不许
		// 稀释占比——"是多少/多少"这类把 3/7 拖成 0.43 的真跑教训）
		near := 0.0
		var supports []Window
		for j := range ws {
			hit := hitRatio(kws, ws[j].Text)
			if hit > near {
				near = hit
			}
			if hit < CoverHit || !corePresent(f.Query, ws[j].Text) {
				continue
			}
			supports = append(supports, ws[j])
		}
		sort.SliceStable(supports, func(a, b int) bool { return supports[a].Score > supports[b].Score })
		f.Supports = supports
		if len(supports) > 0 {
			f.Covered = true
			f.Score = supports[0].Score
		} else {
			f.Covered = false
			f.NearMiss = near
			rep.Missing = append(rep.Missing, f.ID)
			allCovered = false
			if near < weakest {
				weakest = near
			}
		}
	}
	rep.Complete = allCovered
	if allCovered {
		rep.Weakest = 1
	} else {
		rep.Weakest = weakest
	}
	return rep
}

// hitRatio 查询词在窗内出现的占比。
func hitRatio(kws []string, content string) float64 {
	if len(kws) == 0 {
		return 0
	}
	low := strings.ToLower(content)
	hits := 0
	for _, w := range kws {
		if w != "" && strings.Contains(low, strings.ToLower(w)) {
			hits++
		}
	}
	return float64(hits) / float64(len(kws))
}

// corePresent 事实的连续 3 字核心是否在窗内（短于 3 字的事实豁免）。
// 这是"红灯看着已覆盖"的修法：词面占比过半但事实主体不在场 = 没盖。
func corePresent(fact, content string) bool {
	r := []rune(fact)
	if len(r) < 3 {
		return true
	}
	low := strings.ToLower(content)
	for i := 0; i+2 < len(r); i++ {
		if strings.Contains(low, strings.ToLower(string(r[i:i+3]))) {
			return true
		}
	}
	return false
}

// Fields 与检索同口径的二元组分词（事实与窗口必须同一种语言比）。
func Fields(s string) []string {
	var out []string
	var run []rune
	flush := func() {
		if len(run) == 1 {
			out = append(out, string(run))
		} else if len(run) > 1 {
			for i := 0; i+1 < len(run); i++ {
				out = append(out, string(run[i:i+2]))
			}
		}
		run = nil
	}
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			run = append(run, r)
			continue
		}
		flush()
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, strings.ToLower(string(r)))
		}
	}
	flush()
	return out
}

func splitQuery(q string) []string {
	remaining := q
	var parts []string
	for {
		cutAt, cutLen := findMark(remaining)
		if cutAt < 0 {
			break
		}
		left := strings.TrimSpace(remaining[:cutAt])
		if left != "" {
			parts = append(parts, left)
		}
		remaining = remaining[cutAt+cutLen:]
	}
	remaining = strings.TrimSpace(remaining)
	if remaining != "" {
		parts = append(parts, remaining)
	}
	if len(parts) == 0 {
		return []string{q}
	}
	if len(parts) > maxParts {
		parts = parts[:maxParts]
	}
	return parts
}

func findMark(s string) (int, int) {
	bestIdx, bestLen := -1, 0
	for _, m := range splitMarks {
		i := strings.Index(strings.ToLower(s), strings.ToLower(m))
		if i < 0 {
			continue
		}
		if m == "和" || m == "与" || m == "及" {
			if utf8.RuneCountInString(s[:i]) < 2 || utf8.RuneCountInString(s[i+len(m):]) < 2 {
				continue
			}
		}
		if bestIdx < 0 || i < bestIdx {
			bestIdx, bestLen = i, len(m)
		}
	}
	return bestIdx, bestLen
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Conflict 是同一条事实的支撑窗之间的分歧（证据一致性门的产出）。
type Conflict struct {
	FactID    string   `json:"fact_id"`
	Values    []string `json:"values"`     // 各窗给出的（互不相交的）数值
	SourceIDs []string `json:"source_ids"` // 出处
}

// Conflicts 是证据一致性门的**离线可判定版**：同一条事实被多窗支撑，且
// 这些窗各自带有**互不相交的数值集**——同一事实两个出处给不同的数，就
// 是 contested（cumulus 的 evidence_agree 由 LLM 判读，我们用可断言的
// 数值分歧打底：不猜语义，只报"同一事实、不同数字"这个最硬的冲突形状；
// LLM 判读版是下一步，形状先立住）。
//
// 窗口与事实的支撑关系按覆盖判据（3 字核心在场）。无数值的事实两条窗
// 都无数值 → 不报（没有可断言的冲突）。
func Conflicts(fx []Fact, ws []Window) []Conflict {
	var out []Conflict
	for _, f := range fx {
		type ent struct {
			src  string
			nums []string
		}
		var supporting []ent
		for _, w := range ws {
			if !corePresent(f.Query, w.Text) {
				continue
			}
			nums := numbers(w.Text)
			if len(nums) == 0 {
				continue // 只有带数值的窗才参与分歧判定
			}
			supporting = append(supporting, ent{src: w.SourceID, nums: nums})
		}
		if len(supporting) < 2 {
			continue
		}
		// 两窗之间有交叠数值 → 同值，无冲突；互相不相交 → 冲突
		first := keySet(supporting[0].nums)
		for _, s := range supporting[1:] {
			if !intersects(first, keySet(s.nums)) {
				var vals, srcs []string
				for _, x := range supporting {
					vals = append(vals, x.nums...)
					srcs = append(srcs, x.src)
				}
				out = append(out, Conflict{FactID: f.ID, Values: vals, SourceIDs: srcs})
				break
			}
		}
	}
	return out
}

// numbers 抽文本里的数值 token（数字串 + 常见中文数词单位附着）。
func numbers(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '．' || r == '.':
			cur = append(cur, r)
		case len(cur) > 0 && (r == '年' || r == '月' || r == '日' || r == '万' || r == '元' || r == '%' || r == '％'):
			cur = append(cur, r)
			flush()
		default:
			flush()
		}
	}
	flush()
	return out
}

func keySet(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// contentFields 事实的内容词（ Fields 去掉沾胶水字符的二元组）。
func contentFields(q string) []string {
	raw := Fields(q)
	out := make([]string, 0, len(raw))
	for _, t := range raw {
		if t == "" || query.IsGlue(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// SupportsOf 一条事实在给定窗集里的全部支撑（按分降序）。判定与
// Evaluate 同口径（内容词占比 + 3 字核心）。合成面在**最终窗集**上现
// 算分组，不信过期报告：报告里的 supports 可能指向被驱逐掉的窗，拿着
// 过期归属喂模型，模型看到的就是"这条事实没有证据"（真跑踩过：分好组
// 的窗全被裁剪，模型答"证据为空"还违了拒答契约）。
func SupportsOf(f Fact, ws []Window) []Window {
	kws := contentFields(f.Query)
	var out []Window
	for _, w := range ws {
		if hitRatio(kws, w.Text) < CoverHit || !corePresent(f.Query, w.Text) {
			continue
		}
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

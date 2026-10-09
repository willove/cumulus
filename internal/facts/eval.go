package facts

import (
	"unicode/utf8"

	"github.com/willove/cumulus/internal/query"
	"sort"
	"strings"
)

// eval.go —— 事实的**覆盖评估**与冲突检测（哪些事实被窗口盖住、同一事实是否给了
// 不同的数）。
//
// （拆文件的理由：facts.go 原本把"怎么把问句拆成事实"与"怎么评估覆盖"放在一起。
// 前者是语言学的近似（可单测、可替换），后者是流程契约（消费方依赖它的形状）。）

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

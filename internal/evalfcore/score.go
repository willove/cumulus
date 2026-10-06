package evalfcore

import (
	"strconv"
	"strings"
	"unicode"
)

// RuleScore 规则臂：短参考答案的归一化子串 / 数值边界匹配，**非 EM**。
//
// 边界（cumulus 的教训，写死在注释里）：把整段引用当金标，规则列恒 0——
// 那是协议不匹配，不是检索失败。调用方应在跑之前看 ValidateItems 的警告。
func RuleScore(answer, gold string) float64 {
	a := normalizeText(answer)
	g := normalizeText(gold)
	if g == "" || a == "" {
		return 0
	}
	if strings.Contains(a, g) {
		return 1
	}
	if gv, err := strconv.ParseFloat(g, 64); err == nil {
		for _, tok := range strings.Fields(a) {
			tok = strings.TrimFunc(tok, func(r rune) bool { return !unicode.IsDigit(r) && r != '.' })
			if tv, err := strconv.ParseFloat(tok, 64); err == nil && tv == gv {
				return 1
			}
		}
	}
	return 0
}

// normalizeText 归一化：小写、去首尾、压缩连续空白。不动内容，
// 只让“同一文本的不同书写形式”判同。
func normalizeText(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

// EvidenceHit 证据命中：任一金标 id 出现在引用集合里即命中。
// 精确 id 匹配——同名业务键、陈旧修订都不算（cumulus 的口径）。
func EvidenceHit(citedIDs, goldIDs []string) bool {
	set := make(map[string]bool, len(citedIDs))
	for _, id := range citedIDs {
		set[id] = true
	}
	for _, g := range goldIDs {
		if set[g] {
			return true
		}
	}
	return false
}

// CitationsResolvable 全部引用可解析才算通过。resolved 与 cites 等长，
// 逐条对应。
func CitationsResolvable(resolved []bool) bool {
	if len(resolved) == 0 {
		return false // 没有引用 = 没有可核性，判不通过
	}
	for _, r := range resolved {
		if !r {
			return false
		}
	}
	return true
}

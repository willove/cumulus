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
	Judge    string   `json:"judge,omitempty"`     // 判官裁决（rescued=救回/no-support=判没支撑/error:…=判官缺席）
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

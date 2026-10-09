package docgen

// topics.go —— **选题建议**：从使用信号里挑出"值得写成文档"的主题。
//
// 为什么需要它：知识文档的质量取决于**选题**，而选题的最好来源不是拍脑袋，而是
// "**哪些问题我们答不上来 / 答了还要再问**"——这些正是知识库的空缺处（信号库里
// 一直在记：`refusal_unsatisfied` / `answer_incomplete`）。
//
// 纪律：
//
//  1. **只建议，不自动生成**：自动往语料里灌文档等于用没验证的规则污染知识库。
//     这里只给候选，人确认后走 /v1/docs。
//  2. **只用"不满意"信号，不用"满意"信号**：引用被点开（cite）说明那段知识
//     **已经够用**——把它当选题会让人去重写正在起作用的内容。
//  3. **已覆盖的主题要标出来**（不是过滤掉）：知道"这块已经有文档了"与
//     "不知道它已经存在"同样重要——前者提示"该更新而不是新建"。
//  4. **排序可解释**：分数 = Σ(权重×次数)，权重写死并列出（拒答后追问 > 答后追问）。

import (
	"sort"
	"strings"

	"github.com/willove/cumulus/internal/knowledge"
)

// 信号权重（可解释的排序依据，不藏magic number）。
const (
	// WeightRefused：拒答之后又原样问——最强的空缺信号（**弃权没解决问题**）。
	WeightRefused = 2.0
	// WeightIncomplete：答了之后又原样问——答案没答全（较弱）。
	WeightIncomplete = 1.0
)

// TopicCandidate 是一个候选主题（"值得写成文档的一块知识"）。
type TopicCandidate struct {
	Question string  `json:"question"` // 归一化问句（就是用户真正在问的那句）
	Reason   string  `json:"reason"`   // 拒答后追问 / 答后追问（若两者都有 → "两者"）
	Count    int     `json:"count"`    // 被追问了多少次
	Score    float64 `json:"score"`    // 排序分 = Σ(权重×次数)
	Covered  bool    `json:"covered"`  // 语料里已经有这个主题的生成文档
	DocID    string  `json:"doc_id,omitempty"`
}

// SuggestTopics 从信号库给出选题候选（按分数降序，最多 limit 条）。
//
// covers 用于判断"这个主题是不是已经有生成文档了"——调用方传一个查库函数，
// 本包**不依赖 store**（那会让 docgen 反向依赖持久化面）。
func SuggestTopics(st *knowledge.SignalStore, limit int, covers func(topicKey string) (string, bool)) ([]TopicCandidate, error) {
	if st == nil {
		return nil, nil // 信号库缺席：没有建议（不是错误）
	}
	if limit <= 0 {
		limit = 10
	}
	refused := tally(st, knowledge.SignalReaskAfterRefusal)
	incomplete := tally(st, knowledge.SignalReaskAfterAnswer)

	out := make([]TopicCandidate, 0, len(refused)+len(incomplete))
	for q, n := range refused {
		out = append(out, candidate(q, n, 0, true))
	}
	for q, n := range incomplete {
		if n0, dup := refused[q]; dup {
			// 两种信号都有 → 合并（分数相加，理由说清两种）
			for i := range out {
				if out[i].Question == q {
					out[i].Count += n
					out[i].Score += WeightIncomplete * float64(n)
					out[i].Reason = "拒答后追问 + 答后追问"
					_ = n0
				}
			}
			continue
		}
		out = append(out, candidate(q, 0, n, false))
	}
	// 排序：**分数降序**；同分按次数、再按字面序（读数稳定，不抖动）
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Question < out[j].Question
	})
	if len(out) > limit {
		out = out[:limit]
	}
	if covers == nil {
		return out, nil
	}
	for i := range out {
		id, ok := covers(TopicKey(out[i].Question))
		out[i].Covered, out[i].DocID = ok, id
	}
	return out, nil
}

func candidate(q string, refusedN, incompleteN int, refused bool) TopicCandidate {
	c := TopicCandidate{Question: q, Count: refusedN + incompleteN}
	if refused {
		c.Reason, c.Score = "拒答后追问", WeightRefused*float64(refusedN)
	} else {
		c.Reason, c.Score = "答后追问", WeightIncomplete*float64(incompleteN)
	}
	return c
}

// tally 按归一化问句计数（忽略空问句）。
func tally(st *knowledge.SignalStore, kind string) map[string]int {
	out := map[string]int{}
	for _, sig := range st.TopQuestions(kind, 1000) {
		q := strings.TrimSpace(sig.Question)
		if q == "" {
			continue
		}
		n := 1
		// TopQuestions 已把次数写进 Note；没有计数信息时按"至少一次"。
		if v, ok := atoi(sig.Note); ok && v > 0 {
			n = v
		}
		out[q] += n
	}
	return out
}

func atoi(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

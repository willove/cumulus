package learncore

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/willove/cumulus/internal/knowledge"
)

// UsageObservation 是**线上使用信号**折成的观察（离线评测的 Observation 之外的一条
// 入口）。
//
// 为什么要有第二条入口：现有 Observation 只吃**评测失败分类**——那需要一套题、一个
// 判官、一次跑。而线上每天都在产生另一种观察：**同一句话被反复问、答了又被追着问、
// 引用的段落被人点开**。这些信号已经在库里（SignalStore）记着，但**没有任何东西消费
// 它们**（真缺口：库在写、没人在读）。
//
// 三条纪律：
//
//  1. **只观察不推断**：这里**不做因果归因**，只把信号数成读数。谁把它变成旋钮提议，
//     要走 Hypothesizer 的白名单（模型也越界不了）。
//  2. **计数不是分数**：`ReaskedQuestions=3` 说的是"这件事发生过 3 次"，不是"它有多
//     错"。混成分数就会开始编故事。
//  3. **UNKNOWN 也算数**：只有 cite 没有 cite 不代表什么——**没被点**可能只是没人需要。
type UsageObservation struct {
	// RefusalUnsatisfied 是"拒答后又追着问"的次数：最强的不满意信号。
	RefusalUnsatisfied int `json:"refusal_unsatisfied"`
	// AnswerIncomplete 是"答了之后又问同一句"的次数（同一 session 内归一化问句相同）。
	AnswerIncomplete int `json:"answer_incomplete"`
	// CitationClicks 是引用被点开的次数：正反馈（答案确实有用）。
	CitationClicks int `json:"citation_clicks"`
	// TopRefused 是被反复拒答的问句（按次数降序，最多 N 条）。
	TopRefused []QuestionCount `json:"top_refused,omitempty"`
	// TopReasked 是被反复追问的问句。
	TopReasked []QuestionCount `json:"top_reasked,omitempty"`
	// SignalTotal 是库里信号总数（分母用：没有它，"3 次"不知道算多还是算少）。
	SignalTotal int `json:"signal_total"`
}

// QuestionCount 是一条"问句 → 次数"（读数用，可直接展示给人）。
type QuestionCount struct {
	Question string `json:"question"`
	Count    int    `json:"count"`
}

// ObserveUsage 从信号库折出线上观察。
//
// limit 控制 TopRefused/TopReasked 取几条（默认 5；读数太长没人看）。
func ObserveUsage(st *knowledge.SignalStore, limit int) (UsageObservation, error) {
	if st == nil {
		return UsageObservation{}, fmt.Errorf("learncore: nil signal store")
	}
	if limit <= 0 {
		limit = 5
	}
	c := st.Counts()
	return UsageObservation{
		RefusalUnsatisfied: c[knowledge.SignalReaskAfterRefusal],
		AnswerIncomplete:   c[knowledge.SignalReaskAfterAnswer],
		CitationClicks:     c[knowledge.SignalCitationClick],
		TopRefused:         topQuestions(st.TopQuestions(knowledge.SignalReaskAfterRefusal, limit)),
		TopReasked:         topQuestions(st.TopQuestions(knowledge.SignalReaskAfterAnswer, limit)),
		SignalTotal:        st.Len(),
	}, nil
}

// topQuestions 把库给的 Top 信号折成"问句 → 次数"。
//
// 注意：SignalStore.TopQuestions **已经把同问句聚合并把次数写进 Note**（它返回的
// 是一行一"问句"，不是一行一信号）。真跑踩过：在这里再聚合一次 → 次数被按行数重数，
// "被拒后又追问 2 次"变成 1 次——**读数自己骗自己**，而且不会报错。
func topQuestions(in []knowledge.Signal) []QuestionCount {
	out := make([]QuestionCount, 0, len(in))
	for _, sig := range in {
		q := strings.TrimSpace(sig.Question)
		if q == "" {
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSpace(sig.Note))
		if n <= 0 {
			n = 1 // 没有计数信息时按"至少一次"记，不编 0
		}
		out = append(out, QuestionCount{Question: q, Count: n})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Question < out[j].Question // 同次数按字面序：读数稳定
	})
	return out
}

// UnsatisfactionRate 是"不满意比例" = 拒答后追问 / 全部信号。
//
// **它不是满意度**：cite（正反馈）与追问（负反馈）混在一个分母里，只能当粗读的
// "有多少次交互以不满收场"。真要满意度，得把两类分开数（现在分开数没有意义——
// 没被点开的引用数是无穷大）。所以这里给的是**上限读数**：不满意占比的一个粗界。
func (o UsageObservation) UnsatisfactionRate() float64 {
	if o.SignalTotal <= 0 {
		return 0
	}
	return float64(o.RefusalUnsatisfied+o.AnswerIncomplete) / float64(o.SignalTotal)
}

// Summary 一行人读摘要（进 /v1/status 或评测摘要；线上运维先看它）。
func (o UsageObservation) Summary() string {
	return fmt.Sprintf("拒答后追问 %d · 答后追问 %d · 引用点击 %d · 不满粗界 %.0f%%",
		o.RefusalUnsatisfied, o.AnswerIncomplete, o.CitationClicks, o.UnsatisfactionRate()*100)
}

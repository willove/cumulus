package docgen

import (
	"testing"

	"github.com/willove/cumulus/internal/knowledge"
)

func sigStore() *knowledge.SignalStore {
	st := knowledge.NewSignalStore("")
	// 「太湖蓝藻」被拒答后追问 3 次（最强信号）
	for i := 0; i < 3; i++ {
		st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterRefusal, Question: "太湖蓝藻怎么治理"})
	}
	// 「差旅标准」答后追问 1 次（较弱）
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterAnswer, Question: "差旅标准是多少"})
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterAnswer, Question: "差旅标准是多少"})
	// 「专利缴费方式」两种信号都有
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterRefusal, Question: "专利缴费方式"})
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterAnswer, Question: "专利缴费方式"})
	// 引用点击是**满意信号**（那段知识已经够用）→ 不该成为选题
	st.Record(knowledge.Signal{Kind: knowledge.SignalCitationClick, Target: "d1#rune[0:4]"})
	return st
}

// 选题：只用"不满意"信号，按可解释的分数排序。
func TestSuggestTopicsRanksUnsatisfaction(t *testing.T) {
	got, err := SuggestTopics(sigStore(), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("只该有三条不满意主题（cite 不算）: %+v", got)
	}
	if got[0].Question != "太湖蓝藻怎么治理" || got[0].Score != WeightRefused*3 {
		t.Fatalf("拒答后追问（权重更高 ×3）应排第一: %+v", got[0])
	}
	if got[1].Question != "专利缴费方式" || got[1].Reason != "拒答后追问 + 答后追问" {
		t.Fatalf("两种信号都有时要合并并说明: %+v", got[1])
	}
	if got[2].Question != "差旅标准是多少" {
		t.Fatalf("较弱的信号应排最后: %+v", got[2])
	}
	// cite 不该出现
	for _, c := range got {
		if c.Question == "" {
			t.Fatalf("空问句不许进候选: %+v", got)
		}
	}
}

// 已覆盖的主题**要标出来而不是过滤掉**（提示"该更新而不是新建"）。
func TestSuggestTopicsMarksCovered(t *testing.T) {
	covers := func(key string) (string, bool) {
		if key == TopicKey("太湖蓝藻怎么治理") {
			return "gen-" + key, true
		}
		return "", false
	}
	got, err := SuggestTopics(sigStore(), 10, covers)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Covered || got[0].DocID == "" {
		t.Fatalf("已覆盖的主题必须被标出: %+v", got[0])
	}
	if got[2].Covered {
		t.Fatalf("未覆盖的不该被标: %+v", got[2])
	}
	if len(got) != 3 {
		t.Fatalf("标出不等于过滤：候选数不该变: %d", len(got))
	}
}

// limit 生效；空信号库与 nil 库都不报错（**没有建议**不是错误）。
func TestSuggestTopicsEdgeCases(t *testing.T) {
	got, _ := SuggestTopics(sigStore(), 2, nil)
	if len(got) != 2 {
		t.Fatalf("limit must apply: %d", len(got))
	}
	empty, err := SuggestTopics(knowledge.NewSignalStore(""), 5, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("空信号库 → 空建议，无错误: %+v %v", empty, err)
	}
	nilGot, err := SuggestTopics(nil, 5, nil)
	if err != nil || len(nilGot) != 0 {
		t.Fatalf("nil 信号库 → 空建议，无错误: %+v %v", nilGot, err)
	}
}

// 主题键的归一化只处理**空白与全/半角标点**，不做语义归并：
// "怎么治理" 与 "怎么治理？" 是**两个不同的候选**，但它们生成文档时若问法
// 完全一致（全角/半角）会落到**同一篇**——这是设计边界，写清楚以免误读。
func TestTopicKeyNormalizesOnlyPunctuationAndSpace(t *testing.T) {
	// 全角/半角标点等价 → 同一篇
	if TopicKey("太湖蓝藻怎么治理？") != TopicKey("太湖蓝藻怎么治理?") {
		t.Fatal("全角与半角问号必须落到同一篇")
	}
	// 空白差异等价 → 同一篇
	if TopicKey("太湖蓝藻 怎么治理") != TopicKey("太湖蓝藻怎么治理") {
		t.Fatal("空白差异必须落到同一篇")
	}
	// 语义不同 → 不同篇（不假装归并）
	if TopicKey("太湖蓝藻怎么治理") == TopicKey("太湖蓝藻怎么处置") {
		t.Fatal("不同问法不该被归并成一篇")
	}
}

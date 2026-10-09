package learncore

import (
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/knowledge"
)

// 线上信号要被折成可执行读数：三类信号各计数、Top 问句聚合、不满粗界可算。
func TestObserveUsageFoldsSignals(t *testing.T) {
	st := knowledge.NewSignalStore("")
	// 拒答后又追着问「报销」2 次
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterRefusal, Question: "报销", Session: "s1"})
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterRefusal, Question: "报销", Session: "s1"})
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterRefusal, Question: "差旅", Session: "s2"})
	// 答后追问
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterAnswer, Question: "专利期限", Session: "s3"})
	// 引用被点开（正反馈）
	st.Record(knowledge.Signal{Kind: knowledge.SignalCitationClick, Target: "d1#rune[0:4]"})
	st.Record(knowledge.Signal{Kind: knowledge.SignalCitationClick, Target: "d1#rune[9:12]"})

	obs, err := ObserveUsage(st, 5)
	if err != nil {
		t.Fatal(err)
	}
	if obs.RefusalUnsatisfied != 3 || obs.AnswerIncomplete != 1 || obs.CitationClicks != 2 {
		t.Fatalf("counts wrong: %+v", obs)
	}
	if obs.SignalTotal != 6 {
		t.Fatalf("total must be the denominator: %+v", obs)
	}
	// Top 问句：按次数降序
	if len(obs.TopRefused) != 2 || obs.TopRefused[0].Question != "报销" || obs.TopRefused[0].Count != 2 {
		t.Fatalf("top refused wrong: %+v", obs.TopRefused)
	}
	// 不满粗界 = (拒答后追问 + 答后追问) / 总信号 = 4/6
	if got := obs.UnsatisfactionRate(); got < 0.66 || got > 0.67 {
		t.Fatalf("unsatisfaction bound wrong: %v", got)
	}
	if !strings.Contains(obs.Summary(), "拒答后追问 3") {
		t.Fatalf("summary must be human-readable: %s", obs.Summary())
	}
}

// 空库/零信号不许崩也不许说满：分母为零 → 粗界 0，读数明确"没有信号"。
func TestObserveUsageOnEmptyStore(t *testing.T) {
	obs, err := ObserveUsage(knowledge.NewSignalStore(""), 5)
	if err != nil {
		t.Fatal(err)
	}
	if obs.SignalTotal != 0 || obs.UnsatisfactionRate() != 0 {
		t.Fatalf("empty store must read zero, not guess: %+v", obs)
	}
	if obs.TopRefused != nil && len(obs.TopRefused) != 0 {
		t.Fatalf("no signals → no top list: %+v", obs.TopRefused)
	}
}

// nil 信号库要**报错**（不是静默返回零读数）——"没接信号库"与"没有信号"是两件事。
func TestObserveUsageNilStoreErrors(t *testing.T) {
	if _, err := ObserveUsage(nil, 5); err == nil {
		t.Fatal("nil signal store must error (未接入 ≠ 没有信号)")
	}
}

// 同次数的排���要稳定（读数不能每次刷新都换顺序）。
func TestTopQuestionsOrderIsStable(t *testing.T) {
	st := knowledge.NewSignalStore("")
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterRefusal, Question: "b 题"})
	st.Record(knowledge.Signal{Kind: knowledge.SignalReaskAfterRefusal, Question: "a 题"})
	first, _ := ObserveUsage(st, 5)
	for i := 0; i < 3; i++ {
		again, _ := ObserveUsage(st, 5)
		if len(again.TopRefused) != len(first.TopRefused) {
			t.Fatal("length must be stable")
		}
		for j := range first.TopRefused {
			if again.TopRefused[j] != first.TopRefused[j] {
				t.Fatalf("tie order must be stable: %+v vs %+v", first.TopRefused, again.TopRefused)
			}
		}
	}
	if first.TopRefused[0].Question != "a 题" {
		t.Fatalf("ties break by literal order: %+v", first.TopRefused)
	}
}

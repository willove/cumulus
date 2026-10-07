package facts

import (
	"context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/llm"
)

// fakeCompleter 记录调用并回固定文本（判官契约测试用）。
type fakeCompleter struct {
	text string
	err  error
	seen string
}

func (f *fakeCompleter) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	f.seen = req.Prompt
	if f.err != nil {
		return llm.Response{}, f.err
	}
	return llm.Response{Text: f.text}, nil
}

// 判官兜底：词面判没盖的事实（认不出改写——"专利期"三字不连续），模型
// 判有支撑 → 补 Covered 与 Supports。
func TestRescueUpgradesLexicalMiss(t *testing.T) {
	ws := []Window{
		{SourceID: "law", Span: "rune[0:40]", Text: "第四条　　发明专利权的期限为二十年，实用新型专利权的期限为十年。", Score: 3},
		{SourceID: "other", Span: "rune[0:20]", Text: "别的东西。", Score: 1},
	}
	rep := Evaluate([]Fact{{ID: "f1", Query: "专利期限是多少年"}}, ws)
	if rep.Complete {
		t.Fatal("词面判据该判未盖（3 字核心不连续）——这是判官存在的理由")
	}
	fake := &fakeCompleter{text: `{"support":{"f1":["w1"]}}`}
	sc := &LLMScorer{Client: fake}
	if err := Rescue(&rep, ws, sc); err != nil {
		t.Fatal(err)
	}
	if !rep.Complete || len(rep.Facts[0].Supports) != 1 || rep.Facts[0].Supports[0].SourceID != "law" {
		t.Fatalf("判官该把 f1 救回来：%+v", rep.Facts[0])
	}
	// 提示词必须带事实与窗（判官不能凭空气判）
	if !strings.Contains(fake.seen, "f1") || !strings.Contains(fake.seen, "二十年") {
		t.Fatalf("提示词要有事实与窗内容：%s", fake.seen)
	}
}

// 判官说没支撑：保持未盖（兜底不是放水）。
func TestRescueKeepsMissWhenScorerSaysNo(t *testing.T) {
	ws := []Window{{SourceID: "law", Span: "rune[0:9]", Text: "无关内容。", Score: 1}}
	rep := Evaluate([]Fact{{ID: "f1", Query: "专利期限是多少年"}}, ws)
	sc := &LLMScorer{Client: &fakeCompleter{text: `{"support":{"f1":[]}}`}}
	if err := Rescue(&rep, ws, sc); err != nil {
		t.Fatal(err)
	}
	if rep.Complete {
		t.Fatal("判官判没支撑必须保持未盖")
	}
}

// 判官失败/违约：保持词面原判，不阻塞（判官缺席时流程照跑）。
func TestRescueFallsBackOnScorerFailure(t *testing.T) {
	ws := []Window{{SourceID: "law", Span: "rune[0:9]", Text: "发明专利权的期限为二十年。", Score: 1}}
	rep := Evaluate([]Fact{{ID: "f1", Query: "专利期限是多少年"}}, ws)
	sc := &LLMScorer{Client: &fakeCompleter{err: context.DeadlineExceeded}}
	if err := Rescue(&rep, ws, sc); err == nil {
		t.Fatal("失败要报出来")
	}
	if rep.Complete {
		t.Fatal("判官失败时保持词面原判")
	}
	// 违约（非严格 JSON）同理
	sc2 := &LLMScorer{Client: &fakeCompleter{text: "我也不确定"}}
	if err := Rescue(&rep, ws, sc2); err == nil {
		t.Fatal("违约要报出来")
	}
}

// 判官给不存在的窗号/不认识的事实 id：忽略（判官输出不可全信）。
func TestRescueIgnoresInventedCoordinates(t *testing.T) {
	ws := []Window{{SourceID: "law", Span: "rune[0:9]", Text: "发明专利权的期限为二十年。", Score: 1}}
	rep := Evaluate([]Fact{{ID: "f1", Query: "专利期限是多少年"}}, ws)
	sc := &LLMScorer{Client: &fakeCompleter{text: `{"support":{"f9":["w1"],"f1":["w7","w1"]}}`}}
	if err := Rescue(&rep, ws, sc); err != nil {
		t.Fatal(err)
	}
	if !rep.Complete || len(rep.Facts[0].Supports) != 1 {
		t.Fatalf("只该接受存在的窗：%+v", rep.Facts[0])
	}
}

// 无未盖事实 = 不调判官（词面判据自己够了就不劳模型）。
func TestRescueSkipsWhenNothingMissing(t *testing.T) {
	ws := []Window{{SourceID: "law", Span: "rune[0:9]", Text: "专利期限为二十年", Score: 1}}
	rep := Evaluate([]Fact{{ID: "f1", Query: "专利期限是多少年"}}, ws)
	if !rep.Complete {
		t.Skip("词面判据在这个窗上判未盖（预期外壳不成立，跳过）")
	}
	fake := &fakeCompleter{text: `{"support":{}}`}
	if err := Rescue(&rep, ws, &LLMScorer{Client: fake}); err != nil {
		t.Fatal(err)
	}
	if fake.seen != "" {
		t.Fatal("没有未盖事实不该调判官")
	}
}

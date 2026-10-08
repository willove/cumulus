package rerank

import (
	gocontext "context"
	"errors"
	"testing"

	"github.com/willove/cumulus/internal/deepcore"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/marks"
)

// stub 是假补全器：按提示词里的关键片段回指定文本，并记账。
type stub struct {
	reply   string
	err     error
	prompts []string
	calls   int
}

func (s *stub) Complete(_ gocontext.Context, req llm.Request) (llm.Response, error) {
	s.calls++
	s.prompts = append(s.prompts, req.Prompt)
	if s.err != nil {
		return llm.Response{}, s.err
	}
	return llm.Response{Text: s.reply}, nil
}

func pool(n int) []deepcore.Window {
	out := make([]deepcore.Window, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, deepcore.Window{
			SourceID: string(rune('a' + i)),
			Span:     "s",
			Text:     "片段内容" + string(rune('A'+i)),
			Score:    float64(n - i),
		})
	}
	return out
}

// 模型给的编号就是选择结果，且只挑 budget 条。
func TestSelectUsesModelPickList(t *testing.T) {
	// 分数序是 a,b,c,d,e（e 最低）；模型挑 c、a
	s := &stub{reply: "1:N 2:N 3:Y 4:N 5:Y"}
	r := &LLM{Client: s}
	got, err := r.Select(gocontext.Background(), "问题", pool(5), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SourceID != "c" || got[1].SourceID != "e" {
		t.Fatalf("must honour the model pick list: %v", ids(got))
	}
	if s.calls != 1 {
		t.Fatalf("one call per question, got %d", s.calls)
	}
	if len(r.Reason) == 0 {
		t.Fatal("rerank reason must be recorded (degraded must stay visible)")
	}
}

// 提示词必须把整池编号都列出来（清单式重排的前提：模型要能比较候选）。
func TestPromptListsWholePoolWithIndices(t *testing.T) {
	s := &stub{reply: "2"}
	r := &LLM{Client: s, RunesPerPassage: 50}
	if _, err := r.Select(gocontext.Background(), "找段落", pool(4), 2); err != nil {
		t.Fatal(err)
	}
	p := s.prompts[0]
	for _, want := range []string{"找段落", "[1]", "[2]", "[3]", "[4]", "2"} {
		if !contains(p, want) {
			t.Fatalf("prompt missing %q:\n%s", want, p)
		}
	}
}

// 模型不守规矩（乱编号/全不选/NONE）不许静默：能解析的留下，其余按顺序补齐，
// 并在 Reason 里写明补了多少。
func TestSelectFillsGarbageReplyByOrder(t *testing.T) {
	// 垃圾回包（乱编号 + 缺判）→ 判出的少 → 按分数序补齐，并记原因
	s := &stub{reply: "99:Y abc 2:Y"}
	r := &LLM{Client: s}
	got, err := r.Select(gocontext.Background(), "q", pool(4), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SourceID != "b" || got[1].SourceID != "a" {
		t.Fatalf("parseable pick first, then score order: %v", ids(got))
	}
	if !contains(r.Reason, "filled-by-order") {
		t.Fatalf("filling must be visible in the reason: %s", r.Reason)
	}
}

// 调用失败/没配 provider：显式报错，不静默退回默认选择（调用方决定降级）。
func TestSelectFailsLoudly(t *testing.T) {
	if _, err := (&LLM{Client: &stub{err: errors.New("boom")}}).Select(gocontext.Background(), "q", pool(3), 2); err == nil {
		t.Fatal("completion error must surface")
	}
	if _, err := (&LLM{}).Select(gocontext.Background(), "q", pool(3), 2); err == nil {
		t.Fatal("missing client must surface, not silently degrade")
	}
}

// 池子不比预算大时不必调用模型（省 token，也避免无谓失败面）。
func TestSelectSkipsCallWhenPoolFitsBudget(t *testing.T) {
	s := &stub{reply: "1"}
	got, err := (&LLM{Client: s}).Select(gocontext.Background(), "q", pool(2), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || s.calls != 0 {
		t.Fatalf("no call needed when the pool fits: calls=%d n=%d", s.calls, len(got))
	}
}

// 真模型的回包形状由 marks 包的测试统一钉住；这里钉"重排读得出"。
func TestRerankUsesSharedVerdictParser(t *testing.T) {
	for _, shape := range []string{"1:Y\n2:N", "1:Y 2:N", "[1]:Y [2]:N", "1 - 是  2 - 否"} {
		got := marks.Parse(shape)
		if got.Judged != 2 || len(got.Yes) != 1 || got.Yes[0] != 1 {
			t.Fatalf("shape %q must parse via marks: %+v", shape, got)
		}
	}
}

// 模型判完全部候选、只给 1 条 Y —— 那是**精确**不是偷懒，必须采信它的判断
// （真跑：那 1 条恰是含答案的；用 yes<budget/2 当懒会把它误杀成回退）。
func TestFewYesButFullyJudgedIsTrusted(t *testing.T) {
	s := &stub{reply: "1:N 2:N 3:N 4:N 5:N 6:N 7:N 8:N 9:Y"}
	r := &LLM{Client: s}
	got, err := r.Select(gocontext.Background(), "q", pool(9), 4)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got)[0] != "i" {
		t.Fatalf("the model's single yes must lead: %v", ids(got))
	}
	if contains(r.Reason, "fallback") {
		t.Fatalf("must not fall back when the model worked: %s", r.Reason)
	}
}

// 模型没干活（只判了一条就交差）才整份回退分数序：半信半疑地混用会把 top
// 窗口挤掉（真跑：evidence 85% → 40%）。
func TestNotWorkingReplyFallsBackToScoreOrder(t *testing.T) {
	s := &stub{reply: "1:Y"}
	r := &LLM{Client: s}
	got, err := r.Select(gocontext.Background(), "q", pool(9), 4)
	if err != nil {
		t.Fatal(err)
	}
	if ids(got)[0] != "a" || len(got) != 4 {
		t.Fatalf("not-working reply must fall back to score order: %v", ids(got))
	}
	if !contains(r.Reason, "fallback-score-order") {
		t.Fatalf("fallback must be visible: %s", r.Reason)
	}
}

func ids(ws []deepcore.Window) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.SourceID)
	}
	return out
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

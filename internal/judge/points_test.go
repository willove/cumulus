package judge

import (
	gocontext "context"
	"strings"
	"testing"

	"github.com/willove/cumulus/internal/llm"
)

type ptStub struct {
	reply   string
	prompts []string
}

func (s *ptStub) Complete(_ gocontext.Context, req llm.Request) (llm.Response, error) {
	s.prompts = append(s.prompts, req.Prompt)
	return llm.Response{Text: s.reply}, nil
}

// 拆点：按中文标点断，滤掉过短碎片，确定性。
func TestSplitPointsDeterministic(t *testing.T) {
	gold := "共同目标是培养创新人才。在独特性方面，数学强调数学专业化训练。数据科学突出交叉融合。"
	a, b := SplitPoints(gold), SplitPoints(gold)
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("want 3 points, got %d/%d: %v", len(a), len(b), a)
	}
	if a[0] != b[0] {
		t.Fatal("split must be deterministic")
	}
	if SplitPoints("嗯。") != nil && len(SplitPoints("嗯。")) != 1 {
		t.Fatalf("too-short gold degrades to a single point")
	}
}

// 逐点判定：命中比例过阈才判 YES；Raw 里要点/命中数要看得见。
func TestPointsJudgeUsesCoverage(t *testing.T) {
	s := &ptStub{reply: "1:Y\n2:Y\n3:N"} // 3 要点命中 2 → 0.67 ≥ 0.6 → YES
	p := &Points{Client: s}
	v, err := p.Judge("q", "答案覆盖前两点", "共同目标是 X。独特性是 Y。另一件事是 Z。")
	if err != nil {
		t.Fatal(err)
	}
	if !v.OK {
		t.Fatalf("2/3 coverage must pass at 0.6: %+v", v)
	}
	if !strings.Contains(v.Raw, "covered=2") || !strings.Contains(v.Raw, "points=3") {
		t.Fatalf("raw must show the accounting: %s", v.Raw)
	}
	// 提示词里三个要点都在
	for _, want := range []string{"[1]", "[2]", "[3]", "候选答案"} {
		if !strings.Contains(s.prompts[0], want) {
			t.Fatalf("prompt missing %q:\n%s", want, s.prompts[0])
		}
	}
}

// 命中比例不够 → NO（不是"至少一条 Y"）。
func TestPointsJudgeRejectsLowCoverage(t *testing.T) {
	s := &ptStub{reply: "1:Y\n2:N\n3:N\n4:N"}
	// 金标必须真的拆出 4 个要点（碎片 <4 字会被过滤，所以这里写完整句子）
	v, _ := (&Points{Client: s}).Judge("q", "只答了一点", "第一要点在这里。第二要点在这里。第三要点在这里。第四要点在这里。")
	if v.OK {
		t.Fatalf("1/4 coverage must fail: %+v", v)
	}
}

// 超长金标截断要留痕（不许静默）。
func TestPointsJudgeRecordsTruncation(t *testing.T) {
	long := strings.Repeat("要点内容在这里；", 40) + "。"
	// 造 30 个要点
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("要点内容在这里。")
	}
	s := &ptStub{reply: "1:Y"}
	p := &Points{Client: s, MaxPoints: 5}
	v, err := p.Judge("q", "答", b.String())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.Raw, "[truncated]") {
		t.Fatalf("truncation must be visible: %s", v.Raw)
	}
	if len(s.prompts[0]) < 10 {
		t.Fatal("prompt not built")
	}
	_ = long
}

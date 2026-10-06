package deepcore

import (
	gocontext "context"
	"testing"
)

// fakeAcquire 按页给候选：page 1 给 A、B（与查询无关）；page 2 给 C
// （覆盖查询词）。用来验证"覆盖不达标就继续取"。
func fakeAcquire(pages map[int][]Window) Acquire {
	return func(_ gocontext.Context, page int) ([]Window, error) {
		return pages[page], nil
	}
}

func covFake(query string, windows []Window) float64 {
	for _, w := range windows {
		if w.Text == query {
			return 1
		}
	}
	return 0
}

func TestRunStopsOnCoverageTarget(t *testing.T) {
	acquire := fakeAcquire(map[int][]Window{
		1: {{SourceID: "a", Span: "rune[0:2]", Text: "无关一"}},
		2: {{SourceID: "b", Span: "rune[0:2]", Text: "查询词"}},
	})
	ws, tele, err := Run(gocontext.Background(), "查询词", Options{MaxRounds: 3, CoverageTarget: 1}, acquire, covFake)
	if err != nil {
		t.Fatal(err)
	}
	if tele.StopReason != "target" || tele.Rounds != 2 {
		t.Fatalf("must stop at target on round 2: %+v", tele)
	}
	if len(ws) != 2 {
		t.Fatalf("both pages must be merged, got %d", len(ws))
	}
}

func TestRunStopsOnNoNewCandidates(t *testing.T) {
	acquire := fakeAcquire(map[int][]Window{
		1: {{SourceID: "a", Span: "rune[0:2]", Text: "无关"}},
		2: {}, // 没有新候选
	})
	_, tele, err := Run(gocontext.Background(), "查询词", Options{}, acquire, func(string, []Window) float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if tele.StopReason != "no-new" {
		t.Fatalf("must stop on no-new: %+v", tele)
	}
}

// 死路只在本问之内：第一页零增益的文档，第二页再给要跳过。
func TestRunDeadEndsSkippedWithinQuery(t *testing.T) {
	acquire := fakeAcquire(map[int][]Window{
		1: {{SourceID: "a", Span: "rune[0:2]", Text: "无关"}},
		2: {
			{SourceID: "a", Span: "rune[0:2]", Text: "无关"}, // 死路，应跳过
			{SourceID: "b", Span: "rune[0:2]", Text: "查询词"},
		},
	})
	ws, tele, err := Run(gocontext.Background(), "查询词", Options{}, acquire, covFake)
	if err != nil {
		t.Fatal(err)
	}
	if tele.DeadEnds != 1 || tele.SampledDocs != 2 {
		t.Fatalf("dead-end bookkeeping wrong: %+v", tele)
	}
	var ids []string
	for _, w := range ws {
		ids = append(ids, w.SourceID)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("dead-end must be skipped on re-appearance: %v", ids)
	}
}

// 不可回溯的候选（缺 span）直接丢，不许进窗口。
func TestRunDropsUnbackedCandidates(t *testing.T) {
	acquire := fakeAcquire(map[int][]Window{
		1: {{SourceID: "a", Span: "", Text: "查询词"}},
	})
	ws, _, err := Run(gocontext.Background(), "查询词", Options{}, acquire, covFake)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 0 {
		t.Fatalf("unbacked candidate must be dropped, got %v", ws)
	}
}

// 预算耗尽也要停：三轮都不达标，停手上有的窗口。
func TestRunStopsOnBudget(t *testing.T) {
	acquire := fakeAcquire(map[int][]Window{
		1: {{SourceID: "a", Span: "rune[0:2]", Text: "x"}},
		2: {{SourceID: "b", Span: "rune[0:2]", Text: "y"}},
		3: {{SourceID: "c", Span: "rune[0:2]", Text: "z"}},
	})
	ws, tele, err := Run(gocontext.Background(), "查询词", Options{MaxRounds: 3}, acquire, func(string, []Window) float64 { return 0.3 })
	if err != nil {
		t.Fatal(err)
	}
	if tele.StopReason != "budget" || tele.Rounds != 3 {
		t.Fatalf("budget stop wrong: %+v", tele)
	}
	if len(ws) != 3 {
		t.Fatalf("all rounds' fresh docs must accumulate, got %d", len(ws))
	}
}

// acquire 报错必须带着已取到的窗口返回（调用方决定降级还是失败）。
func TestRunAcquireErrorSurfaces(t *testing.T) {
	acquire := func(gocontext.Context, int) ([]Window, error) { return nil, gocontext.Canceled }
	if _, _, err := Run(gocontext.Background(), "q", Options{}, acquire, covFake); err == nil {
		t.Fatal("acquire error must surface")
	}
}

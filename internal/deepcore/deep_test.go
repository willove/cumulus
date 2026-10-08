package deepcore

import (
	gocontext "context"
	"fmt"
	"testing"
)

// fakeAcquire 按轮给候选：喂一个"候选带"，每轮按 (offset, limit) 切一段
// ——与真实取数（SearchWith 取 offset+limit 条再跳过前 offset 条）同形。
func fakeAcquire(pages map[int][]Window) Acquire {
	round := 0
	return func(_ gocontext.Context, offset, limit int) ([]Window, error) {
		round++
		return pages[round], nil
	}
}

// bandAcquire 按 offset/limit 从一条候选带里切：第 r 轮拿到第 r 段。
func bandAcquire(band []Window) Acquire {
	return func(_ gocontext.Context, offset, limit int) ([]Window, error) {
		if offset >= len(band) {
			return nil, nil
		}
		end := offset + limit
		if end > len(band) {
			end = len(band)
		}
		return band[offset:end], nil
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
	acquire := func(gocontext.Context, int, int) ([]Window, error) { return nil, gocontext.Canceled }
	if _, _, err := Run(gocontext.Background(), "q", Options{}, acquire, covFake); err == nil {
		t.Fatal("acquire error must surface")
	}
}

// 限额模式：池子没取满不许停——v1 在覆盖达标处就停，把预算剩在桌上，
// 窗集成了单轮 top-k 的真子集（DuReader hard 实测 61% 的题这样）。
func TestRunBudgetModeDoesNotStopAtCoverageTarget(t *testing.T) {
	band := []Window{
		{SourceID: "a", Span: "rune[0:2]", Text: "查询词"}, // 第一轮就覆盖达标
		{SourceID: "b", Span: "rune[0:2]", Text: "x"},
		{SourceID: "c", Span: "rune[0:2]", Text: "y"},
		{SourceID: "d", Span: "rune[0:2]", Text: "z"},
	}
	ws, tele, err := Run(gocontext.Background(), "查询词",
		Options{MaxRounds: 4, PageSize: 1, Budget: 2, CoverageTarget: 1}, bandAcquire(band), covFake)
	if err != nil {
		t.Fatal(err)
	}
	if tele.Pooled != 4 {
		t.Fatalf("budget mode must fill the pool, pooled=%d", tele.Pooled)
	}
	if len(ws) != 2 || tele.Selected != 2 {
		t.Fatalf("must select exactly the budget, got %d (tele %+v)", len(ws), tele)
	}
	if ws[0].SourceID != "a" {
		t.Fatalf("selection must keep the coverage-carrying window, got %v", ws)
	}
}

// 选择阶段挑的是**互补**的窗口，不是分数最高的几条：分数最高的两条
// 覆盖同一批词，第三、第四条才把新词带进来。
func TestSelectPrefersComplementaryWindows(t *testing.T) {
	pool := []Window{
		{SourceID: "hi1", Span: "s", Text: "甲", Score: 9},
		{SourceID: "hi2", Span: "s", Text: "甲", Score: 8},
		{SourceID: "lo1", Span: "s", Text: "乙", Score: 1},
	}
	cov := func(_ string, ws []Window) float64 {
		seen := map[string]bool{}
		for _, w := range ws {
			seen[w.Text] = true
		}
		return float64(len(seen)) / 2
	}
	got := selectByCoverage("q", pool, 2, cov)
	if len(got) != 2 {
		t.Fatalf("want 2 windows, got %d", len(got))
	}
	ids := map[string]bool{}
	for _, w := range got {
		ids[w.SourceID] = true
	}
	if !ids["hi1"] || !ids["lo1"] {
		t.Fatalf("must pick the complementary pair, got %v", ids)
	}
	if got[0].Score < got[1].Score {
		t.Fatalf("presentation order must be score-desc, got %v", got)
	}
}

// 池子干了（没新候选）在限额模式下也停，别空转轮次。
func TestRunBudgetModeStopsWhenPoolExhausted(t *testing.T) {
	band := []Window{{SourceID: "a", Span: "s", Text: "x"}}
	_, tele, err := Run(gocontext.Background(), "q",
		Options{MaxRounds: 5, PageSize: 3, Budget: 9}, bandAcquire(band), func(string, []Window) float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if tele.StopReason != "pool-exhausted" || tele.Rounds != 2 {
		t.Fatalf("must stop as soon as the pool dries up: %+v", tele)
	}
}

// 注入的选择器优先于默认的覆盖贪心——"选谁"是策略，不是循环内建。
func TestRunUsesInjectedSelector(t *testing.T) {
	band := []Window{}
	for i := 0; i < 9; i++ {
		band = append(band, Window{SourceID: fmt.Sprintf("d%d", i), Span: "s", Text: fmt.Sprintf("t%d", i), Score: float64(9 - i)})
	}
	picked, _, err := Run(gocontext.Background(), "q",
		Options{MaxRounds: 3, PageSize: 3, Budget: 3},
		bandAcquire(band),
		func(string, []Window) float64 { return 0 },
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(picked) != 3 {
		t.Fatalf("selector output must honour the budget, got %d", len(picked))
	}
	// 覆盖函数对所有窗口一视同仁（增益全 0）⇒ 同增益按分数降序
	if picked[0].SourceID != "d0" {
		t.Fatalf("ties must fall back to the higher BM25 score, got %s", picked[0].SourceID)
	}

	// 注入选择器：故意挑最高分的两条 + 一条中间的
	sel := func(_ gocontext.Context, _ string, pool []Window, budget int) ([]Window, error) {
		return []Window{pool[0], pool[1], pool[2]}, nil
	}
	got, _, err := Run(gocontext.Background(), "q",
		Options{MaxRounds: 3, PageSize: 3, Budget: 3, Selector: sel},
		bandAcquire(band), func(string, []Window) float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	if got[0].SourceID != "d0" || got[2].SourceID != "d2" {
		t.Fatalf("injected selector must win: %v %v %v", got[0].SourceID, got[1].SourceID, got[2].SourceID)
	}
}

// 选择器失败必须显性报错（带已取窗口），不许悄悄退回默认选择。
func TestRunSelectorErrorSurfaces(t *testing.T) {
	band := []Window{
		{SourceID: "a", Span: "s", Text: "x"},
		{SourceID: "b", Span: "s", Text: "y"},
		{SourceID: "c", Span: "s", Text: "z"},
	}
	_, _, err := Run(gocontext.Background(), "q",
		Options{MaxRounds: 3, PageSize: 1, Budget: 1,
			Selector: func(gocontext.Context, string, []Window, int) ([]Window, error) {
				return nil, gocontext.Canceled
			}},
		bandAcquire(band), func(string, []Window) float64 { return 0 })
	if err == nil {
		t.Fatal("selector error must surface, not silently degrade")
	}
}

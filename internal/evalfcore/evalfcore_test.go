package evalfcore

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/willove/cumulus/internal/failure"
	"github.com/willove/cumulus/internal/store"
)

// ---- 测试替身 ----

type memStore struct {
	states map[string]RunState
}

func newMemStore() *memStore { return &memStore{states: map[string]RunState{}} }

func (m *memStore) SaveRun(_ context.Context, s RunState) error {
	// 深拷贝，防止调用方后续改切片时污染“已落盘”的档案
	cp := s
	cp.Results = append([]ItemResult(nil), s.Results...)
	m.states[s.RunID] = cp
	return nil
}

func (m *memStore) LoadRun(_ context.Context, runID string) (RunState, error) {
	s, ok := m.states[runID]
	if !ok {
		return RunState{}, ErrRunNotFound
	}
	return s, nil
}

// stubExecutor 每题返回一个可编排的产出。黄金标对它不可见（签名里没有）。
type stubExecutor struct {
	outcomes map[string]ItemOutcome
	seen     []string // 记录收到过的问题——用来证明黄金标没漏进执行面
}

func (e *stubExecutor) Answer(_ context.Context, q string) (ItemOutcome, error) {
	e.seen = append(e.seen, q)
	if o, ok := e.outcomes[q]; ok {
		return o, nil
	}
	return ItemOutcome{Answer: "", Refused: true, RouteAction: "refuse"}, nil
}

func testFP() Fingerprints {
	return Fingerprints{ItemsSHA: "i1", CorpusSHA: "c1", ConfigSHA: "g1"}
}

func testItems() []Item {
	return []Item{
		{ID: "q1", Question: "连接池最大连接数是多少", Answer: "100", GoldIDs: []string{"law-1"}},
		{ID: "q2", Question: "默认端口是多少", Answer: "8484", GoldIDs: []string{"ops-1"}},
	}
}

// ---- 题集与指纹 ----

func TestDatasetContentAddressed(t *testing.T) {
	a, err := NewDataset(testItems())
	if err != nil {
		t.Fatal(err)
	}
	// 打乱题序：同一批内容是同一个题集
	b, err := NewDataset([]Item{testItems()[1], testItems()[0]})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("same content must give same id: %s vs %s", a.ID, b.ID)
	}
	// 改一个字节就是新题集
	c, _ := NewDataset([]Item{testItems()[0], {ID: "q2", Question: "默认端口是多少", Answer: "8485", GoldIDs: []string{"ops-1"}}})
	if c.ID == a.ID {
		t.Fatal("different content must give different id")
	}
}

func TestDatasetRejectsBadItems(t *testing.T) {
	if _, err := NewDataset([]Item{{ID: "q1", Question: "?", Answer: ""}}); err == nil {
		t.Fatal("empty gold must be rejected")
	}
	if _, err := NewDataset([]Item{{ID: "q1", Question: "?", Answer: "a"}, {ID: "q1", Question: "?", Answer: "b"}}); err == nil {
		t.Fatal("duplicate ids must be rejected")
	}
}

func TestValidateWarnsLongGold(t *testing.T) {
	long := make([]rune, 201)
	for i := range long {
		long[i] = '法'
	}
	items := []Item{{ID: "q1", Question: "x", Answer: string(long), GoldIDs: []string{"d1"}}}
	if diags := ValidateItems(items); len(diags) != 0 {
		t.Fatalf("long gold must not block the run, got %v", diags)
	}
	if warns := WarnItems(items); len(warns) != 1 {
		t.Fatalf("long gold must warn once, got %v", warns)
	}
	// 无金标 docid → 证据神谕不可用（警告，不是致命）
	if warns := WarnItems([]Item{{ID: "q2", Question: "x", Answer: "y"}}); len(warns) != 1 {
		t.Fatalf("missing gold ids must warn once, got %v", warns)
	}
	// 真致命的是重复题号与空字段
	if diags := ValidateItems([]Item{{ID: "a", Question: "x", Answer: "y"}, {ID: "a", Question: "x", Answer: "y"}}); len(diags) != 1 {
		t.Fatalf("duplicate id must stay fatal, got %v", diags)
	}
}

func TestFingerprintDiffReasons(t *testing.T) {
	fp := testFP()
	if reasons := fp.DiffReasons(fp); len(reasons) != 0 {
		t.Fatalf("identical fingerprints must be comparable, got %v", reasons)
	}
	other := fp
	other.CorpusSHA = "c2"
	if reasons := fp.DiffReasons(other); len(reasons) != 1 || reasons[0] != "corpus_sha differs" {
		t.Fatalf("want corpus reason, got %v", reasons)
	}
}

func TestMaskHost(t *testing.T) {
	got := MaskHost("https://user:pass@api.example.com:8443/v1?key=secret")
	if got != "api.example.com:8443" {
		t.Fatalf("host must be masked, got %q", got)
	}
}

// ---- 规则臂 ----

func TestRuleScoreSubstringAndNumeric(t *testing.T) {
	if RuleScore("最大连接数为 100，超过需调整", "100") != 1 {
		t.Fatal("numeric boundary must match")
	}
	if RuleScore("答案是 200", "100") != 0 {
		t.Fatal("different number must not match")
	}
	if RuleScore("连接池最大连接数默认为一百", "连接池") != 1 {
		t.Fatal("normalized substring must match")
	}
	if RuleScore("完全不相关", "连接池") != 0 {
		t.Fatal("no overlap must score 0")
	}
	// 协议边界：整段金标恒 0
	if RuleScore("《中华人民共和国刑法》第三条规定…", "《中华人民共和国刑法》第三条规定…（全文）") != 0 {
		t.Fatal("long gold must score 0 by protocol")
	}
}

func TestEvidenceHitExactID(t *testing.T) {
	if !EvidenceHit([]string{"law-1", "ops-1"}, []string{"law-1"}) {
		t.Fatal("exact id must hit")
	}
	if EvidenceHit([]string{"law-1"}, []string{"law-1-v2"}) {
		t.Fatal("revision id must not hit same-key predecessor")
	}
}

// ---- 运行状态机 ----

func TestRunHappyPathPersistsPerItem(t *testing.T) {
	st := newMemStore()
	ex := &stubExecutor{outcomes: map[string]ItemOutcome{
		"连接池最大连接数是多少": {
			Answer: "100", RouteAction: "fast",
			Cited: []Citation{{DocID: "law-1", Span: "rune[0:10]", Resolved: true}},
		},
		"默认端口是多少": {
			Answer: "8484", RouteAction: "fast",
			Cited: []Citation{{DocID: "ops-1", Span: "rune[0:5]", Resolved: true}},
		},
	}}
	r := NewRunner(st, testFP(), ex, nil)
	state, err := r.Start(context.Background(), "run-1", testItems())
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != StatusDone || state.ItemsDone != 2 {
		t.Fatalf("want done 2/2, got %s %d", state.Status, state.ItemsDone)
	}
	// 金标没有漏进执行面：executor 只见过问题
	for _, q := range ex.seen {
		if q != "连接池最大连接数是多少" && q != "默认端口是多少" {
			t.Fatalf("executor saw something beyond questions: %q", q)
		}
	}
	s := Summarize(state)
	if s.RuleAvg != 1 || s.EvidenceHitRate != 1 || s.CitationsOKRate != 1 {
		t.Fatalf("all arms should be perfect: %s", s)
	}
	if s.JudgeN != 0 {
		t.Fatal("no judge wired must be N/A, not 0")
	}
}

func TestInterruptedRunIsNotColdReplayed(t *testing.T) {
	st := newMemStore()
	// 预设一个在飞的运行（模拟进程被杀前的落盘）
	_ = st.SaveRun(context.Background(), RunState{RunID: "run-x", Status: StatusRunning, Fingerprints: testFP(), ItemsTotal: 2, ItemsDone: 1})
	r := NewRunner(st, testFP(), &stubExecutor{}, nil)
	state, err := r.Start(context.Background(), "run-x", testItems())
	if err != nil {
		t.Fatalf("interrupted replay must not error: %v", err)
	}
	if state.Status != StatusInterrupted || state.ItemsDone != 1 {
		t.Fatalf("want interrupted 1/2, got %s %d", state.Status, state.ItemsDone)
	}
	// 二次启动：interrupted 不能续跑
	if _, err := r.Start(context.Background(), "run-x", testItems()); !errors.Is(err, ErrStartNewRun) {
		t.Fatalf("want ErrStartNewRun, got %v", err)
	}
}

// 单题失败不杀全场：记成 eval-error 项，其余题照跑（真跑教训：300 题
// 两次都在 130-250 题处被单题杀死，12 分钟全废）。
func TestSingleItemErrorDoesNotKillRun(t *testing.T) {
	st := newMemStore()
	ex := &failOnSecondExecutor{}
	r := NewRunner(st, testFP(), ex, nil)
	state, err := r.Start(context.Background(), "run-f", testItems())
	if err != nil {
		t.Fatalf("single item error must not kill the run: %v", err)
	}
	if state.Status != StatusDone {
		t.Fatalf("run must complete, got %s", state.Status)
	}
	if state.ItemErrors != 1 {
		t.Fatalf("item error must be counted, got %d", state.ItemErrors)
	}
	var found bool
	for _, res := range state.Results {
		if res.ItemID == testItems()[1].ID && len(res.Failure) > 9 && res.Failure[:10] == "eval-error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed item must be recorded with reason: %+v", state.Results[1])
	}
}

// 系统性故障：开头全失败 → 停（key 错/端点挂，重试无意义）。
func TestSystemicFailureAbortsEarly(t *testing.T) {
	st := newMemStore()
	ex := &alwaysFailExecutor{}
	r := NewRunner(st, testFP(), ex, nil)
	_, err := r.Start(context.Background(), "run-s", testItems())
	if err == nil {
		t.Fatal("systemic failure must abort")
	}
	saved, _ := st.LoadRun(context.Background(), "run-s")
	if saved.Status != StatusFailed {
		t.Fatalf("want failed, got %s", saved.Status)
	}
}

type failOnSecondExecutor struct{ n int }

func (e *failOnSecondExecutor) Answer(_ context.Context, _ string) (ItemOutcome, error) {
	e.n++
	if e.n == 2 {
		return ItemOutcome{}, errors.New("upstream exploded")
	}
	return ItemOutcome{Answer: "100", RouteAction: "fast"}, nil
}

type alwaysFailExecutor struct{}

func (e *alwaysFailExecutor) Answer(_ context.Context, _ string) (ItemOutcome, error) {
	return ItemOutcome{}, errors.New("endpoint down")
}

// ---- 可比性 ----

func TestCompareOnlyWhenFingerprintsMatch(t *testing.T) {
	a := RunState{Fingerprints: testFP(), Results: []ItemResult{{RuleScore: 1, EvidenceHit: true, CitationsTotal: 1, CitationsResolved: 1}}}
	b := RunState{Fingerprints: testFP(), Results: []ItemResult{{RuleScore: 0}}}
	diff, reasons := Compare(a, b)
	if len(reasons) != 0 {
		t.Fatalf("same fingerprints must be comparable, got %v", reasons)
	}
	if diff["rule_avg"] != 1 {
		t.Fatalf("want rule diff 1, got %v", diff["rule_avg"])
	}
	b.Fingerprints.CorpusSHA = "other"
	diff, reasons = Compare(a, b)
	if len(reasons) == 0 || diff != nil {
		t.Fatalf("incomparable must give reasons and no numbers, got %v / %v", diff, reasons)
	}
}

// ---- 失败归类 ----

func TestClassifyRules(t *testing.T) {
	cases := []struct {
		name string
		in   ClassifyInput
		want string
	}{
		{"budget", ClassifyInput{BudgetExhausted: true}, "budget-exceeded"},
		{"unresolved", ClassifyInput{Windows: 3, UnresolvedCitations: 1}, "grounding-fail"},
		// 底物错配（BioHarness）：要数值、有窗、窗里没有数
		{"substrate numeric", ClassifyInput{Windows: 2, AsksNumeric: true, EvidenceHasNumeric: false}, "substrate-mismatch"},
		// 查询的实体语料里没有：需要实体接地，不是再翻文本
		{"substrate oov", ClassifyInput{Windows: 2, QueryOutOfCorpus: true}, "substrate-mismatch"},
		// 连窗都没有也算错配：库没命名这个实体，跟"检索没够到"是两种病
		{"substrate oov no windows", ClassifyInput{Windows: 0, QueryOutOfCorpus: true, Refused: true}, "substrate-mismatch"},
		// 窗里有数就不是底物问题——宁可落到召回/路由，不许滥用错配
		{"numeric but evidence has numbers", ClassifyInput{Windows: 2, AsksNumeric: true, EvidenceHasNumeric: true, GoldHit: true, RouteAction: "escalate", RuleScore: 1}, "unclassified"},
		// 拒答不再一律算腐烂：金标在窗内却拒 = 过度拒答（路由误判）
		{"refused with evidence", ClassifyInput{Windows: 2, GoldHit: true, Refused: true}, "route-error"},
		// 空手拒答：出口是对的，账记在召回上
		{"refused no windows", ClassifyInput{Refused: true}, "recall-miss"},
		// 有窗、没命中、预算没用完就放弃 = 提前给不确定答案
		{"refused early with windows", ClassifyInput{Windows: 2, Refused: true}, "rot"},
		{"no windows", ClassifyInput{Windows: 0}, "recall-miss"},
		{"gold miss", ClassifyInput{Windows: 2, GoldHit: false}, "recall-miss"},
		{"route", ClassifyInput{Windows: 2, GoldHit: true, RouteAction: "fast", RuleScore: 0}, "route-error"},
		{"none", ClassifyInput{Windows: 2, GoldHit: true, RouteAction: "escalate", RuleScore: 1}, "unclassified"},
	}
	for _, c := range cases {
		got := FormatCategory(Classify(c.in))
		if got != c.want {
			t.Errorf("%s: want %s, got %s", c.name, c.want, got)
		}
	}
}

// 六类必须都是可达的：定义了判据却没有规则返回它 = 诊断词汇表变装饰。
// 这条测试用最小输入逐个触达六类——加规则时若把某类挤成死标签，这里红。
func TestAllCategoriesReachable(t *testing.T) {
	reached := map[string]bool{}
	inputs := []ClassifyInput{
		{BudgetExhausted: true},
		{Windows: 1, UnresolvedCitations: 1},
		{Windows: 1, AsksNumeric: true},
		{Windows: 1, GoldHit: true, Refused: true},
		{Windows: 1, Refused: true},
		{},
	}
	for _, in := range inputs {
		reached[FormatCategory(Classify(in))] = true
	}
	for _, c := range failure.All() {
		if !reached[c.String()] {
			t.Errorf("failure category %q is unreachable by the classifier rules", c)
		}
	}
}

// ---- 真存储上的原子落盘 ----

func TestRunStoreOnCumulite(t *testing.T) {
	p, err := store.Open("", true)
	if err != nil {
		t.Fatal(err)
	}
	st := NewArchive(p)
	r := NewRunner(st, testFP(), &stubExecutor{outcomes: map[string]ItemOutcome{
		"连接池最大连接数是多少": {Answer: "100", RouteAction: "fast",
			Cited: []Citation{{DocID: "law-1", Resolved: true}}},
		"默认端口是多少": {Answer: "8484", RouteAction: "fast",
			Cited: []Citation{{DocID: "ops-1", Resolved: true}}},
	}}, nil)
	if _, err := r.Start(context.Background(), "run-kv", testItems()); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadRun(context.Background(), "run-kv")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDone || !reflect.DeepEqual(Summarize(got), Summarize(RunState{Results: got.Results})) {
		t.Fatalf("persisted run must round-trip: %+v", got)
	}
	if _, err := st.LoadRun(context.Background(), "nope"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("missing run must be ErrRunNotFound, got %v", err)
	}
}

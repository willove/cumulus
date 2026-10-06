package learncore

import (
	"context"
	"errors"
	"testing"

	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/knowledge/belief"
	"github.com/willove/cumulus/internal/store"
)

// ---- 旋钮白名单 ----

func TestRegistryRejectsOutOfWhitelist(t *testing.T) {
	k, _ := NewKnob("evidence.topk", 1, 10, 3)
	reg := NewRegistry(k)
	if _, ok := reg.Get("evidence.width"); ok {
		t.Fatal("unregistered knob must not resolve")
	}
	reasons := (&Cycle{Reg: reg}).applyToRegistry(map[string]float64{"evidence.width": 5})
	if len(reasons) != 1 {
		t.Fatalf("out-of-whitelist proposal must be rejected whole, got %v", reasons)
	}
}

func TestKnobBoundsEnforced(t *testing.T) {
	if _, err := NewKnob("k", 1, 10, 0); err == nil {
		t.Fatal("initial out of bounds must fail")
	}
	k, _ := NewKnob("k", 1, 10, 3)
	if err := k.Set(11); err == nil {
		t.Fatal("set above max must fail")
	}
	if err := k.Set(0.5); err == nil {
		t.Fatal("set below min must fail")
	}
}

func TestRestoreRollsBack(t *testing.T) {
	k, _ := NewKnob("k", 1, 10, 3)
	reg := NewRegistry(k)
	snap := reg.Snapshot()
	_ = k.Set(9)
	if err := reg.Restore(snap); err != nil {
		t.Fatal(err)
	}
	if k.Value() != 3 {
		t.Fatalf("restore must bring back 3, got %v", k.Value())
	}
}

// ---- 护栏 ----

func TestGuardrailRateFloor(t *testing.T) {
	g := Guardrail{EvidenceHitFloor: 0.8, CitationsOKFloor: 0.8}
	ok, reasons := g.Check(Baseline{EvidenceHitRate: 0.9, CitationsOKRate: 0.85})
	if !ok {
		t.Fatalf("above floor must pass, got %v", reasons)
	}
	ok, reasons = g.Check(Baseline{EvidenceHitRate: 0.7, CitationsOKRate: 0.85})
	if ok || len(reasons) != 1 {
		t.Fatalf("below floor must fail with reasons, got ok=%v reasons=%v", ok, reasons)
	}
}

func TestGuardrailFloorsFromBaselineZeroTolerance(t *testing.T) {
	g := FloorsFromBaseline(Baseline{EvidenceHitRate: 0.75, CitationsOKRate: 1})
	ok, _ := g.Check(Baseline{EvidenceHitRate: 0.75, CitationsOKRate: 1})
	if !ok {
		t.Fatal("at floor must pass (rate floor, not slack)")
	}
}

// ---- 离线假设 ----

func TestOfflineHypothesizerDirections(t *testing.T) {
	h := OfflineHypothesizer{TopKKnob: "evidence.topk", WidthKnob: "evidence.width", TopKStep: 1, WidthStep: 20}
	obs := Observation{
		ItemsDone:     2,
		FailureCounts: map[string]int{"recall-miss": 2, "grounding-fail": 1},
		Knobs:         map[string]float64{"evidence.topk": 3, "evidence.width": 60},
	}
	p, err := h.Propose(obs)
	if err != nil {
		t.Fatal(err)
	}
	if p.Knobs["evidence.topk"] != 4 {
		t.Fatalf("recall-miss dominant must push topk +1 step, got %v", p.Knobs)
	}
	// 没有失败可归因：学不到就直说
	if _, err := h.Propose(Observation{ItemsDone: 2, Knobs: map[string]float64{"evidence.topk": 3}}); !errors.Is(err, ErrNothingToLearn) {
		t.Fatalf("want ErrNothingToLearn, got %v", err)
	}
}

// ---- 受管变更全流程 ----

// knobExecutor 的产出由旋钮决定：topk >= 4 才给可命中证据，
// 且第二题的引用故意不可解析——真实系统总有不完美的候选。
type knobExecutor struct {
	topk float64
}

func (e *knobExecutor) Answer(_ context.Context, q string) (evalfcore.ItemOutcome, error) {
	if e.topk >= 4 {
		return evalfcore.ItemOutcome{
			Answer: "gold-ish", RouteAction: "escalate",
			Cited: []evalfcore.Citation{{DocID: "any", Resolved: q == "a?"}},
		}, nil
	}
	return evalfcore.ItemOutcome{Refused: true, RouteAction: "refuse"}, nil
}

func testItems2() []evalfcore.Item {
	return []evalfcore.Item{
		{ID: "q1", Question: "a?", Answer: "x", GoldIDs: []string{"any"}},
		{ID: "q2", Question: "b?", Answer: "y", GoldIDs: []string{"any"}},
	}
}

func testFingerprints() evalfcore.Fingerprints {
	return evalfcore.Fingerprints{ItemsSHA: "i", CorpusSHA: "c", ConfigSHA: "g"}
}

// 提升路径：候选过了护栏，旋钮真的改了，记录 promoted。
func TestCyclePromotes(t *testing.T) {
	reg, _ := newTestRegistry(3)
	st := newMemStore2()
	cyc := &Cycle{
		Reg:          reg,
		Guard:        FloorsFromBaseline(Baseline{EvidenceHitRate: 0.5, CitationsOKRate: 0.5}),
		Hypo:         OfflineHypothesizer{TopKKnob: "evidence.topk", WidthKnob: "evidence.width", TopKStep: 1, WidthStep: 20},
		Base:         Baseline{EvidenceHitRate: 0.5, CitationsOKRate: 0.5},
		Fingerprints: testFingerprints(),
		RunStore:     st,
		NewExec: func(knobs map[string]float64) evalfcore.Executor {
			return &knobExecutor{topk: knobs["evidence.topk"]}
		},
	}
	obs := Observation{
		ItemsDone:     2,
		FailureCounts: map[string]int{"recall-miss": 2},
		Knobs:         reg.Snapshot(),
	}
	rec, err := cyc.RunWithObservation(context.Background(), obs, testItems2(), "cyc-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != "promoted" {
		t.Fatalf("want promoted, got %s reasons=%v", rec.Verdict, rec.Reasons)
	}
	if got := reg.Get2("evidence.topk"); got != 4 {
		t.Fatalf("topk must be promoted to 4, got %v", got)
	}
	if rec.KnobsBefore["evidence.topk"] != 3 || rec.KnobsAfter["evidence.topk"] != 4 {
		t.Fatalf("record must carry before/after: %+v", rec)
	}
}

// 拒绝路径：候选不过护栏，旋钮一个都没动。
func TestCycleRejectsWhenGuardrailFails(t *testing.T) {
	reg, _ := newTestRegistry(3)
	st := newMemStore2()
	cyc := &Cycle{
		Reg:          reg,
		Guard:        Guardrail{EvidenceHitFloor: 0.99, CitationsOKFloor: 0.99}, // 候选永远到不了
		Hypo:         OfflineHypothesizer{TopKKnob: "evidence.topk", WidthKnob: "evidence.width", TopKStep: 1, WidthStep: 20},
		Base:         Baseline{EvidenceHitRate: 0.5, CitationsOKRate: 0.5},
		Fingerprints: testFingerprints(),
		RunStore:     st,
		NewExec: func(knobs map[string]float64) evalfcore.Executor {
			return &knobExecutor{topk: knobs["evidence.topk"]}
		},
	}
	obs := Observation{ItemsDone: 2, FailureCounts: map[string]int{"recall-miss": 2}, Knobs: reg.Snapshot()}
	rec, err := cyc.RunWithObservation(context.Background(), obs, testItems2(), "cyc-2")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != "rejected" || len(rec.Reasons) == 0 {
		t.Fatalf("want rejected with reasons, got %s %v", rec.Verdict, rec.Reasons)
	}
	if got := reg.Get2("evidence.topk"); got != 3 {
		t.Fatalf("rejected cycle must not touch knobs, got %v", got)
	}
}

// 无护栏基线拒跑。
func TestCycleRefusesWithoutBaseline(t *testing.T) {
	reg, _ := newTestRegistry(3)
	cyc := &Cycle{
		Reg: reg, Base: Baseline{}, RunStore: newMemStore2(),
		Hypo:         OfflineHypothesizer{TopKKnob: "evidence.topk", WidthKnob: "evidence.width", TopKStep: 1, WidthStep: 20},
		Fingerprints: testFingerprints(),
	}
	rec, err := cyc.RunWithObservation(context.Background(), Observation{ItemsDone: 2, FailureCounts: map[string]int{"recall-miss": 1}, Knobs: reg.Snapshot()}, testItems2(), "cyc-3")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Verdict != "rejected" || rec.Reasons[0] == "" {
		t.Fatalf("no baseline must refuse, got %s %v", rec.Verdict, rec.Reasons)
	}
}

// 白名单外的提议整组拒绝，一个旋钮都不动。
func TestCycleRejectsWholeProposalOnWhitelistViolation(t *testing.T) {
	reg, _ := newTestRegistry(3)
	cyc := &Cycle{
		Reg: reg, Base: Baseline{EvidenceHitRate: 0.5, CitationsOKRate: 0.5},
		Guard:        FloorsFromBaseline(Baseline{EvidenceHitRate: 0.5, CitationsOKRate: 0.5}),
		RunStore:     newMemStore2(),
		Fingerprints: testFingerprints(),
		Hypo:         badHypo{},
		NewExec:      func(map[string]float64) evalfcore.Executor { return &knobExecutor{topk: 4} },
	}
	rec, _ := cyc.RunWithObservation(context.Background(), Observation{ItemsDone: 2, Knobs: reg.Snapshot()}, testItems2(), "cyc-4")
	if rec.Verdict != "rejected" {
		t.Fatalf("whitelist violation must reject, got %s", rec.Verdict)
	}
	if got := reg.Get2("evidence.topk"); got != 3 {
		t.Fatalf("knobs must be untouched, got %v", got)
	}
}

type badHypo struct{}

func (badHypo) Propose(Observation) (Proposal, error) {
	return Proposal{Knobs: map[string]float64{"not.in.whitelist": 1}, Reason: "malicious"}, nil
}

// 周期档案在真存储上往返。
func TestCycleStoreRoundTrip(t *testing.T) {
	p, err := store.Open("", true)
	if err != nil {
		t.Fatal(err)
	}
	cs := NewKVCycleStore(p)
	rec := CycleRecord{ID: "c-rt", Verdict: "promoted", KnobsAfter: map[string]float64{"evidence.topk": 4}}
	if err := cs.SaveCycle(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := cs.LoadCycle(context.Background(), "c-rt")
	if err != nil {
		t.Fatal(err)
	}
	if got.Verdict != "promoted" || got.KnobsAfter["evidence.topk"] != 4 {
		t.Fatalf("round trip broken: %+v", got)
	}
	if _, err := cs.LoadCycle(context.Background(), "nope"); !errors.Is(err, ErrCycleNotFound) {
		t.Fatalf("missing cycle must be ErrCycleNotFound, got %v", err)
	}
}

// ---- 测试辅助 ----

func newTestRegistry(topk float64) (*Registry, error) {
	k, err := NewKnob("evidence.topk", 1, 10, topk)
	if err != nil {
		return nil, err
	}
	w, err := NewKnob("evidence.width", 20, 400, 60)
	if err != nil {
		return nil, err
	}
	return NewRegistry(k, w), nil
}

// Get2 是测试用的读值捷径。
func (r *Registry) Get2(name string) float64 {
	k, _ := r.Get(name)
	return k.Value()
}

type memStore2 struct{ m map[string]evalfcore.RunState }

func newMemStore2() *memStore2 { return &memStore2{m: map[string]evalfcore.RunState{}} }

func (s *memStore2) SaveRun(_ context.Context, st evalfcore.RunState) error {
	cp := st
	cp.Results = append([]evalfcore.ItemResult(nil), st.Results...)
	s.m[st.RunID] = cp
	return nil
}

func (s *memStore2) LoadRun(_ context.Context, id string) (evalfcore.RunState, error) {
	st, ok := s.m[id]
	if !ok {
		return evalfcore.RunState{}, evalfcore.ErrRunNotFound
	}
	return st, nil
}

// ObserveBelief：命中的文档后验上升，陪跑的下降，没引用的不观测。
func TestObserveBeliefFromRun(t *testing.T) {
	items := []evalfcore.Item{
		{ID: "q3", Question: "成本结构怎么样", Answer: "优化", GoldIDs: []string{"fin-1"}},
	}
	run := evalfcore.RunState{Results: []evalfcore.ItemResult{
		{ItemID: "q3", EvidenceHit: true, CitedDocs: []string{"cost-a", "fin-1"}},
	}}
	b := belief.New(nil, 0.5)
	n := ObserveBelief(b, run, items)
	if n != 2 {
		t.Fatalf("want 2 observations (only cited docs), got %d", n)
	}
	if p, _ := b.Get("fin-1"); p != 1 {
		t.Fatalf("gold doc must reach 1, got %v", p)
	}
	if p, _ := b.Get("cost-a"); p != 0 {
		t.Fatalf("non-yielding doc must sink to 0, got %v", p)
	}
	if _, ok := b.Get("cost-b"); ok {
		t.Fatal("uncited doc must not be observed at all")
	}
	// nil 信念不许 panic
	if ObserveBelief(nil, run, items) != 0 {
		t.Fatal("nil belief must be a no-op")
	}
}

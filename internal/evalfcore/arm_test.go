package evalfcore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 哑对照臂不变量（v0.2 §三.7）：对照运行必须至少含一个最笨基线臂。
// 缺了它就不是"跑不了"，是**不可解释**——任何改进数字都无法归因。
func TestValidateArmsRequiresDumb(t *testing.T) {
	ok := func(context.Context) (RunState, error) { return RunState{}, nil }
	cases := []struct {
		name    string
		arms    []Arm
		wantErr string
	}{
		{name: "no arms", arms: nil, wantErr: "no arms registered"},
		{
			name:    "no dumb arm",
			arms:    []Arm{{ID: "deep", Run: ok}, {ID: "cascade", Run: ok}},
			wantErr: "no dumb baseline arm",
		},
		{
			name:    "empty id",
			arms:    []Arm{{ID: "", Dumb: true, Run: ok}},
			wantErr: "empty id",
		},
		{
			name:    "no executor",
			arms:    []Arm{{ID: "bm25-bare", Dumb: true}},
			wantErr: "has no executor",
		},
		{
			name: "dumb present",
			arms: []Arm{{ID: "bm25-bare", Dumb: true, Run: ok}, {ID: "deep", Run: ok}},
		},
	}
	for _, c := range cases {
		err := ValidateArms(c.arms)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("%s: want nil, got %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
	}
}

// 校验不过就不许开跑：缺哑臂时**一题都不跑**（不烧钱跑完再说）。
func TestRunArmsRejectsBeforeRunning(t *testing.T) {
	ran := 0
	arms := []Arm{{ID: "deep", Run: func(context.Context) (RunState, error) {
		ran++
		return RunState{}, nil
	}}}
	if _, err := RunArms(context.Background(), arms); err == nil {
		t.Fatal("RunArms must refuse a comparison set without a dumb arm")
	}
	if ran != 0 {
		t.Fatalf("no arm may run before validation; %d ran", ran)
	}
}

// 注册顺序即执行顺序，结果按注册返回；单臂失败带上臂 id 报错。
func TestRunArmsOrderAndError(t *testing.T) {
	arms := []Arm{
		{ID: "bm25-bare", Dumb: true, Run: func(context.Context) (RunState, error) {
			return RunState{RunID: "bare"}, nil
		}},
		{ID: "deep", Run: func(context.Context) (RunState, error) {
			return RunState{RunID: "deep"}, nil
		}},
	}
	states, err := RunArms(context.Background(), arms)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0].RunID != "bare" || states[1].RunID != "deep" {
		t.Fatalf("registration order must be execution order: %+v", states)
	}

	arms[1].Run = func(context.Context) (RunState, error) { return RunState{}, errors.New("boom") }
	if _, err := RunArms(context.Background(), arms); err == nil || !strings.Contains(err.Error(), "deep") {
		t.Fatalf("arm failure must name the arm, got %v", err)
	}
}

// 校准读数：τ₀ 候选按口径给，分桶单调性对"能不能拿置信度画阈值"给判据。
func TestCalibrationTableAndTau0(t *testing.T) {
	run := RunState{Results: []ItemResult{
		{ItemID: "a", Confidence: 0.05, EvidenceHit: false, RouteAction: "escalate"},
		{ItemID: "b", Confidence: 0.15, EvidenceHit: false, RouteAction: "escalate"},
		{ItemID: "c", Confidence: 0.85, EvidenceHit: true},
		{ItemID: "d", Confidence: 0.95, EvidenceHit: true},
		{ItemID: "e", Confidence: 0.9, Failure: "eval-error: transport"},
	}}
	bs := CalibrationTable(run, 5)
	if bs[0].N != 2 || bs[0].Escalate != 2 || bs[0].Accuracy != 0 {
		t.Fatalf("low bucket wrong: %+v", bs[0])
	}
	if bs[4].N != 2 || bs[4].Accuracy != 1 {
		t.Fatalf("high bucket wrong (eval-error must be excluded): %+v", bs[4])
	}
	if !CalibrationMonotone(bs) {
		t.Fatal("this synthetic run is monotone; the checker must say so")
	}
	// 执行失败项不进校准：4 条有效题（不是 5 条）
	total := 0
	for _, b := range bs {
		total += b.N
	}
	if total != 4 {
		t.Fatalf("eval-error items must be excluded from calibration, counted %d", total)
	}

	tau, oracle := SuggestedTau0(run)
	if oracle != "evidence-hit-rate" || tau != 0.5 {
		t.Fatalf("no judge → evidence-hit-rate oracle, got %v (%s)", tau, oracle)
	}
	yes := true
	run.Results[0].JudgeOK = &yes
	if _, oracle := SuggestedTau0(run); oracle != "judge-accuracy" {
		t.Fatalf("judge present → judge oracle, got %s", oracle)
	}

	// 非单调（高桶更差）必须被抓住
	bad := []Bucket{{N: 1, Accuracy: 0.9}, {N: 1, Accuracy: 0.1}}
	if CalibrationMonotone(bad) {
		t.Fatal("descending buckets must fail the monotonicity check")
	}
}

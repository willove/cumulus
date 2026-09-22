package facts

import (
	"testing"

	"github.com/cumubase/ask/internal/mcs"
)

func TestBuildSingleFact(t *testing.T) {
	fx := Build("连接池最大连接数")
	if len(fx) != 1 {
		t.Fatalf("K=%d want 1", len(fx))
	}
	if fx[0].ID != "f1" || fx[0].Query != "连接池最大连接数" {
		t.Fatalf("fact=%+v", fx[0])
	}
}

func TestBuildMultiHopSplits(t *testing.T) {
	fx := Build("路由器配置 和 交换机配置")
	if len(fx) < 2 {
		t.Fatalf("K=%d want >=2 for conjunction query", len(fx))
	}
	joined := ""
	for _, f := range fx {
		joined += f.Query
	}
	if !contains(joined, "路由器") || !contains(joined, "交换机") {
		t.Fatalf("facts lost content: %+v", fx)
	}
}

func TestEvaluateWeakestMissing(t *testing.T) {
	fx := Build("路由器配置 和 交换机配置")
	samples := []mcs.Sample{
		{Content: "路由器配置 步骤 一 二 三", Score: 8, Source: "s1", Start: 0, End: 12},
	}
	rep := Evaluate(fx, samples)
	if rep.Complete {
		t.Fatal("must not be complete with only one fact covered")
	}
	if len(rep.Missing) == 0 {
		t.Fatal("missing must be non-empty")
	}
	if rep.Weakest != 0 {
		t.Fatalf("weakest=%v want 0 when a fact is open", rep.Weakest)
	}
}

func TestEvaluateCompleteWhenAllCovered(t *testing.T) {
	fx := Build("路由器配置 和 交换机配置")
	samples := []mcs.Sample{
		{Content: "路由器配置 步骤 一 二", Score: 8, Source: "s1", Start: 0, End: 8},
		{Content: "交换机配置 步骤 三 四", Score: 7, Source: "s2", Start: 0, End: 8},
	}
	rep := Evaluate(fx, samples)
	if !rep.Complete {
		t.Fatalf("want complete, missing=%v facts=%+v", rep.Missing, rep.Facts)
	}
	if rep.Weakest <= 0 {
		t.Fatalf("weakest=%v want >0", rep.Weakest)
	}
}

func TestNeedContinueBudget(t *testing.T) {
	fx := Build("A 和 B")
	rep := Evaluate(fx, nil)
	if !NeedContinue(rep, 1, 6) {
		t.Fatal("open fact + budget left → continue")
	}
	if NeedContinue(rep, 6, 6) {
		t.Fatal("budget exhausted → stop")
	}
	rep.Complete = true
	if NeedContinue(rep, 1, 6) {
		t.Fatal("complete → stop")
	}
}

func TestMissingQueries(t *testing.T) {
	fx := Build("路由器配置 和 交换机配置")
	samples := []mcs.Sample{
		{Content: "路由器配置 步骤", Score: 8, Source: "s1", Start: 0, End: 4},
	}
	rep := Evaluate(fx, samples)
	mq := MissingQueries(fx, rep)
	if len(mq) == 0 {
		t.Fatal("want missing queries for self-correction")
	}
	for _, q := range mq {
		if contains(q, "路由器") {
			t.Fatalf("covered fact should not be in missing: %v", mq)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

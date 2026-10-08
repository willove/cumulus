package calib

import "testing"

func s(c float64, ok bool) Sample { return Sample{Confidence: c, Correct: ok} }

// 完美排序 = 1.0；完全反着 = 0.0；全并列 = 0.5。
func TestAUCKnownValues(t *testing.T) {
	if got := AUC([]Sample{s(0.9, true), s(0.8, true), s(0.2, false), s(0.1, false)}); math1(got) != 1.0 {
		t.Fatalf("perfect separation must be 1.0, got %v", got)
	}
	if got := AUC([]Sample{s(0.1, true), s(0.2, false)}); math1(got) != 0.0 {
		t.Fatalf("inverted signal must be 0.0, got %v", got)
	}
	if got := AUC([]Sample{s(0.5, true), s(0.5, false)}); math1(got) != 0.5 {
		t.Fatalf("all ties must be 0.5, got %v", got)
	}
}

// 单类标签/样本太少时不能假装有区分度。
func TestAUCDegenerate(t *testing.T) {
	if AUC(nil) != 0.5 || AUC([]Sample{s(0.9, true)}) != 0.5 || AUC([]Sample{s(0.1, false)}) != 0.5 {
		t.Fatal("degenerate inputs must report 0.5")
	}
}

// Lift@20%：最高分那 20% 全对、整体 50% → lift 2.0。
func TestLiftAt(t *testing.T) {
	samples := []Sample{s(0.9, true), s(0.8, true), s(0.5, false), s(0.4, false), s(0.3, false), s(0.2, false)}
	// top 20% = 1.2 → take 1（rounded）→ 全对；整体 2/6=0.33 → lift 3
	if got := LiftAt(samples, 0.2); got < 2.0 {
		t.Fatalf("top slice must lift above base, got %v", got)
	}
	if got := LiftAt(samples, 1.0); got < 1.0 {
		t.Fatalf("full-set lift is 1 by definition, got %v", got)
	}
}

func math1(v float64) float64 { return float64(int(v*100+0.5)) / 100 }

package facts

import "testing"

// The 2026-09-28 measured case this file pins: a live run on the 727k-rune
// novel asked "孙悟空在斜月三星洞跟随谁学艺？学成了哪些本领？" — two separate
// questions, no marker word. The model decomposed it into exactly the two
// atomic requirements, and the coordination gate vetoed all of it, so the
// ~837-token call bought nothing and K stayed 1.

func TestQuestionClausesLicenseCoordination(t *testing.T) {
	q := "孙悟空在斜月三星洞跟随谁学艺？学成了哪些本领？"
	if !HasCoordination(q) {
		t.Fatal("two separate questions must license a requirement list")
	}
	parts := []string{"孙悟空在斜月三星洞跟随谁学艺", "孙悟空在斜月三星洞学成了哪些本领"}
	fx, why := BuildPartsWhy(q, parts)
	if len(fx) != 2 || why != WhyNone {
		t.Fatalf("the measured split must survive intact: K=%d why=%q", len(fx), why)
	}
	if fx[0].Query != parts[0] || fx[1].Query != parts[1] {
		t.Fatalf("order/content changed: %v", fx)
	}
}

func TestSingleQuestionStillUncoordinated(t *testing.T) {
	q := "孙悟空的兵器是什么？"
	if HasCoordination(q) {
		t.Fatal("one question licenses no list")
	}
	// A multi-part split of a single question is still the measured unlicensed
	// defect — the veto fires, and the reason is named for the verbose log.
	fx, why := BuildPartsWhy(q, []string{"孙悟空的兵器是什么", "金箍棒有多重"})
	if fx != nil || why != WhyCoordination {
		t.Fatalf("single question must veto K>1: %v why=%q", fx, why)
	}
}

// The gate's original measured protection must survive the new license: a bare
// term-of-art lookup carries no interrogative and no marker, so K=1 by
// construction.
func TestBareTermOfArtStillVetoed(t *testing.T) {
	if HasCoordination("发明创造定义") {
		t.Fatal("a bare term of art licenses no list")
	}
	fx, why := BuildPartsWhy("发明创造定义", []string{"发明的定义", "创造的定义"})
	if fx != nil || why != WhyCoordination {
		t.Fatalf("the 发明创造 miscut must stay vetoed: %v why=%q", fx, why)
	}
}

// An alternative marker disqualifies even a multi-question query — the
// either/or reading dominates whatever the question count says.
func TestAlternativeDisqualifiesMultipleQuestions(t *testing.T) {
	if HasCoordination("他去取经还是回家？为什么？") {
		t.Fatal("还是 is an alternative, not coordination — even with two questions")
	}
}

// Halfwidth question marks license the same as fullwidth: the marker set is
// punctuation-normalized the way the rest of the gate already lowercases.
func TestHalfwidthQuestionClausesCount(t *testing.T) {
	if !HasCoordination("who taught him?what did he learn?") {
		t.Fatal("two halfwidth questions must license a list")
	}
}

// The units reason is distinct from the coordination reason: IsSemanticUnit
// rejecting every part names "units", so a fallback line in the log tells the
// two failure modes apart.
func TestWhyDistinguishesUnitsFromCoordination(t *testing.T) {
	fx, why := BuildPartsWhy("孙悟空的兵器是什么", []string{"参", "\"右"})
	if fx != nil || why != WhyUnits {
		t.Fatalf("all-fragments input names units: %v why=%q", fx, why)
	}
}

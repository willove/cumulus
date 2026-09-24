package eval

import "testing"

// Correct: a numeric gold must not match a LONGER number. The old substring
// rule scored gold "128" against "1280" / "12800" — and gold answers in this
// suite are overwhelmingly numeric claims, so EM was inflated exactly where it
// matters most.
func TestCorrectNumericBoundary(t *testing.T) {
	cases := []struct {
		gold, got string
		want      bool
	}{
		{"128", "连接池最大 128", true},
		{"128", "连接池最大 128，超时 30 秒", true},
		{"128", "上限是(128)。", true},
		{"128", "上限为128。", true},
		{"128", "连接池最大 1280", false},
		{"128", "连接池最大 12800", false},
		{"128", "5128 也行", false},
		{"3.5", "版本 3.50 发布", false},
		{"3.5", "版本 3.5 发布", true},
		{"30", "超时 30 秒", true},
	}
	for _, c := range cases {
		if got := Correct(c.gold, c.got); got != c.want {
			t.Errorf("Correct(%q,%q)=%v, want %v", c.gold, c.got, got, c.want)
		}
	}
}

// Non-numeric gold keeps substring semantics: natural-language answers
// legitimately wrap the gold phrase.
func TestCorrectNonNumericKeepsSubstring(t *testing.T) {
	if !Correct("连接池", "连接池最大 128") {
		t.Fatal("non-numeric gold must still match as a substring")
	}
	if !Correct("广州", "系统部署在广州机房") {
		t.Fatal("non-numeric gold must still match embedded")
	}
	if Correct("连接池配置说明", "连接池") {
		t.Fatal("gold longer than the answer cannot match")
	}
	if Correct("", "128") || Correct("128", "") {
		t.Fatal("empty sides must be false")
	}
}

// Ev.Rec must be EXACT over canonical keys. The old two-way Contains made
// gold "law1" count as retrieved by "law10".
func TestEvRecNoSubstringFalsePositive(t *testing.T) {
	it := Item{ID: "q", Query: "x", Answer: "128", Gold: []string{"law1"}}
	s := Score(it, Prediction{Answer: "128", SourceIDs: []string{"law10"}})
	if s.EvRec {
		t.Fatal("gold law1 must NOT count as retrieved by law10")
	}
	// Exact hit still works.
	s2 := Score(it, Prediction{Answer: "128", SourceIDs: []string{"law1"}})
	if !s2.EvRec {
		t.Fatal("exact gold hit must count")
	}
	// And the internal-id form of the SAME document must still match: that is
	// the one legitimate mismatch canonicalization covers.
	s3 := Score(it, Prediction{Answer: "128", SourceIDs: []string{"src:law1#3"}})
	if !s3.EvRec {
		t.Fatal("src:law1#3 must canonicalize to law1")
	}
	// src: prefix alone is not a false positive either.
	s4 := Score(it, Prediction{Answer: "128", SourceIDs: []string{"src:law10#3"}})
	if s4.EvRec {
		t.Fatal("src:law10#3 must not canonicalize onto law1")
	}
}

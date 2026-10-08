package marks

import "testing"

// 真模型回包的形状都要认（重排与判官两侧都踩过）。
func TestParseAcceptsRealShapes(t *testing.T) {
	cases := []struct {
		in     string
		yes    int
		no     int
		judged int
	}{
		{"1:Y\n2:N\n3:N", 1, 2, 3},
		{"1:Y 2:N", 1, 1, 2},
		{"[1]:Y [2]:N", 1, 1, 2},
		{"1 - Y  2 - N", 1, 1, 2},
		{"1:是 2:否", 1, 1, 2},
		{"1:Y，2:N、3:Y", 2, 1, 3},
		{"garbage only", 0, 0, 0},
		{"", 0, 0, 0},
	}
	for _, c := range cases {
		got := Parse(c.in)
		if len(got.Yes) != c.yes || len(got.No) != c.no || got.Judged != c.judged {
			t.Errorf("Parse(%q) = %+v, want yes=%d no=%d judged=%d", c.in, got, c.yes, c.no, c.judged)
		}
	}
}

func TestParseCoversOnlyYes(t *testing.T) {
	r := Parse("1:Y 2:N 5:Y")
	if !r.Covers(1) || !r.Covers(5) || r.Covers(2) || r.Covers(3) {
		t.Fatalf("covers must follow the yes-list: %+v", r)
	}
}

// ParseIDs：纯编号回包（"哪几条直接给了答案 → 1,3"）。
func TestParseIDs(t *testing.T) {
	got := ParseIDs("1, 3")
	if len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("bare id list must parse: %v", got)
	}
	if len(ParseIDs("NONE")) != 0 {
		t.Fatal("NONE means no id picked")
	}
	if len(ParseIDs("没有一条直接给出答案")) != 0 {
		t.Fatal("prose reply means no id picked")
	}
}

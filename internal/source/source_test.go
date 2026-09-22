package source

import (
	"strings"
	"testing"
)

func TestNormalizeAndDigestIdempotent(t *testing.T) {
	a := New("t", "md", "file://a", "k1", "zh", "hello   world\r\nfoo", nil)
	b := New("t", "md", "file://a", "k1", "zh", "hello world\nfoo", nil)
	if a.Digest != b.Digest {
		t.Fatalf("digest differs after normalize: %s vs %s", a.Digest, b.Digest)
	}
	if a.ID != b.ID {
		t.Fatalf("id differs: %s vs %s", a.ID, b.ID)
	}
	if a.Body != "hello world\nfoo" {
		t.Fatalf("body = %q", a.Body)
	}
}

func TestStructureMapsBackToOriginal(t *testing.T) {
	body := "# Intro\nalpha beta\n--- page 2 ---\ngamma\n## Detail\ndelta\n"
	s := New("t", "md", "", "", "en", body, nil)
	if len(s.Structure) < 3 {
		t.Fatalf("want >=3 spans, got %d: %+v", len(s.Structure), s.Structure)
	}
	total := 0
	for i, sp := range s.Structure {
		got, err := SliceSpan(s.Body, s.Structure, i)
		if err != nil {
			t.Fatalf("span %d: %v", i, err)
		}
		if len(got) != sp.End-sp.Start {
			t.Fatalf("span %d slice len %d want %d", i, len(got), sp.End-sp.Start)
		}
		total += len(got)
	}
	if total != len([]rune(s.Body)) {
		t.Fatalf("spans cover %d runes, body %d", total, len([]rune(s.Body)))
	}
	joined := ""
	for _, sp := range s.Structure {
		joined += sp.Label + "|"
	}
	if !strings.Contains(joined, "Intro") || !strings.Contains(joined, "Detail") {
		t.Fatalf("heading labels missing: %s", joined)
	}
	if !strings.Contains(joined, "p2") {
		t.Fatalf("page label missing: %s", joined)
	}
}

func TestSliceSpanRejectsOutOfRange(t *testing.T) {
	s := New("t", "md", "", "", "en", "abc", nil)
	if _, err := SliceSpan(s.Body, s.Structure, 99); err == nil {
		t.Fatal("want error for out-of-range span")
	}
}

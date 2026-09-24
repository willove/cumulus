package main

import "testing"

func TestFlagsFirst(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"flags first already", []string{"-job", "b", "f.jsonl"}, []string{"-job", "b", "f.jsonl"}},
		{"flags after positional", []string{"f.jsonl", "-job", "b"}, []string{"-job", "b", "f.jsonl"}},
		{"bool flag needs no value", []string{"f.jsonl", "-recursive"}, []string{"-recursive", "f.jsonl"}},
		{"bool flag then positional", []string{"-recursive", "f.jsonl"}, []string{"-recursive", "f.jsonl"}},
		{"equals form", []string{"f.jsonl", "-job=b"}, []string{"-job=b", "f.jsonl"}},
		{"mixed", []string{"a.jsonl", "b.jsonl", "-body", "content", "-recursive"},
			[]string{"-body", "content", "-recursive", "a.jsonl", "b.jsonl"}},
		{"dashdash terminates", []string{"a.jsonl", "--", "-weird"}, []string{"a.jsonl", "--", "-weird"}},
		{"dashdash keeps earlier flags", []string{"-job", "b", "a.jsonl", "--", "-weird"},
			[]string{"-job", "b", "a.jsonl", "--", "-weird"}},
		{"lone dash is positional", []string{"-job", "-", "x"}, []string{"-job", "-", "x"}},
		{"trailing flag with no value", []string{"f.jsonl", "-job"}, []string{"-job", "f.jsonl"}},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := flagsFirst(c.in, "recursive")
			if len(got) != len(c.want) {
				t.Fatalf("len: got %v want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v want %v", got, c.want)
				}
			}
		})
	}
}

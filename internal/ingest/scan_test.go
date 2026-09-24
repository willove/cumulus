package ingest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// scanFixture builds a deterministic tree: two .md (one fresh, one old),
// a .txt, a .csv (wrong ext), a hidden file, an empty .md, a PDF oversize,
// and a nested subdir .md (only seen when recursive).
func scanFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(p, body string, age time.Duration) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := time.Now().Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "fresh.md"), "# 部署手册\n\n连接池最大连接数为 200，端口 8484。", 24*time.Hour)
	write(filepath.Join(root, "old.md"), "# 归档说明\n\n历史档案。", 30*24*time.Hour)
	write(filepath.Join(root, "notes.txt"), "值班 runbook：先看网关。", 2*time.Hour)
	write(filepath.Join(root, "data.csv"), "a,b\n1,2\n", time.Hour)
	write(filepath.Join(root, ".hidden.md"), "# 隐藏\n", time.Hour)
	write(filepath.Join(root, "empty.md"), "", time.Hour)
	write(filepath.Join(root, "big.pdf"), "%PDF-1.4 fake", time.Hour) // oversize below via MaxSize
	write(filepath.Join(root, "sub", "nested.md"), "# 嵌套文档\n\n子目录内容。", 3*time.Hour)
	return root
}

func TestScanDirRules(t *testing.T) {
	root := scanFixture(t)
	rep, err := ScanDir(root, ScanOptions{Recursive: true})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// fresh.md, notes.txt, sub/nested.md — old.md dropped by the default
	// freshness window? No: default is off; it is kept. Adjusting below.
	got := map[string]bool{}
	for _, c := range rep.Candidates {
		got[filepath.Base(c.Path)] = true
	}
	for _, want := range []string{"fresh.md", "notes.txt", "nested.md", "old.md", "big.pdf"} {
		if !got[want] {
			t.Fatalf("candidates missing %s: %v", want, rep.Candidates)
		}
	}
	for _, bad := range []string{"data.csv", ".hidden.md", "empty.md"} {
		if got[bad] {
			t.Fatalf("candidate %s must be skipped", bad)
		}
	}
	if rep.Skipped["ext"] != 1 || rep.Skipped["hidden"] != 1 || rep.Skipped["empty"] != 1 || rep.Skipped["oversized"] != 0 {
		t.Fatalf("skip accounting: %v", rep.Skipped)
	}
	// Newest first (big.pdf 1h old beats notes.txt 2h); fresh.md before old.md.
	if rep.Candidates[0].Path != filepath.Join(root, "big.pdf") {
		t.Fatalf("order: want big.pdf first, got %s", rep.Candidates[0].Path)
	}
	if got2 := pathIndex(rep.Candidates, filepath.Join(root, "fresh.md")); got2 > pathIndex(rep.Candidates, filepath.Join(root, "old.md")) {
		t.Fatalf("fresh.md must rank before old.md: %v", rep.Candidates)
	}
	// Text candidates carry a headline; age is computed.
	for _, c := range rep.Candidates {
		if filepath.Base(c.Path) == "notes.txt" {
			if c.Headline == "" || c.AgeDays != 0 {
				t.Fatalf("headline/age: %+v", c)
			}
		}
	}
	// PDF headline stays empty (binary).
	for _, c := range rep.Candidates {
		if c.Ext == ".pdf" && c.Headline != "" {
			t.Fatalf("binary headline must be empty: %+v", c)
		}
	}
}

func pathIndex(cands []Candidate, p string) int {
	for i, c := range cands {
		if c.Path == p {
			return i
		}
	}
	return len(cands)
}

func TestScanDirFreshnessAndSize(t *testing.T) {
	root := scanFixture(t)
	rep, err := ScanDir(root, ScanOptions{Recursive: true, NewerThan: 7 * 24 * time.Hour, MaxSize: 1 << 20})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, c := range rep.Candidates {
		if filepath.Base(c.Path) == "old.md" {
			t.Fatalf("older-than must drop old.md")
		}
	}
	if rep.Skipped["old"] != 1 {
		t.Fatalf("old skip: %v", rep.Skipped)
	}
	// big.pdf is 14 bytes — a 13-byte cap marks every bigger file oversized.
	rep2, err := ScanDir(root, ScanOptions{Recursive: true, MaxSize: 13})
	if err != nil {
		t.Fatalf("scan2: %v", err)
	}
	var oversize, kept int
	for _, c := range rep2.Candidates {
		if c.Size > 13 {
			t.Fatalf("candidate over the cap leaked: %+v", c)
		}
		kept++
	}
	oversize = rep2.Skipped["oversized"]
	if oversize == 0 || kept+oversize != 5 {
		t.Fatalf("oversized=%d kept=%d (5 text-ish files)", oversize, kept)
	}
}

func TestScanDirNonRecursive(t *testing.T) {
	root := scanFixture(t)
	rep, err := ScanDir(root, ScanOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, c := range rep.Candidates {
		if filepath.Base(c.Path) == "nested.md" {
			t.Fatalf("non-recursive must not see sub/nested.md")
		}
	}
}

func TestStratifyIsDeterministicAndFair(t *testing.T) {
	cands := []Candidate{
		{Path: "a.md", Ext: ".md"}, {Path: "b.md", Ext: ".md"}, {Path: "c.md", Ext: ".md"},
		{Path: "d.txt", Ext: ".txt"}, {Path: "e.txt", Ext: ".txt"},
		{Path: "f.pdf", Ext: ".pdf"},
	}
	got := Stratify(cands, 4)
	if len(got) != 4 {
		t.Fatalf("limit: %d", len(got))
	}
	// Round-robin by extension (alphabetical bucket order): md, pdf, txt, md —
	// not the first four paths.
	var order []string
	for _, c := range got {
		order = append(order, c.Ext)
	}
	want := []string{".md", ".pdf", ".txt", ".md"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("stratified order = %v, want %v", order, want)
		}
	}
	// Stable across calls.
	again := Stratify(cands, 4)
	for i := range got {
		if got[i].Path != again[i].Path {
			t.Fatalf("not deterministic at %d", i)
		}
	}
	if len(Stratify(cands, 0)) != len(cands) {
		t.Fatalf("limit 0 must keep all")
	}
}

func TestApplyRankReordersAndDegrades(t *testing.T) {
	rep := ScanReport{Candidates: []Candidate{
		{Path: "a.md"}, {Path: "b.md"}, {Path: "c.md"},
	}}
	// Ranker picks b then a — c keeps rule order at the end.
	ranker := func(ctx context.Context, q string, cands []Candidate) ([]string, error) {
		return []string{"b.md", "a.md", "zzz-unknown.md"}, nil
	}
	if err := ApplyRank(context.Background(), &rep, "主题", ranker); err != nil {
		t.Fatalf("apply rank: %v", err)
	}
	var paths []string
	for _, c := range rep.Candidates {
		paths = append(paths, c.Path)
	}
	want := []string{"b.md", "a.md", "c.md"}
	for i := range want {
		if paths[i] != want[i] {
			t.Fatalf("ranked order = %v, want %v", paths, want)
		}
	}
	// A failing ranker must leave the rule order untouched.
	failing := func(ctx context.Context, q string, cands []Candidate) ([]string, error) {
		return nil, context.DeadlineExceeded
	}
	if err := ApplyRank(context.Background(), &rep, "主题", failing); err == nil {
		t.Fatalf("failing ranker must surface its error")
	}
	if rep.Candidates[0].Path != "b.md" {
		t.Fatalf("failed rank must not reorder: %v", rep.Candidates)
	}
}

func TestCandidateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scan.json")
	cf := CandidateFile{Dir: "/x", Candidates: []Candidate{{Path: "/x/a.md"}, {Path: "/x/b.md"}}}
	b, err := json.Marshal(cf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	back, err := LoadCandidateFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(back.Paths()) != 2 || back.Paths()[0] != "/x/a.md" {
		t.Fatalf("paths: %v", back.Paths())
	}
	// A bare path array also loads.
	bare := filepath.Join(dir, "bare.json")
	if err := os.WriteFile(bare, []byte(`["/x/c.md"]`), 0o644); err != nil {
		t.Fatal(err)
	}
	back2, err := LoadCandidateFile(bare)
	if err != nil {
		t.Fatalf("load bare: %v", err)
	}
	if len(back2.Paths()) != 1 || back2.Paths()[0] != "/x/c.md" {
		t.Fatalf("bare paths: %v", back2.Paths())
	}
}

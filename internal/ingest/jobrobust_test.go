package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

// H2: one unreadable/unextractable file must not strand the rest of a job, and
// the cursor must advance past it so a resumed run does not re-fail forever.
// Before the fix a single bad file marked the whole job failed with the cursor
// never moving, so every retry died on the same file.
func TestIngestFileListSkipsBadFilesAndAdvances(t *testing.T) {
	st, _ := newTestStore(t)
	dir := t.TempDir()
	good1 := filepath.Join(dir, "a.md")
	good2 := filepath.Join(dir, "b.md")
	if err := os.WriteFile(good1, []byte("连接池最大 128。"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good2, []byte("端口是 8480。"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An unextractable DOCX: a valid zip container is required, so plain text
	// fails extraction and must be skipped rather than failing the job.
	badDocx := filepath.Join(dir, "c.docx")
	if err := os.WriteFile(badDocx, []byte("not a zip container"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A PDF whose text stream yields nothing (encrypted/CID-font shape).
	badPDF := filepath.Join(dir, "d.pdf")
	if err := os.WriteFile(badPDF, []byte("%PDF-1.4 encrypted binary"), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := st.IngestFiles(context.Background(), dir, false, "j1")
	if err != nil {
		t.Fatalf("a bad file must not fail the job: %v", err)
	}
	if n != 2 {
		t.Fatalf("ingested=%d, want 2 (the two readable md files)", n)
	}
	doc, err := st.GetJobDoc(context.Background(), "j1")
	if err != nil {
		t.Fatal(err)
	}
	if doc.State != "done" {
		t.Fatalf("state=%q, want done", doc.State)
	}
	if doc.Done != 4 || doc.Total != 4 {
		t.Fatalf("progress done=%d total=%d, want 4/4 (processed, incl. skipped)", doc.Done, doc.Total)
	}
	if doc.Skipped != 2 {
		t.Fatalf("skipped=%d, want 2", doc.Skipped)
	}
	if doc.SkipReasons["extract_failed"] != 1 || doc.SkipReasons["empty"] != 1 {
		t.Fatalf("skip reasons not auditable: %v", doc.SkipReasons)
	}
	// The cursor is past the skipped files: a resume is a no-op, not a re-fail.
	n2, err := st.IngestFiles(context.Background(), dir, false, "j1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("resume ingested=%d, want 0", n2)
	}
}

// H2: the Job path must store bodies over the synchronous cap. SSOT §3.4.2
// sends over-cap documents to the Job precisely so they can be stored; the old
// code routed them through Put and rejected them forever.
func TestJobPathStoresOverSyncCapBodies(t *testing.T) {
	st, _ := newTestStore(t)
	dir := t.TempDir()
	big := filepath.Join(dir, "big.md")
	body := ""
	for len(body) <= MaxSyncBodyBytes+4096 {
		body += "连接池最大 128，超时 30 秒。\n"
	}
	if err := os.WriteFile(big, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), source.New("big.md", "md", "", "big.md", "zh", body, nil)); err == nil {
		t.Fatal("precondition: synchronous Put must still reject an over-cap body")
	}
	n, err := st.IngestFiles(context.Background(), dir, false, "big")
	if err != nil {
		t.Fatalf("job path must accept an over-cap body: %v", err)
	}
	if n != 1 {
		t.Fatalf("ingested=%d, want 1", n)
	}
}

// H3: two same-named files in different directories used to collapse onto one
// business key (the base name), the second silently superseding the first as a
// new revision. A candidate list has no walk root, so it must key by path.
func TestCandidatesKeepDistinctIdentitiesAcrossDirectories(t *testing.T) {
	st, _ := newTestStore(t)
	root := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(root, sub, "readme.md")
		if err := os.WriteFile(p, []byte("content of "+sub), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{
		filepath.Join(root, "a", "readme.md"),
		filepath.Join(root, "b", "readme.md"),
	}
	n, err := st.IngestCandidates(context.Background(), paths, "cand")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("ingested=%d, want 2 — same-named candidates must not collapse", n)
	}
	live, err := st.ActiveSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("active sources=%d, want 2 (one superseded the other)", len(live))
	}
	keys := map[string]bool{}
	for _, s := range live {
		keys[s.BusinessKey] = true
	}
	if len(keys) != 2 {
		t.Fatalf("business keys collapsed: %v", keys)
	}
}

// A single-rooted candidate list must produce the SAME identities as ingesting
// that directory directly, so scan → pick → ingest is not a second-class path.
func TestCandidatesFromOneRootMatchDirectoryWalkKeys(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "top.md"), "top")
	write(filepath.Join(root, "sub", "deep.md"), "deep")
	paths := []string{filepath.Join(root, "top.md"), filepath.Join(root, "sub", "deep.md")}

	st1, _ := newTestStore(t)
	if _, err := st1.IngestCandidates(context.Background(), paths, "c"); err != nil {
		t.Fatal(err)
	}
	fromCandidates, err := st1.ActiveSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	st2, _ := newTestStore(t)
	if _, err := st2.IngestFiles(context.Background(), root, true, "w"); err != nil {
		t.Fatal(err)
	}
	fromWalk, err := st2.ActiveSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fromCandidates) != len(fromWalk) {
		t.Fatalf("candidate ingest produced %d sources, walk produced %d", len(fromCandidates), len(fromWalk))
	}
	walkKeys := map[string]bool{}
	for _, s := range fromWalk {
		walkKeys[s.BusinessKey] = true
	}
	for _, s := range fromCandidates {
		if !walkKeys[s.BusinessKey] {
			t.Fatalf("candidate key %q has no walk counterpart (walk keys %v)", s.BusinessKey, walkKeys)
		}
	}
}

func TestCommonRoot(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"one shared root", []string{"/x/a.md", "/x/sub/b.md"}, "/x"},
		{"siblings share nothing", []string{"a/readme.md", "b/readme.md"}, ""},
		{"single path", []string{"/x/y/a.md"}, "/x/y"},
		{"mixed abs and rel", []string{"/x/a.md", "b.md"}, ""},
		{"name prefix is not a boundary", []string{"/xab/a.md", "/x/c.md"}, "/"},
		{"empty", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commonRoot(tc.in); got != tc.want {
				t.Fatalf("commonRoot(%v)=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRelKeyNeverUsesBaseNameAlone(t *testing.T) {
	// Same base name, different directories: the key must differ.
	a := relKey("", "docs/a/readme.md")
	b := relKey("", "docs/b/readme.md")
	if a == b {
		t.Fatalf("candidate keys collapsed to %q", a)
	}
	// Inside the walk root the key stays relative (and outside it falls back
	// to the full path rather than the base name).
	if got := relKey("/scan", "/scan/sub/x.md"); got != "sub/x.md" {
		t.Fatalf("relKey inside root=%q, want sub/x.md", got)
	}
	if got := relKey("/scan", "/elsewhere/x.md"); got != "/elsewhere/x.md" {
		t.Fatalf("relKey outside root=%q, want the full path", got)
	}
}

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The resume cursor is a bare file index, so it must be bound to the input
// list it indexes. The hard case this guards: ingest dir1 to completion under
// the CLI's static default job key, then point the same key at a smaller dir2 —
// the old bare-integer cursor said "already past 3 files" against a 2-file
// list, the loop body never ran, and the job reported State:"done" with dir2
// never read.
func TestFileCursorDoesNotLeapAcrossDirectories(t *testing.T) {
	st, _ := newTestStore(t)
	dir1 := t.TempDir()
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(dir1, name), []byte(name+" 连接池 128。"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := st.IngestFiles(context.Background(), dir1, false, "files"); err != nil || c.Stored() != 3 {
		t.Fatalf("dir1: n=%d err=%v, want 3", c.Stored(), err)
	}
	dir2 := t.TempDir()
	for _, name := range []string{"x.md", "y.md"} {
		if err := os.WriteFile(filepath.Join(dir2, name), []byte(name+" 端口 8480。"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.IngestFiles(context.Background(), dir2, false, "files")
	if err != nil {
		t.Fatal(err)
	}
	if n.Stored() != 2 {
		t.Fatalf("dir2 under the same job key: ingested=%d, want 2 — the cursor leapt across directories", n.Stored())
	}
	// The job doc must tell the same story: dir2 fully done, nothing phantom.
	doc, err := st.GetJobDoc(context.Background(), "files")
	if err != nil {
		t.Fatal(err)
	}
	if doc.State != "done" || doc.Total != 2 || doc.Done != 2 {
		t.Fatalf("job doc after dir2: %+v", doc)
	}
}

// A cursor that matches its list still resumes mid-run; the legacy
// bare-integer format restarts from zero instead of guessing.
func TestFileCursorResumeAndLegacyRestart(t *testing.T) {
	st, _ := newTestStore(t)
	dir := t.TempDir()
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+" 正文内容。"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cursorKey := st.jobs + "files" + jobCursorSuffix
	if _, err := st.IngestFiles(context.Background(), dir, false, "files"); err != nil {
		t.Fatal(err)
	}
	// Re-seed the completed cursor back by one file, keeping the fingerprint
	// (the walk order is the store's business; the stored value carries it).
	raw, err := st.c.KVGet(context.Background(), cursorKey)
	if err != nil {
		t.Fatal(err)
	}
	fp, _, ok := strings.Cut(string(raw), ":")
	if !ok {
		t.Fatalf("cursor %q is not fingerprinted", raw)
	}
	if err := st.saveJobCursor(context.Background(), cursorKey, fp, 1); err != nil {
		t.Fatal(err)
	}
	n, err := st.IngestFiles(context.Background(), dir, false, "files")
	if err != nil {
		t.Fatal(err)
	}
	if n.Stored() != 2 {
		t.Fatalf("resumed run ingested=%d, want 2 (only the files after the cursor)", n.Stored())
	}
	// The legacy bare-integer cursor must restart, not skip.
	if err := st.c.KVPut(context.Background(), cursorKey, []byte("2"), 0); err != nil {
		t.Fatal(err)
	}
	n, err = st.IngestFiles(context.Background(), dir, false, "files")
	if err != nil {
		t.Fatal(err)
	}
	// All three are already in the store, so this run is 0 written / 3
	// unchanged: Stored() is the count, and a Written-only reading would be 0.
	if n.Stored() != 3 {
		t.Fatalf("legacy cursor run ingested=%d, want 3 (restart from zero)", n.Stored())
	}
	if n.Written != 0 || n.Unchanged != 3 {
		t.Fatalf("legacy cursor restart = %+v, want written 0 unchanged 3", n)
	}
}

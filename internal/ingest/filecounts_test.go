package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/willove/cumulus/internal/adapt"
)

// The file faces used to return ONE int that (a) mixed newly-written and
// unchanged documents and (b) said nothing about the files the run skipped —
// that ledger lived only in the JobDoc. So `ingest-files` over the 1,251-file
// corpus in the charset census printed `{"processed":1156}` and exit 0 while 95
// undecodable files were dropped, and a re-run under a fresh job key printed the
// same full count while storing nothing new. Both faces must now account for
// every file they examined.

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// put() has returned Result{Status:"unchanged"} on a digest match since the
// revision work, and ingestFileList discarded it with `_`. The split is the
// whole point of the count: "did this run store anything new" is the question
// an operator asks after pointing -dir at the wrong corpus.
func TestIngestFilesSplitsWrittenFromUnchanged(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "a.md 连接池最大 128。")
	writeFile(t, filepath.Join(dir, "b.md"), "b.md 端口是 8480。")

	first, err := st.IngestFiles(ctx, dir, false, "split1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 2 || first.Unchanged != 0 {
		t.Fatalf("first run = %+v, want written 2 unchanged 0", first)
	}

	// A NEW job key, so the resume cursor does not short-circuit the loop: every
	// file is read again and only the digest match keeps it out of Written.
	second, err := st.IngestFiles(ctx, dir, false, "split2")
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 || second.Unchanged != 2 {
		t.Fatalf("re-run = %+v, want written 0 unchanged 2 — put's unchanged status is being discarded", second)
	}
	if second.Stored() != 2 {
		t.Fatalf("Stored() = %d, want 2 (the number the faces used to report as one int)", second.Stored())
	}
}

// An edited file is a new revision, not an unchanged one — the split must not
// collapse "updated" into "unchanged" just because a live revision existed.
func TestIngestFilesCountsAnEditedFileAsWritten(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.md")
	writeFile(t, p, "连接池最大 128。")
	if _, err := st.IngestFiles(ctx, dir, false, "edit"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, "连接池最大 256，超时改成 60 秒。")
	again, err := st.IngestFiles(ctx, dir, false, "edit2")
	if err != nil {
		t.Fatal(err)
	}
	if again.Written != 1 || again.Unchanged != 0 {
		t.Fatalf("edited file = %+v, want written 1 unchanged 0", again)
	}
}

// The partition invariant: every file the run examined lands in exactly one
// bucket. This is what makes a silent drop impossible to hide — the same
// invariant the jsonl face got, at file granularity.
func TestIngestFilesPartitionsEveryFileExamined(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "连接池最大 128。")
	writeFile(t, filepath.Join(dir, "b.md"), "端口是 8480。")
	// Unextractable DOCX (a real zip container is required) and a file whose
	// text normalizes to nothing: both are skips with auditable reasons.
	writeFile(t, filepath.Join(dir, "c.docx"), "not a zip container")
	writeFile(t, filepath.Join(dir, "d.txt"), "\x00\r\n\t")

	c, err := st.IngestFiles(ctx, dir, false, "part")
	if err != nil {
		t.Fatal(err)
	}
	if c.Files != 4 || c.Processed != 4 {
		t.Fatalf("files/processed = %d/%d, want 4/4 (fresh run examines everything)", c.Files, c.Processed)
	}
	if got := c.Written + c.Unchanged + c.Skips(); got != c.Processed {
		t.Fatalf("partition = %d (written %d + unchanged %d + skipped %d), want %d: %+v",
			got, c.Written, c.Unchanged, c.Skips(), c.Processed, c)
	}
	if c.Written != 2 {
		t.Fatalf("written = %d, want 2: %+v", c.Written, c)
	}
	if c.Skipped["extract_failed"] != 1 || c.Skipped["empty"] != 1 {
		t.Fatalf("skip ledger = %+v, want one extract_failed and one empty", c.Skipped)
	}
}

// A resumed run examines nothing, so it must report nothing — an all-zero
// partition, not the file total dressed up as work done.
func TestIngestFilesResumeExaminesNothing(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.md"), "连接池最大 128。")
	if _, err := st.IngestFiles(ctx, dir, false, "res"); err != nil {
		t.Fatal(err)
	}
	again, err := st.IngestFiles(ctx, dir, false, "res")
	if err != nil {
		t.Fatal(err)
	}
	if again.Files != 1 || again.Processed != 0 {
		t.Fatalf("resume files/processed = %d/%d, want 1/0", again.Files, again.Processed)
	}
	if again.Written != 0 || again.Unchanged != 0 || again.Skips() != 0 {
		t.Fatalf("resume must account for nothing: %+v", again)
	}
}

// The adapt twin stores many documents per file, so its Written/Unchanged are
// RECORD counts while Skipped stays FILE counts. PutBatch already returned the
// real stored count; adaptOne added it up and threw the remainder away.
func TestIngestAdaptedSplitsWrittenFromUnchanged(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	writeFile(t, a, "{\"body\": \"连接池最大 128。\"}\n{\"body\": \"超时 30 秒。\"}\n")

	first, err := st.IngestAdapted(ctx, []string{a}, adapt.Fields{}, "asplit")
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 2 || first.Unchanged != 0 {
		t.Fatalf("first adapt run = %+v, want written 2 unchanged 0", first)
	}
	if first.Files != 1 || first.Processed != 1 {
		t.Fatalf("files/processed = %d/%d, want 1/1", first.Files, first.Processed)
	}

	second, err := st.IngestAdapted(ctx, []string{a}, adapt.Fields{}, "asplit2")
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 || second.Unchanged != 2 {
		t.Fatalf("re-run = %+v, want written 0 unchanged 2 — PutBatch's remainder is being dropped", second)
	}
	if got := second.Processed - second.Skips(); got != 1 {
		t.Fatalf("files that reached the store = %d, want 1: %+v", got, second)
	}
}

// The adapt face's file-level ledger must reach the CALLER, not only the
// JobDoc: the CLI face has no other way to show it.
func TestIngestAdaptedCarriesSkipLedgerToTheCaller(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	good := filepath.Join(dir, "a.jsonl")
	writeFile(t, good, "{\"body\": \"连接池最大 128。\"}\n")
	bad := filepath.Join(dir, "c.jsonl")
	writeFile(t, bad, "{\"body\": \"broken\"\n")
	missing := filepath.Join(dir, "gone.md")

	c, err := st.IngestAdapted(ctx, []string{good, bad, missing}, adapt.Fields{}, "aledger")
	if err != nil {
		t.Fatal(err)
	}
	if c.Files != 3 || c.Processed != 3 {
		t.Fatalf("files/processed = %d/%d, want 3/3", c.Files, c.Processed)
	}
	if c.Skipped["adapt_failed"] != 1 || c.Skipped["unreadable"] != 1 {
		t.Fatalf("skip ledger = %+v, want one adapt_failed and one unreadable", c.Skipped)
	}
	if got := c.Processed - c.Skips(); got != 1 {
		t.Fatalf("files that reached the store = %d, want 1: %+v", got, c)
	}
	if c.Written != 1 {
		t.Fatalf("written = %d, want 1: %+v", c.Written, c)
	}
}

// The skip reason the whole G4 justification rests on: the charset census found
// 7.6% of a real 1,251-file corpus undecodable, and before this the only place
// that number landed was the JobDoc. The bytes are charset_test's own fixture —
// the exact input that made x/text emit U+FFFD with err == nil.
func TestIngestFilesLedgersAnUndecodableFile(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "good.md"), "连接池最大 128。")
	garbage := []byte{0xFF, 0xFE, 0xFF, 0xFF, 0x00, 0x80, 0x81, 0x8D}
	if err := os.WriteFile(filepath.Join(dir, "bad.txt"), garbage, 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := st.IngestFiles(ctx, dir, false, "undec")
	if err != nil {
		t.Fatalf("one undecodable file must not fail the job: %v", err)
	}
	if c.Skipped["undecodable"] != 1 {
		t.Fatalf("skip ledger = %+v, want undecodable=1", c.Skipped)
	}
	if c.Written != 1 || c.Unchanged != 0 {
		t.Fatalf("the readable neighbour must still be stored: %+v", c)
	}
	if got := c.Written + c.Unchanged + c.Skips(); got != c.Processed {
		t.Fatalf("partition = %d, want %d: %+v", got, c.Processed, c)
	}
	// A partial skip is NOT an empty run: something was stored, so the faces
	// exit 0 by design and the ledger on stdout is the whole signal.
	if c.Empty() {
		t.Fatalf("Empty() = true over %+v, want false: a partial skip is not an empty run", c)
	}
	doc, err := st.GetJobDoc(ctx, "undec")
	if err != nil {
		t.Fatal(err)
	}
	if doc.SkipErrors[filepath.Join(dir, "bad.txt")] == "" {
		t.Fatalf("the per-file reason must survive into the job doc: %+v", doc)
	}
}

// A run that examined files and stored NOTHING is the case that must not look
// like success. Both faces feed the CLI's exit code from this predicate.
func TestFileCountsReportsAnEmptyRun(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "c.docx"), "not a zip container")
	writeFile(t, filepath.Join(dir, "d.txt"), "\x00\r\n\t")

	c, err := st.IngestFiles(ctx, dir, false, "nothing")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Empty() {
		t.Fatalf("Empty() = false over %+v, want true (2 files examined, 0 stored)", c)
	}
	// A resume examined nothing, so it is not an empty run — it is a no-op.
	again, err := st.IngestFiles(ctx, dir, false, "nothing")
	if err != nil {
		t.Fatal(err)
	}
	if again.Empty() {
		t.Fatalf("Empty() = true over %+v, want false: a resume examined nothing", again)
	}
}

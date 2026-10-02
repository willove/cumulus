package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/adapt"
	"github.com/willove/cumulus/internal/source"
)

// IngestAdapted's skip ledger must add up: Skipped is the SUM over reasons
// (it used to read skip["file"] — a key nothing writes on this path — so the
// job said Skipped=0 forever while SkipReasons held the real counts), and
// SkipErrors is keyed by the path AS LISTED (base names collide across
// directories, and the loser's reason silently vanished).
func TestIngestAdaptedSkipAccounting(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	good1 := filepath.Join(dir, "a.jsonl")
	good2 := filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(good1, []byte("{\"body\": \"连接池最大 128。\"}\n{\"body\": \"超时 30 秒。\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good2, []byte("{\"body\": \"端口是 8480。\"}\n{\"body\": \"备份每天 02:00。\"}\n{\"body\": \"灰度窗口 10%。\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A malformed JSONL: record decode fails, so adaptation fails and the
	// file is skipped with a reason, not fatal.
	badJSONL := filepath.Join(dir, "c.jsonl")
	if err := os.WriteFile(badJSONL, []byte("{\"body\": \"broken\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "gone.md") // never created

	n, err := st.IngestAdapted(ctx, []string{good1, badJSONL, missing, good2}, adapt.Fields{}, "jacc")
	if err != nil {
		t.Fatal(err)
	}
	if n.Stored() != 5 {
		t.Fatalf("records stored = %d, want 5 (%+v)", n.Stored(), n)
	}
	doc, err := st.GetJobDoc(ctx, "jacc")
	if err != nil {
		t.Fatal(err)
	}
	if doc.State != "done" {
		t.Fatalf("state = %q (%+v)", doc.State, doc)
	}
	if doc.Skipped != 2 {
		t.Fatalf("Skipped = %d, want 2 (sum over reasons)", doc.Skipped)
	}
	if doc.SkipReasons["adapt_failed"] != 1 || doc.SkipReasons["unreadable"] != 1 {
		t.Fatalf("SkipReasons = %+v", doc.SkipReasons)
	}
	if len(doc.SkipErrors) != 2 {
		t.Fatalf("SkipErrors = %+v, want one entry per skipped file", doc.SkipErrors)
	}
	for _, p := range []string{badJSONL, missing} {
		if _, ok := doc.SkipErrors[p]; !ok {
			t.Fatalf("SkipErrors must key by the listed path; %q missing from %+v", p, doc.SkipErrors)
		}
	}
	if doc.Records != 5 || doc.Done != 4 || doc.Total != 4 {
		t.Fatalf("counters = %+v (Records=docs, Done/Total=files)", doc)
	}
}

// Records is a DOCUMENT count for THIS run: the resumed FILE offset (the
// cursor start) must not be added into it — a job resumed at file index 1
// used to report records+1.
func TestIngestAdaptedRecordsIsThisRunOnly(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	good1 := filepath.Join(dir, "a.jsonl")
	good2 := filepath.Join(dir, "b.jsonl")
	if err := os.WriteFile(good1, []byte("{\"body\": \"连接池最大 128。\"}\n{\"body\": \"超时 30 秒。\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(good2, []byte("{\"body\": \"端口是 8480。\"}\n{\"body\": \"备份每天 02:00。\"}\n{\"body\": \"灰度窗口 10%。\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a resume that already covered file 0: the cursor parks at 1,
	// fingerprinted to this exact file list.
	cursorKey := st.jobs + "jr" + jobCursorSuffix
	if err := st.saveJobCursor(ctx, cursorKey, listFingerprint([]string{good1, good2}), 1); err != nil {
		t.Fatal(err)
	}
	n, err := st.IngestAdapted(ctx, []string{good1, good2}, adapt.Fields{}, "jr")
	if err != nil {
		t.Fatal(err)
	}
	if n.Stored() != 3 {
		t.Fatalf("records stored this run = %d, want 3 (%+v)", n.Stored(), n)
	}
	doc, err := st.GetJobDoc(ctx, "jr")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Records != 3 {
		t.Fatalf("Records = %d, want 3 (no file-offset inflation)", doc.Records)
	}
	if doc.Done != 2 {
		t.Fatalf("Done = %d, want 2 (file 0 counts through the cursor)", doc.Done)
	}
}

// failingRetirePort refuses the first retire patch (sources status→stale),
// simulating a transient engine refusal exactly where the old code swallowed
// the error and left two live revisions for one identity.
type failingRetirePort struct {
	cumulite.Port
	failed bool
}

func (p *failingRetirePort) PatchDocument(ctx context.Context, coll, id string, update map[string]any) (map[string]any, error) {
	if set, ok := update["$set"].(map[string]any); ok &&
		coll == "clus_sources" && set["status"] == source.StatusStale && !p.failed {
		p.failed = true
		return nil, errors.New("engine says no")
	}
	return p.Port.PatchDocument(ctx, coll, id, update)
}

// A failed retire must fail the batch, not pass silently: two live revisions
// for one identity never converge again (Reconcile skips actives).
func TestPutBatchFailsWhenRetireFails(t *testing.T) {
	st, engine := newTestStore(t)
	ctx := context.Background()
	bi, err := st.NewBatchIngester(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bi.PutBatch(ctx, []source.Source{
		source.New("A", "jsonl", "", "k", "zh", "第一版正文", nil),
	}); err != nil {
		t.Fatal(err)
	}
	st.c = &failingRetirePort{Port: engine}
	bi2, err := st.NewBatchIngester(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bi2.PutBatch(ctx, []source.Source{
		source.New("A", "jsonl", "", "k", "zh", "第二版正文，内容完全不同", nil),
	})
	if err == nil || !strings.Contains(err.Error(), "retiring previous revision") {
		t.Fatalf("want retire failure to fail the batch, got %v", err)
	}
}

// A storage refusal inside the adapt stream must fail the JOB — the file is
// not malformed, so counting it as adapt_failed (and reporting done over the
// lost data) is the exact bug.
func TestIngestAdaptedStoreErrorFailsJobNotSkip(t *testing.T) {
	st, engine := newTestStore(t)
	ctx := context.Background()
	bi, err := st.NewBatchIngester(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bi.PutBatch(ctx, []source.Source{
		source.New("A", "jsonl", "", "k", "zh", "第一版正文", nil),
	}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "a.jsonl")
	if err := os.WriteFile(f, []byte("{\"key\": \"k\", \"body\": \"第二版正文，完全不同\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.c = &failingRetirePort{Port: engine}
	_, err = st.IngestAdapted(ctx, []string{f}, adapt.Fields{}, "sf")
	if err == nil || !strings.Contains(err.Error(), "retiring previous revision") {
		t.Fatalf("want the store refusal to fail the job, got %v", err)
	}
	doc, err := st.GetJobDoc(ctx, "sf")
	if err != nil {
		t.Fatal(err)
	}
	if doc.SkipReasons["adapt_failed"] != 0 {
		t.Fatalf("store error must not be counted as adapt_failed: %+v", doc.SkipReasons)
	}
}

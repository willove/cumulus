package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/willove/cumulite"
	"github.com/willove/cumulus/internal/ingest"
)

// The last two fail-opens this suite carried as known-and-unfixed:
// widenSemantic collapsing three different degradations into one silent empty
// result, and EnsureEmbed treating a nil embedder as a no-op. Neither is fixed
// by making the caller fail — D7 says a missing cache slows a search down
// rather than breaking it — so in both cases the fix is that degrading stops
// being indistinguishable from working.

func swapSemanticArmOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	semanticArmMu.Lock()
	prevOut, prevSaid := semanticArmOut, semanticArmSaid
	semanticArmOut, semanticArmSaid = &buf, map[string]bool{}
	semanticArmMu.Unlock()
	t.Cleanup(func() {
		semanticArmMu.Lock()
		semanticArmOut, semanticArmSaid = prevOut, prevSaid
		semanticArmMu.Unlock()
	})
	return &buf
}

// Once per reason, not once per call: a missing body_embed index is a STATE, so
// printing it on every search would make the note unusable — and an unusable
// note gets redirected to /dev/null, which is the silence we started from.
func TestSemanticArmNoteFiresOncePerReason(t *testing.T) {
	buf := swapSemanticArmOut(t)
	noteSemanticArm("knn", errors.New("no such index"))
	noteSemanticArm("knn", errors.New("no such index"))
	noteSemanticArm("embed", errors.New("401 unauthorized"))

	got := buf.String()
	if n := strings.Count(got, "（knn）"); n != 1 {
		t.Fatalf("knn noted %d times, want exactly 1:\n%s", n, got)
	}
	// A different reason is a different fact about the process, so it gets its
	// own line: collapsing them would hide an endpoint failure behind a
	// previously-reported missing index.
	if n := strings.Count(got, "（embed）"); n != 1 {
		t.Fatalf("embed noted %d times, want 1:\n%s", n, got)
	}
}

func TestSemanticArmNoteNamesTheReasonAndCause(t *testing.T) {
	buf := swapSemanticArmOut(t)
	noteSemanticArm("embed", errors.New("401 unauthorized"))

	got := buf.String()
	for _, want := range []string{"embed", "401 unauthorized", "只走词面"} {
		if !strings.Contains(got, want) {
			t.Fatalf("note must carry %q, got %q", want, got)
		}
	}
}

// A degradation note with no remedy gets read once and then ignored, which is a
// slower version of the silence it replaced. Each reason must say what to do.
func TestSemanticArmNoteCarriesARemedyPerReason(t *testing.T) {
	for reason, want := range map[string]string{
		"seat":  "model status",
		"embed": "端点",
		"knn":   "ensure -embed",
	} {
		buf := swapSemanticArmOut(t)
		noteSemanticArm(reason, errors.New("x"))
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("reason %q must point at %q, got %q", reason, want, buf.String())
		}
	}
}

func newFailOpenStore(t *testing.T) (*ingest.Store, cumulite.Port) {
	t.Helper()
	engine, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { engine.Close() })
	st := ingest.New(engine, "clus_sources", "clus_evidence", "clus_clusters", "")
	if _, err := st.Ensure(context.Background(), "clus_weak_edges", "clus_cites", "clus_conflicts"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return st, engine
}

// CLUS_EMBED=minilm with the weights directory pointed somewhere empty is the
// one seat configuration embedderFor refuses, which makes the degradation
// reachable without a code seam. Before the fix this produced NO output at all
// unless CLUS_VERBOSE=1 — a flag nobody sets — so a search that had silently
// lost its semantic arm looked exactly like one that never had it.
func TestWidenSemanticReportsWhichArmWentMissing(t *testing.T) {
	buf := swapSemanticArmOut(t)
	t.Setenv("CLUS_EMBED", "minilm")
	t.Setenv("CLUS_MODEL_DIR", t.TempDir()) // weights absent → Resolve must fail
	_, engine := newFailOpenStore(t)

	out := widenSemantic(context.Background(), engine, "clus_sources", nil, nil, "连接池最大是多少", 4)
	if len(out) != 0 {
		t.Fatalf("a missing seat must degrade to no candidates, got %d", len(out))
	}
	if !strings.Contains(buf.String(), "（seat）") {
		t.Fatalf("the degradation must name its reason, got %q", buf.String())
	}
}

// The other half of D7: degrading must not fail the caller. widenSemantic no
// longer even has an error to return, so this pins the behaviour that replaced
// it — an empty candidate list, a search that goes on keyword-only, and a note
// naming the reason. The engine really does refuse here (measured:
// "cumulite: no vector index on field: clus_sources/body_embed"), so the knn
// branch is reachable without any injected collaborator.
func TestWidenSemanticDegradesToEmptyWithoutAnIndex(t *testing.T) {
	buf := swapSemanticArmOut(t)
	t.Setenv("CLUS_EMBED", "hash") // deterministic seat: no weights, no network
	_, engine := newFailOpenStore(t)
	// Ensure ran, EnsureEmbed did not, so there is no body_embed index to KNN.

	if out := widenSemantic(context.Background(), engine, "clus_sources", nil, nil, "连接池最大是多少", 4); len(out) != 0 {
		t.Fatalf("no vector index must yield no semantic candidates, got %d", len(out))
	}
	got := buf.String()
	if !strings.Contains(got, "（knn）") {
		t.Fatalf("the degradation must name its reason, got %q", got)
	}
	if !strings.Contains(got, "no vector index") {
		t.Fatalf("the note must carry the engine's own words, got %q", got)
	}
}

// A nil embedder used to mean "silently skip the entire vector backfill", which
// is indistinguishable from a corpus that needed none. No production caller can
// pass nil (embedderFor returns a working function or an error), so this is a
// caller-bug guard — and a guard that returns nil is not a guard.
func TestEnsureEmbedRefusesANilEmbedder(t *testing.T) {
	st, _ := newFailOpenStore(t)

	n, err := st.EnsureEmbed(context.Background(), nil, 4, "test-nil", 64)
	if err == nil {
		t.Fatalf("a nil embedder must be refused, got n=%d err=nil", n)
	}
	if !strings.Contains(err.Error(), "embedder") {
		t.Fatalf("the refusal must name what is missing: %v", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0 alongside the error", n)
	}
}

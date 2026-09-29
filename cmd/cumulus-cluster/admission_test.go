package main

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/willove/cumulus/internal/source"
)

func doc(id, key string) source.Source {
	return source.Source{ID: id, BusinessKey: key, Status: source.StatusActive}
}

func idsOf(ss []source.Source) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

// lexicalSweep builds the shape the admission really sees: a wide lexical
// sweep whose head is what the DEEP loop would otherwise score, plus a
// semantic neighbour sitting at the far tail.
func lexicalSweep(n int) []source.Source {
	out := make([]source.Source, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, doc(fmt.Sprintf("lex%03d", i), fmt.Sprintf("law-%03d", i)))
	}
	return out
}

// TestSemanticHeadDefaultAndRevert pins both ends of the switch.
//
// The default is 0 and the A/B evidence that once justified 4 is WITHDRAWN:
// it was gathered on a corpus whose documents (p50 145 characters) fit inside
// MiniLM's 128-token window, and on a real-length document the same arm scores
// R@4 1.1% against a 0.18% random baseline. Everything that would make 0
// silently turn the mechanism off must therefore be an accepted value, not
// just an absent one — a typo that parsed as 0 would disable the arm without
// anyone noticing, and a positive value must still opt back in.
func TestSemanticHeadDefaultAndRevert(t *testing.T) {
	t.Setenv("CLUS_ADMIT_SEMANTIC_HEAD", "")
	if got := semanticHead(); got != 0 {
		t.Fatalf("semanticHead() default = %d, want 0 (the A/B basis was a short-document artefact)", got)
	}
	t.Setenv("CLUS_ADMIT_SEMANTIC_HEAD", "4")
	if got := semanticHead(); got != 4 {
		t.Fatalf("semanticHead() = %d, want 4 (opt-in must still work)", got)
	}
	t.Setenv("CLUS_ADMIT_SEMANTIC_HEAD", "garbage")
	if got := semanticHead(); got != 0 {
		t.Fatalf("semanticHead() on an unparseable value = %d, want the default 0", got)
	}
	t.Setenv("CLUS_ADMIT_SEMANTIC_HEAD", "-3")
	if got := semanticHead(); got != 0 {
		t.Fatalf("semanticHead() on a negative value = %d, want the default 0", got)
	}
}

// The no-op contract: with the default on, a deployment that has no embedder
// or no body_embed index must still behave exactly as before, because
// widenSemantic returns nil in all three of those cases.
func TestPromoteSemanticHeadIsNoOpWithoutSemanticArm(t *testing.T) {
	cands := lexicalSweep(6)
	if got := promoteSemanticHead(cands, nil, semanticHead()); !reflect.DeepEqual(idsOf(got), idsOf(cands)) {
		t.Fatalf("a nil semantic arm must be an identity under the default, got %v", idsOf(got))
	}
}

func TestPromoteSemanticHeadZeroIsIdentity(t *testing.T) {
	cands := lexicalSweep(6)
	sem := []source.Source{doc("sem1", "law-x"), doc("sem2", "law-y")}

	got := promoteSemanticHead(cands, sem, 0)
	if !reflect.DeepEqual(idsOf(got), idsOf(cands)) {
		t.Fatalf("k=0 must be an identity, got %v want %v", idsOf(got), idsOf(cands))
	}
	if got := promoteSemanticHead(cands, nil, 4); !reflect.DeepEqual(idsOf(got), idsOf(cands)) {
		t.Fatalf("empty semantic arm must be an identity, got %v", idsOf(got))
	}
}

func TestPromoteSemanticHeadPreservesSet(t *testing.T) {
	cands := lexicalSweep(6)
	sem := []source.Source{doc("sem1", "law-x"), doc("sem2", "law-y"), doc("sem3", "law-z")}

	got := promoteSemanticHead(cands, sem, 2)
	if want := []string{"sem1", "sem2", "lex000", "lex001", "lex002", "lex003", "lex004", "lex005"}; !reflect.DeepEqual(idsOf(got), want) {
		t.Fatalf("k=2 order = %v, want %v", idsOf(got), want)
	}
}

// A neighbour that is ALSO a lexical hit must not be duplicated — it moves to
// the head rather than appearing twice.
func TestPromoteSemanticHeadDedupesAgainstLexical(t *testing.T) {
	cands := []source.Source{doc("lex000", "law-000"), doc("lex001", "law-001"), doc("lex002", "law-002")}
	sem := []source.Source{doc("lex002", "law-002"), doc("sem1", "law-x")}

	got := promoteSemanticHead(cands, sem, 2)
	seen := map[string]int{}
	for _, id := range idsOf(got) {
		seen[id]++
	}
	if seen["lex002"] != 1 {
		t.Fatalf("lex002 appears %d times, want exactly 1 (%v)", seen["lex002"], idsOf(got))
	}
	if want := []string{"lex002", "sem1", "lex000", "lex001"}; !reflect.DeepEqual(idsOf(got), want) {
		t.Fatalf("order = %v, want %v", idsOf(got), want)
	}
}

func TestPromoteSemanticHeadKOverLength(t *testing.T) {
	cands := lexicalSweep(3)
	sem := []source.Source{doc("sem1", "law-x")}
	got := promoteSemanticHead(cands, sem, 99)
	if want := []string{"sem1", "lex000", "lex001", "lex002"}; !reflect.DeepEqual(idsOf(got), want) {
		t.Fatalf("k>len(semantic) order = %v, want %v", idsOf(got), want)
	}
}

// TestAdmissionTailAppendLosesSemanticNeighbour is the regression gate for the
// defect this change fixes: rankFunc appends the KNN arm after a 500-document
// lexical sweep, and usageFirst/mixedAffinity then cut to maxDeepLoops. The
// neighbour is discarded before the DEEP loop can reach it. Both halves of the
// admission chain are exercised here because neither usageFirst nor
// mixedAffinity had any test at all.
func TestAdmissionTailAppendLosesSemanticNeighbour(t *testing.T) {
	const maxLoops = 4
	cands := lexicalSweep(500)
	sem := []source.Source{doc("sem1", "law-x")}

	// Today's shape: semantic arm merged at the tail.
	withTail := append(append([]source.Source{}, cands...), sem...)
	tailCut := mixedAffinity(usageFirst(withTail, nil, maxLoops, usageCap()), nil, maxLoops, 3)
	if contains(tailCut, "sem1") {
		t.Fatalf("expected the tail-append to be truncated away, but sem1 survived: %v", idsOf(tailCut))
	}

	// Fixed shape: the neighbour is promoted to the head first.
	headCut := mixedAffinity(usageFirst(promoteSemanticHead(cands, sem, 1), nil, maxLoops, usageCap()), nil, maxLoops, 3)
	if !contains(headCut, "sem1") {
		t.Fatalf("promoted semantic neighbour must reach the admission cut, got %v", idsOf(headCut))
	}
	if len(headCut) != maxLoops {
		t.Fatalf("admission cut = %d docs, want %d", len(headCut), maxLoops)
	}
}

// Promotion must not starve the usage/affinity machinery it feeds.
func TestPromoteSemanticHeadStillAllowsUsagePromotion(t *testing.T) {
	const maxLoops = 4
	cands := lexicalSweep(500)
	sem := []source.Source{doc("sem1", "law-x"), doc("sem2", "law-y")}

	promoted := promoteSemanticHead(cands, sem, 2)
	uw := map[string]float64{"lex010": 0.5}
	cut := usageFirst(promoted, uw, maxLoops, usageCap())

	if !contains(cut, "sem1") {
		t.Fatalf("sem1 should survive the usage promotion: %v", idsOf(cut))
	}
	if !contains(cut, "lex010") {
		t.Fatalf("a weighted document must still be promotable alongside: %v", idsOf(cut))
	}
}

func contains(ss []source.Source, id string) bool {
	for _, s := range ss {
		if s.ID == id {
			return true
		}
	}
	return false
}

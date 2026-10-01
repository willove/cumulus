package eval

import (
	"testing"

	"github.com/willove/cumulus/internal/source"
)

// B-3, resolved as a latent risk rather than a live bug — and the reason it is
// still worth a test is the reason it is NOT worth a format change today.
//
// RevisionID composes identities as "src:" + identity + "#" + version. If a
// business key itself contains '#' or ':' (a document key like "spec#v2" is
// not far-fetched), that id is ambiguous to a naive parser: splitting on the
// FIRST '#' would read key="spec", version="v2#3". So the format invites a
// class of bug rather than being one.
//
// It is not a live bug because:
//   - the only parser in the tree, canonicalKeys, uses LastIndex — correct;
//   - 0 of 14,313 real business keys in var/chinalaw/corpus.jsonl contain
//     '#' or ':'.
//
// Changing the id format would be a DATA MIGRATION — every stored document
// would need re-ingesting — which is a far bigger risk than the ambiguity it
// removes. So instead of changing the format, this test pins the property
// that actually protects the system today: an id built from a hostile key
// still canonicalises back to that key, and still cannot be confused with a
// different key that happens to share a prefix.

func TestRevisionIDWithDelimitersInTheKeyRoundTrips(t *testing.T) {
	const key = "spec#v2"
	id := source.RevisionID(key, "标题", "0123456789abcdef", 3)
	if want := "src:spec#v2#3"; id != want {
		t.Fatalf("RevisionID = %q, want %q", id, want)
	}

	got := canonicalKeys(id)
	var hit bool
	for _, k := range got {
		if k == key {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("canonicalKeys(%q) = %v — a key containing '#' must still resolve, "+
			"or eval would score the correct document as a miss", id, got)
	}
}

// The ambiguity is real: a FIRST-'#' parse of the same id yields a different
// key. That is precisely why canonicalKeys must keep using LastIndex — this
// test fails loudly if someone "simplifies" it to Index.
func TestRevisionIDDelimiterAmbiguityIsPinned(t *testing.T) {
	id := source.RevisionID("spec#v2", "", "0123456789abcdef", 3)

	// What a naive first-delimiter parse would yield (i.e. what must NOT happen).
	const naiveKey = "spec"
	for _, k := range canonicalKeys(id) {
		if k == naiveKey {
			t.Fatalf("canonicalKeys(%q) = %v — resolved to %q, which is the naive "+
				"first-'#' parse. The correct answer is the last-'#' parse; "+
				"switching to strings.Index here would corrupt eval accounting silently.",
				id, []string{k}, naiveKey)
		}
	}
}

// Two different keys must not collapse to the same canonical form, including
// the case where one key is a prefix of the other's id.
func TestDistinctKeysStayDistinct(t *testing.T) {
	a := source.RevisionID("spec", "", "0123456789abcdef", 1)    // src:spec#1
	b := source.RevisionID("spec#v2", "", "0123456789abcdef", 3) // src:spec#v2#3
	if a == b {
		t.Fatalf("distinct keys produced the same id %q", a)
	}
	set := map[string]string{}
	for _, id := range []string{a, b} {
		for _, k := range canonicalKeys(id) {
			if prev, ok := set[k]; ok {
				t.Fatalf("canonical form %q is shared by %q and %q", k, prev, id)
			}
			set[k] = id
		}
	}
}

package eval

import (
	"context"
	"reflect"
	"testing"

	"github.com/willove/cumulite"
)

// D-3, re-derived. The review note said docToReport drops `frozen`/`notes` —
// on inspection both halves are wrong: the decoder is docToRun, it does read
// `frozen` into a typed field, and RunDoc has no `notes` at all. The real gap
// was the absence of anything pinning that: every field a Save writes must
// survive the trip back out, and no test asserted it.
//
// A dropped field here is not a crash — it is a row that looks complete and is
// not. `frozen` is the A.6 binding that tells a reader whether two runs are
// comparable, so losing it makes runs silently incomparable, which is worse
// than visibly missing.

func roundTripStore(t *testing.T) (*CumuStore, context.Context) {
	t.Helper()
	ctx := context.Background()
	eng, err := cumulite.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if err := eng.EnsureCollection(ctx, "clus_evals"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	return NewCumuStore(eng, "clus_evals"), ctx
}

func fullRunDoc() RunDoc {
	return RunDoc{
		ID: "run:1700000000000", Tag: "l1-ab", At: "2026-10-01T07:44:28Z",
		N: 39, Judged: true,
		System: Report{
			N: 39, EM: 0.433, EvRec: 0.30, Ground: 0.90,
			Taxonomy: Taxonomy{Correct: 13, AnsweredWrong: 14, NotRetrieved: 12},
		},
		ClosedBook:   Report{N: 39, EM: 0.70, EvRec: 0.0},
		McNemar:      McNemar{BOnly: 4, COnly: 12, P: 0.077, N: 39},
		Modes:        map[string]int{"DEEP": 30, "FAST": 9},
		SearchTokens: 164672, JudgeTokens: 5349, RejectedProposals: 2,
		Frozen:     &Frozen{ItemsSHA: "i-abc", CorpusSHA: "c-def", ConfigSHA: "g-hij", OrderSeed: 7},
		ConfigText: `{"fast":{"a":1}}`,
		// Extra is map[string]any, so a value that goes through the engine's
		// JSON round-trip comes back as float64 even if it went in as an int.
		// That is a property of the type, not a defect — but it is exactly the
		// kind of thing a round-trip test should SAY out loud rather than
		// quietly encode. See TestRunStoreExtraNumbersComeBackAsFloat64.
		Extra: map[string]any{"nr_breakdown": map[string]any{"label": "not_retrieved"}},
	}
}

func deref(f *Frozen) any {
	if f == nil {
		return nil
	}
	return *f
}

func TestRunStorePreservesEveryField(t *testing.T) {
	st, ctx := roundTripStore(t)
	want := fullRunDoc()
	if err := st.SaveRun(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetRun(ctx, want.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("GetRun returned nil for a row that was just written")
	}
	if !reflect.DeepEqual(*got, want) {
		// %+v prints a *Frozen as an ADDRESS, which hides the very difference we
		// are hunting for — dereference it before printing.
		t.Fatalf("round-trip lost or altered data\n"+
			"  want frozen=%+v got frozen=%+v\n"+
			"  want extra=%+v got extra=%+v\n"+
			"  want system=%+v got system=%+v",
			deref(want.Frozen), deref(got.Frozen), want.Extra, got.Extra, want.System, got.System)
	}
}

// The A.6 binding is the field that matters most: it is how a reader tells
// whether two scoreboard rows may be compared at all. Assert it separately so
// a failure names the actual problem rather than burying it in a struct diff.
func TestRunStoreKeepsTheFrozenBinding(t *testing.T) {
	st, ctx := roundTripStore(t)
	want := fullRunDoc()
	if err := st.SaveRun(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetRun(ctx, want.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v (doc=%v)", err, got)
	}
	if got.Frozen == nil {
		t.Fatal("Frozen is nil after a round-trip — the A.6 comparability binding was dropped")
	}
	if !reflect.DeepEqual(*got.Frozen, *want.Frozen) {
		t.Fatalf("frozen binding altered: want %+v, got %+v", *want.Frozen, *got.Frozen)
	}
	if got.ConfigText != want.ConfigText {
		t.Fatalf("config text lost: want %q, got %q", want.ConfigText, got.ConfigText)
	}
	if _, ok := got.Extra["nr_breakdown"]; !ok {
		t.Fatalf("Extra (cmd-level breakdowns) was dropped: %+v", got.Extra)
	}
}

// A row written before these readers existed must still round-trip, and one
// with no frozen binding must come back as nil rather than a zero struct that
// looks like a real binding.
func TestRunStoreAbsentOptionalsStayAbsent(t *testing.T) {
	st, ctx := roundTripStore(t)
	plain := RunDoc{ID: "run:1", Tag: "t", N: 1, System: Report{N: 1}}
	if err := st.SaveRun(ctx, plain); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetRun(ctx, "run:1")
	if err != nil || got == nil {
		t.Fatalf("get: %v (doc=%v)", err, got)
	}
	if got.Frozen != nil {
		t.Fatalf("a row with no frozen binding came back as %+v — a zero binding is "+
			"indistinguishable from a real one and would let incomparable runs "+
			"compare as if they were comparable", got.Frozen)
	}
}

// Extra is typed map[string]any, so the engine's JSON round-trip normalises
// every number to float64. reflect.DeepEqual on the whole RunDoc therefore
// fails for a perfectly healthy row whose Extra held an int — the strict
// round-trip test above would be asserting an impossibility, not a property.
// Pinned here so the behaviour is documented rather than discovered.
func TestRunStoreExtraNumbersComeBackAsFloat64(t *testing.T) {
	st, ctx := roundTripStore(t)
	in := RunDoc{
		ID: "run:x", Tag: "t", N: 1, System: Report{N: 1},
		Extra: map[string]any{"n": 12},
	}
	if err := st.SaveRun(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetRun(ctx, "run:x")
	if err != nil || got == nil {
		t.Fatalf("get: %v (doc=%v)", err, got)
	}
	v, ok := got.Extra["n"]
	if !ok {
		t.Fatalf("Extra lost its key: %+v", got.Extra)
	}
	if f, ok := v.(float64); !ok || f != 12 {
		t.Fatalf("Extra[\"n\"] = %#v, want float64(12) — a caller comparing with "+
			"reflect.DeepEqual against the value it wrote will be surprised, which "+
			"is why this is pinned", v)
	}
}

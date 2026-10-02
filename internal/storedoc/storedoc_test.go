package storedoc

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/willove/cumulite"
)

// rec is the shape under test. LaterAt stands in for "a field someone added
// later": the package's whole reason to exist is that such a field reaches
// storage without anybody remembering to register it in a hand-kept table.
type rec struct {
	ID      string   `json:"_id"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Score   float64  `json:"score"`
	Tags    []string `json:"tags,omitempty"`
	LaterAt string   `json:"later_at"`
}

func sample() rec {
	return rec{ID: "r1", Title: "手册", Body: "连接池最大 128。", Score: 4.5, Tags: []string{"ops"}, LaterAt: "2026-10-03"}
}

// Doc must carry every json tag, and derive them rather than curate them:
// the resulting key set is exactly what encoding/json produces for the value.
func TestDocCarriesEveryTaggedField(t *testing.T) {
	got, err := Doc(sample())
	if err != nil {
		t.Fatal(err)
	}
	// Derived, not hand-maintained: identical to the marshal/unmarshal round trip.
	raw, err := json.Marshal(sample())
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Doc must equal the json-tag derivation:\n got %#v\nwant %#v", got, want)
	}
	for _, k := range []string{"_id", "title", "body", "score", "tags", "later_at"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("tagged field %q missing from the document — that is the drift this package exists to prevent", k)
		}
	}
}

// rawPort hides everything beyond the Port interface: embedding the INTERFACE
// (not the concrete engine) means the value satisfies cumulite.Port but NOT
// StructPort or ShapePort, which is what drives the fallback branch.
type rawPort struct{ cumulite.Port }

func openEngine(t *testing.T) cumulite.Port {
	t.Helper()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	ctx := context.Background()
	for _, c := range []string{"typed", "fallback"} {
		if err := engine.EnsureCollection(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	return engine
}

// The central promise: whichever path a store takes, the same document lands.
// If these two diverged, switching ports would silently change stored shape.
func TestTypedAndFallbackPathsProduceTheSameDocument(t *testing.T) {
	ctx := context.Background()
	engine := openEngine(t)

	if _, ok := engine.(cumulite.StructPort); !ok {
		t.Fatal("precondition: the in-memory engine must implement StructPort, else this tests one path twice")
	}
	var raw cumulite.Port = rawPort{engine}
	if _, ok := raw.(cumulite.StructPort); ok {
		t.Fatal("precondition: rawPort must NOT satisfy StructPort or the fallback branch is never exercised")
	}

	r := sample()
	if err := WriteStruct(ctx, engine, "typed", r.ID, r, false); err != nil {
		t.Fatalf("typed insert: %v", err)
	}
	r2 := sample()
	if err := WriteStruct(ctx, rawPort{engine}, "fallback", r2.ID, r2, false); err != nil {
		t.Fatalf("fallback insert: %v", err)
	}

	typedDoc, err := engine.GetDocument(ctx, "typed", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	fallbackDoc, err := engine.GetDocument(ctx, "fallback", r2.ID)
	if err != nil {
		t.Fatal(err)
	}
	// _id is engine-assigned bookkeeping; compare the payload the caller owns.
	for _, d := range []map[string]any{typedDoc, fallbackDoc} {
		delete(d, "_id")
	}
	if !reflect.DeepEqual(typedDoc, fallbackDoc) {
		t.Fatalf("the two write paths stored different documents:\n typed    %#v\n fallback %#v", typedDoc, fallbackDoc)
	}
	// And the payload must carry the late-added field, on both paths.
	for name, d := range map[string]map[string]any{"typed": typedDoc, "fallback": fallbackDoc} {
		if d["later_at"] != "2026-10-03" {
			t.Fatalf("%s path dropped later_at: %#v", name, d)
		}
	}
}

// exists routes to replace, not insert — on both paths, or a store's update
// would duplicate instead of overwrite depending on engine capability.
func TestWriteStructReplacesInPlaceWhenExists(t *testing.T) {
	ctx := context.Background()
	engine := openEngine(t)
	for _, tc := range []struct {
		name string
		port cumulite.Port
	}{
		{"typed", engine},
		{"fallback", rawPort{engine}},
	} {
		coll := tc.name
		r := sample()
		if err := WriteStruct(ctx, tc.port, coll, r.ID, r, false); err != nil {
			t.Fatalf("%s insert: %v", tc.name, err)
		}
		r.Body = "改成 256。"
		r.Score = 7
		if err := WriteStruct(ctx, tc.port, coll, r.ID, r, true); err != nil {
			t.Fatalf("%s replace: %v", tc.name, err)
		}
		doc, err := engine.GetDocument(ctx, coll, r.ID)
		if err != nil {
			t.Fatalf("%s get: %v", tc.name, err)
		}
		if doc["body"] != "改成 256。" {
			t.Fatalf("%s: replace did not land: %#v", tc.name, doc)
		}
	}
}

// DeclareShape is documented as best effort: a port without ShapePort must be
// silently inert rather than panic — probes run against both kinds.
func TestDeclareShapeIsInertWithoutShapePort(t *testing.T) {
	engine := openEngine(t)
	ctx := context.Background()
	var rawShape cumulite.Port = rawPort{engine}
	if _, ok := rawShape.(cumulite.ShapePort); ok {
		t.Fatal("precondition: rawPort must not satisfy ShapePort")
	}
	DeclareShape(ctx, rawPort{engine}, "fallback", rec{}) // must not panic
	DeclareShape(ctx, engine, "typed", rec{})             // the capable path, also must not panic
}

func TestDocReportsUnmarshalableValue(t *testing.T) {
	if _, err := Doc(make(chan int)); err == nil {
		t.Fatal("an unmarshalable value must error, not yield a partial document")
	} else if !strings.Contains(err.Error(), "storedoc") {
		t.Fatalf("error must name the package: %v", err)
	}
}

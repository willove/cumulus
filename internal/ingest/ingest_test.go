package ingest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/willove/cumulite"

	"github.com/willove/cumulus/internal/source"
)

func TestToFromDocRoundtrip(t *testing.T) {
	src := source.New("t", "md", "u", "k", "zh", "正文内容", map[string]any{"a": 1})
	doc, derr := sourceDoc(src)
	if derr != nil {
		t.Fatal(derr)
	}
	if doc["_id"] != src.ID {
		t.Fatalf("id mismatch %v", doc["_id"])
	}
	back, err := fromDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	if back.Digest != src.Digest || back.Body != src.Body {
		t.Fatalf("roundtrip mismatch %+v", back)
	}
	if len(back.Structure) != len(src.Structure) {
		t.Fatalf("structure lost: %+v", back.Structure)
	}
}

// The typed write path (cumulite StructPort / storedoc.Doc) is the end of
// hand-maintained field tables: source documents and evidence windows now
// serialize from their structs' json tags, so a new field cannot be dropped
// by a translator that forgot it. This pins it with the engine's own ruler:
// the declared shape's audit of what actually LANDED for both collections
// (Missing/Unknown would name a drifted key). Value equality is not
// assertable from outside put — it stamps its own timestamps — and the
// parse-level round-trip is TestToFromDocRoundtrip's job.
func TestStructDerivedDocsAreShapeCleanInStorage(t *testing.T) {
	ctx := context.Background()
	engine, err := cumulite.Open("", cumulite.WithInMemory())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	for _, coll := range []string{"clus_sources", "clus_evidence", "clus_clusters"} {
		if err := engine.EnsureCollection(ctx, coll); err != nil {
			t.Fatal(err)
		}
	}
	st := New(engine, "clus_sources", "clus_evidence", "clus_clusters", "alpha")
	src := source.New("t", "md", "u", "k", "zh", "正文内容", map[string]any{"a": 1})
	put, err := st.put(ctx, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkEvidence(ctx, put.ID, 0, 4, 7, "r", "snip"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ coll, id string }{
		{"clus_sources", put.ID},
		{"clus_evidence", fmt.Sprintf("ev:%s:0:4", strings.TrimPrefix(put.ID, "src:"))},
	} {
		raw, err := engine.GetDocument(ctx, tc.coll, tc.id)
		if err != nil {
			t.Fatalf("%s/%s: %v", tc.coll, tc.id, err)
		}
		audit, err := engine.ShapeReport(tc.coll, raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(audit.Missing) > 0 || len(audit.Unknown) > 0 {
			t.Fatalf("%s/%s shape audit: missing=%v unknown=%v", tc.coll, tc.id, audit.Missing, audit.Unknown)
		}
	}
}

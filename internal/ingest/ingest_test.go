package ingest

import (
	"testing"

	"github.com/willove/cumulus/internal/source"
)

func TestToFromDocRoundtrip(t *testing.T) {
	src := source.New("t", "md", "u", "k", "zh", "正文内容", map[string]any{"a": 1})
	doc := toDoc(src)
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

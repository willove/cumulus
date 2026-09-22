package ingest

import (
	"strings"
	"testing"
)

// Path B: body comes from a declarative template — never raw JSON serialization.
func TestRenderMapTemplate(t *testing.T) {
	spec := MapSpec{
		Title: "{{name}}",
		Body:  "{{name}}\n{{desc}}",
		Key:   "{{name}}",
		Meta:  []string{"vendor"},
	}
	rec := map[string]any{"name": "路由器条目", "desc": "型号 AX3000", "vendor": "TP"}
	src, err := RenderMap(spec, rec)
	if err != nil {
		t.Fatal(err)
	}
	if src.Body != "路由器条目\n型号 AX3000" {
		t.Fatalf("body not rendered: %q", src.Body)
	}
	if src.Title != "路由器条目" || src.BusinessKey != "路由器条目" {
		t.Fatalf("title/key mismatch: %+v", src)
	}
	if src.Meta["vendor"] != "TP" {
		t.Fatalf("meta passthrough lost: %+v", src.Meta)
	}
	if strings.Contains(src.Body, "{") {
		t.Fatalf("body must not carry raw JSON: %q", src.Body)
	}
}

func TestRenderMapMissingField(t *testing.T) {
	if _, err := RenderMap(MapSpec{Body: "{{missing}}"}, map[string]any{}); err == nil {
		t.Fatal("missing template field must error, not silently blank")
	}
	if _, err := RenderMap(MapSpec{Body: ""}, map[string]any{}); err == nil {
		t.Fatal("body template is required")
	}
}

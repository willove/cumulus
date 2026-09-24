package adapt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTmp(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeJSONLines(t *testing.T) {
	p := writeTmp(t, "a.json", `{"title":"甲","content":"正文","id":"1","dataTime":"2024-12-24"}
{"title":"乙","content":"正文二","id":"2","dataTime":"2024-12-25"}`)
	got, err := ProbeFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindJSONLines {
		t.Fatalf("kind=%v", got.Kind)
	}
	if got.Records < 2 {
		t.Fatalf("records=%d", got.Records)
	}
	for _, want := range []string{"title", "content", "id", "dataTime"} {
		if !containsStr(got.Fields, want) {
			t.Fatalf("field %q missing from %v", want, got.Fields)
		}
	}
	if len(got.Bodyish) != 1 || got.Bodyish[0] != "content" {
		t.Fatalf("bodyish=%v", got.Bodyish)
	}
	if len(got.Titleish) != 1 || got.Titleish[0] != "title" {
		t.Fatalf("titleish=%v", got.Titleish)
	}
	if len(got.IDish) != 1 || got.IDish[0] != "id" {
		t.Fatalf("idish=%v", got.IDish)
	}
	if got.Bytes == 0 {
		t.Fatal("bytes must be reported")
	}
}

func TestProbeCSV(t *testing.T) {
	p := writeTmp(t, "a.csv", "text_id,text\n1,红棉优级小粒老黄冰糖\n2,异形魔方\n")
	got, err := ProbeFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindCSV {
		t.Fatalf("kind=%v", got.Kind)
	}
	if !containsStr(got.Fields, "text_id") || !containsStr(got.Fields, "text") {
		t.Fatalf("fields=%v", got.Fields)
	}
	if !containsStr(got.IDish, "text_id") {
		t.Fatalf("text_id should be recognised as an identity: %v", got.IDish)
	}
}

// A record with no recognised body field must say so, so the operator knows the
// widest column will be used instead of silently getting something else.
func TestProbeUnknownBodySaysSo(t *testing.T) {
	p := writeTmp(t, "a.jsonl", `{"alpha":"x","beta":"y"}`)
	got, err := ProbeFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Bodyish) != 0 {
		t.Fatalf("nothing should look like a body: %v", got.Bodyish)
	}
	if !strings.Contains(got.Note, "widest") {
		t.Fatalf("the probe must warn that the widest column will be used: %q", got.Note)
	}
}

func TestProbeArray(t *testing.T) {
	p := writeTmp(t, "a.json", `[{"author":"杜甫","paragraphs":["国破山河在","城春草木深"]}]`)
	got, err := ProbeFile(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindJSONArray {
		t.Fatalf("kind=%v", got.Kind)
	}
	if !containsStr(got.Bodyish, "paragraphs") {
		t.Fatalf("paragraphs should be a body candidate: %v", got.Bodyish)
	}
	if !containsStr(got.Titleish, "author") {
		t.Fatalf("author should be a title candidate: %v", got.Titleish)
	}
}

func TestProbeMissingFile(t *testing.T) {
	if _, err := ProbeFile(context.Background(), filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing file must error")
	}
}

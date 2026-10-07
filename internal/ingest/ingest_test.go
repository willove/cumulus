package ingest

import (
	"os"
	"path/filepath"
	"strings"

	gocontext "context"
	"testing"

	"github.com/willove/cumulus/internal/corpus"
)

func writeFileRaw(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

var _ = filepath.Join

type memPort struct {
	docs map[string]map[string]any
}

func newMemPort() *memPort { return &memPort{docs: map[string]map[string]any{}} }

func (m *memPort) EnsureCollection(gocontext.Context, string) error { return nil }
func (m *memPort) PutStruct(_ gocontext.Context, coll, id string, v any) error {
	if m.docs[coll] == nil {
		m.docs[coll] = map[string]any{}
	}
	m.docs[coll][id] = v
	return nil
}
func (m *memPort) GetStruct(gocontext.Context, string, string, any) error { return nil }
func (m *memPort) ListIDs(_ gocontext.Context, coll string, _ int) ([]string, error) {
	ids := make([]string, 0, len(m.docs[coll]))
	for id := range m.docs[coll] {
		ids = append(ids, id)
	}
	return ids, nil
}
func (m *memPort) Delete(gocontext.Context, string, string) error     { return nil }
func (m *memPort) PutValue(gocontext.Context, string, []byte) error   { return nil }
func (m *memPort) GetValue(gocontext.Context, string) ([]byte, error) { return nil, nil }
func (m *memPort) Health(gocontext.Context) error                     { return nil }

func TestContentIDStable(t *testing.T) {
	a := corpus.DigestHex([]byte("同一内容"))
	b := corpus.DigestHex([]byte("同一内容"))
	c := corpus.DigestHex([]byte("别的内容"))
	if a != b {
		t.Fatal("same content must give same id")
	}
	if a == c {
		t.Fatal("different content must give different id")
	}
}

func TestTextIdempotent(t *testing.T) {
	ctx := gocontext.Background()
	p := newMemPort()
	id1, err := Text(ctx, p, "连接池默认为 100。", "paste")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := Text(ctx, p, "连接池默认为 100。", "paste")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatal("same text must dedupe to one id")
	}
	if len(p.docs["documents"]) != 1 {
		t.Fatalf("store must hold exactly one doc, got %d", len(p.docs["documents"]))
	}
}

func TestTextRejectsEmpty(t *testing.T) {
	if _, err := Text(gocontext.Background(), newMemPort(), "   ", "x"); err == nil {
		t.Fatal("empty body must be rejected")
	}
}

func TestStripHTML(t *testing.T) {
	html := `<html><head><style>body{color:red}</style></head>
<body><h1>标题</h1><p>第一段&nbsp;内容。</p><script>var x=1</script><ul><li>甲</li><li>乙</li></ul></body></html>`
	out := StripHTML(html)
	if strings.Contains(out, "color:red") || strings.Contains(out, "var x") {
		t.Fatalf("script/style must be stripped, got %q", out)
	}
	if !strings.Contains(out, "标题") || !strings.Contains(out, "第一段 内容。") || !strings.Contains(out, "甲") {
		t.Fatalf("text must survive: %q", out)
	}
	if !strings.Contains(out, "\n") {
		t.Fatal("block boundaries must survive as newlines")
	}
}

func TestWatchDirImportsFiles(t *testing.T) {
	ctx := gocontext.Background()
	p := newMemPort()
	dir := t.TempDir()
	writeFile(t, dir, "a.md", "# 甲\n连接池默认为 100。")
	writeFile(t, dir, "b.txt", "部署端口默认 8484。")
	writeFile(t, dir, "skip.bin", "\x00\x01")
	writeFile(t, dir, ".hidden.md", "隐藏文件不该收")

	seen := map[string]string{}
	n := scanOnce(ctx, p, dir, seen)
	if n != 2 {
		t.Fatalf("want 2 imported (md+txt), got %d", n)
	}
	// 第二轮：没变化 → 0
	if again := scanOnce(ctx, p, dir, seen); again != 0 {
		t.Fatalf("unchanged files must not re-import, got %d", again)
	}
	// 文件变了 → 再收
	writeFile(t, dir, "a.md", "# 甲改\n连接池默认为 200。")
	if changed := scanOnce(ctx, p, dir, seen); changed != 1 {
		t.Fatalf("changed file must re-import, got %d", changed)
	}
	if len(p.docs["documents"]) != 3 {
		t.Fatalf("store must hold 3 unique docs (2 original + 1 changed), got %d", len(p.docs["documents"]))
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := writeFileRaw(dir+"/"+name, content); err != nil {
		t.Fatal(err)
	}
}

package corpus

import (
	gocontext "context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/willove/cumulus/internal/retrieval"
)

// 内存假 store（只实现语料面用到的几个方法）
type memStore struct {
	docs map[string]map[string]any
}

func newMemStore() *memStore { return &memStore{docs: map[string]map[string]any{}} }

func (m *memStore) EnsureCollection(gocontext.Context, string) error { return nil }
func (m *memStore) PutStruct(_ gocontext.Context, coll, id string, v any) error {
	if m.docs[coll] == nil {
		m.docs[coll] = map[string]any{}
	}
	b, _ := jsonMarshal(v)
	var out map[string]any
	jsonUnmarshal(b, &out)
	m.docs[coll][id] = out
	return nil
}
func (m *memStore) GetStruct(_ gocontext.Context, coll, id string, out any) error {
	d, ok := m.docs[coll][id]
	if !ok {
		return os.ErrNotExist
	}
	b, _ := jsonMarshal(d)
	return jsonUnmarshal(b, out)
}
func (m *memStore) ListIDs(_ gocontext.Context, coll string, limit int) ([]string, error) {
	ids := make([]string, 0, len(m.docs[coll]))
	for id := range m.docs[coll] {
		ids = append(ids, id)
	}
	return ids, nil
}
func (m *memStore) Delete(gocontext.Context, string, string) error     { return nil }
func (m *memStore) PutValue(gocontext.Context, string, []byte) error   { return nil }
func (m *memStore) GetValue(gocontext.Context, string) ([]byte, error) { return nil, os.ErrNotExist }
func (m *memStore) Health(gocontext.Context) error                     { return nil }

func TestSaveLoadRoundTrip(t *testing.T) {
	ctx := gocontext.Background()
	st := newMemStore()
	docs := []retrieval.Document{
		{ID: "a", Body: "连接池默认 100"},
		{ID: "b", Body: "端口默认 8484"},
	}
	if err := Save(ctx, st, docs); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 docs, got %d", len(got))
	}
	byID := map[string]string{}
	for _, d := range got {
		byID[d.ID] = d.Body
	}
	if byID["a"] != "连接池默认 100" || byID["b"] != "端口默认 8484" {
		t.Fatalf("round trip lost content: %v", byID)
	}
}

func TestImportFileJSONL(t *testing.T) {
	ctx := gocontext.Background()
	st := newMemStore()
	dir := t.TempDir()
	p := filepath.Join(dir, "docs.jsonl")
	content := "{\"id\":\"x1\",\"body\":\"甲内容\"}\n{\"id\":\"x2\",\"body\":\"乙内容\"}\n\n{\"body\":\"无 id 文档\"}\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := ImportFile(ctx, st, p)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("want 3 imported, got %d", n)
	}
	got, _ := Load(ctx, st)
	if len(got) != 3 {
		t.Fatalf("store must hold 3, got %d", len(got))
	}
	// 幂等：再导入一次不翻倍（upsert）
	if _, err := ImportFile(ctx, st, p); err != nil {
		t.Fatal(err)
	}
	got2, _ := Load(ctx, st)
	if len(got2) != 3 {
		t.Fatalf("re-import must upsert, got %d", len(got2))
	}
}

func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Query：语料面不用它（评测档案才用），测试替身给一个空实现即可——
// 但**必须有**：端口加方法时替身要跟着长，漏了就是编译期红（这正是
// 接口纪律的价值：加方法要负责）。
func (m *memStore) Query(_ gocontext.Context, _ string, _ map[string]any, _, _ int, _ any) (int, error) {
	return 0, nil
}

// jsonl 导入**必须按 realm** 写。原实现硬编码 `documents`（无 realm 后缀），于是
// "-watch 吃 jsonl 语料 + 配了 CUMULUS_KEYS" 的组合下**导入 9 篇、查询一篇都读不到**
// ——真跑（长文档探针 0/10 命中）才暴露；短条文语料用 .md，所以一直没踩到。
func TestImportFileRealmWritesIntoCallerRealm(t *testing.T) {
	m := newMemStore()
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	body := "专利权期限为二十年，自申请日起计算。"
	if err := os.WriteFile(path, []byte(`{"id":"x1","body":"`+body+`"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 写进 alpha
	n, err := ImportFileRealm(gocontext.Background(), m, "alpha", path)
	if err != nil || n != 1 {
		t.Fatalf("导入应成功: n=%d err=%v", n, err)
	}
	if _, ok := m.docs[CollectionFor("alpha")]["x1"]; !ok {
		t.Fatalf("文档应写进调用者的 realm 集合（documents/alpha），实际：%v", collsOf(m))
	}
	// beta 读不到
	docsB, _ := LoadRealm(gocontext.Background(), m, "beta")
	if len(docsB) != 0 {
		t.Fatalf("beta 不该读到 alpha 的语料: %d 篇", len(docsB))
	}
	// 默认集合里也不该有（原来就是写在这儿）
	if _, ok := m.docs[Collection]["x1"]; ok {
		t.Fatalf("不该再写进默认集合 documents（那正是原 bug）: %v", collsOf(m))
	}
	// realm 空 = 单租户老路径（仍写默认集合）
	if _, err := ImportFile(gocontext.Background(), m, path); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.docs[Collection]["x1"]; !ok {
		t.Fatalf("realm 为空时应写默认集合: %v", collsOf(m))
	}
}

// collsOf 列出有文档的集合（断言信息用）。
func collsOf(m *memStore) []string {
	out := make([]string, 0, len(m.docs))
	for coll, docs := range m.docs {
		if len(docs) > 0 {
			out = append(out, coll)
		}
	}
	return out
}

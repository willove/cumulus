package evaldata

import (
	"os"
	"path/filepath"
	"testing"
)

// 本地小语料装载：内容寻址、去重、金标缺失只警告不拒（那是显式构造的
// "该拒答"对照组，不是数据错）。指纹随内容走——同语料不同题集块必须
// 得到相同 CorpusSHA 与不同 ItemsSHA（校准/锁箱划分靠这个）。
func TestLoadJSONLLocalSet(t *testing.T) {
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus.jsonl")
	items := filepath.Join(dir, "items.jsonl")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(corpus, []byte(
		`{"id":"d1","body":"部署手册：服务端口默认 8484。"}`+"\n"+
			`{"body":"连接池最大连接数默认为 100。"}`+"\n"+ // 无 id → 内容哈希
			`{"id":"d1","body":"部署手册：服务端口默认 8484。"}`+"\n"+ // 重复 id → 去重
			`{"id":"d3","body":"   "}`+"\n"), 0o644)) // 空正文 → 丢
	must(os.WriteFile(items, []byte(
		`{"id":"q1","question":"服务端口是多少","answer":"8484","gold_ids":["d1"]}`+"\n"+
			`{"question":"连接数是多少","answer":"100","gold_ids":[]}`+"\n"+ // 无金标 → 警告
			`{"id":"q3","question":"库外问题","answer":"x","gold_ids":["missing"]}`+"\n"), 0o644))

	set, warns, err := LoadJSONL(corpus, items)
	must(err)
	if len(set.Docs) != 2 {
		t.Fatalf("want 2 unique non-empty docs, got %d", len(set.Docs))
	}
	if len(set.Items) != 3 {
		t.Fatalf("want 3 items, got %d", len(set.Items))
	}
	if set.Docs[1].ID == "" || len(set.Docs[1].ID) != 12 {
		t.Fatalf("missing corpus id must be content-addressed, got %q", set.Docs[1].ID)
	}
	if set.CorpusSHA == "" || set.ItemsSHA == "" || set.CorpusSHA == set.ItemsSHA {
		t.Fatalf("fingerprints must be present and distinct: %s / %s", set.CorpusSHA, set.ItemsSHA)
	}
	if len(warns) == 0 {
		t.Fatal("missing gold must warn visibly (it is an explicit construction, not silence)")
	}

	// 同一语料 + 只留一题：CorpusSHA 相同、ItemsSHA 不同
	must(os.WriteFile(items, []byte(`{"id":"q1","question":"服务端口是多少","answer":"8484","gold_ids":["d1"]}`+"\n"), 0o644))
	sub, _, err := LoadJSONL(corpus, items)
	must(err)
	if sub.CorpusSHA != set.CorpusSHA {
		t.Fatalf("same corpus must keep the same CorpusSHA: %s vs %s", sub.CorpusSHA, set.CorpusSHA)
	}
	if sub.ItemsSHA == set.ItemsSHA {
		t.Fatal("different item sets must have different ItemsSHA")
	}
}

func TestLoadJSONLRejectsEmptyQuestion(t *testing.T) {
	dir := t.TempDir()
	corpus := filepath.Join(dir, "corpus.jsonl")
	items := filepath.Join(dir, "items.jsonl")
	if err := os.WriteFile(corpus, []byte(`{"id":"d1","body":"正文"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(items, []byte(`{"id":"q1","question":"  ","answer":"x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadJSONL(corpus, items); err == nil {
		t.Fatal("empty question must be rejected")
	}
}

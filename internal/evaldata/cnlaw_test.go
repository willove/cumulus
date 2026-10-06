package evaldata

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTmp(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "mini.jsonl")
	if err := os.WriteFile(p, []byte(joinLines(lines...)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func joinLines(lines ...string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out + "\n"
}

func TestLoadCNLawDedupesAndHashes(t *testing.T) {
	p := writeTmp(t,
		`{"anchor":"专利率啥用？","positive":"title: 专利法 第一条 | text: 第一条 为了保护专利权","negative":"x"}`,
		`{"anchor":"专利率啥用？2","positive":"title: 专利法 第一条 | text: 第一条 为了保护专利权","negative":"x"}`,
		`{"anchor":"合同怎签？","positive":"title: 合同法 第二条 | text: 第二条 合同是平等主体","negative":"y"}`,
	)
	set, err := LoadCNLaw(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Docs) != 2 {
		t.Fatalf("duplicate positive must dedupe to 2 docs, got %d", len(set.Docs))
	}
	if len(set.Items) != 3 {
		t.Fatalf("want 3 items, got %d", len(set.Items))
	}
	if set.Items[0].GoldIDs[0] != set.Docs[0].ID {
		t.Fatalf("gold must be the positive doc id: %v", set.Items[0].GoldIDs)
	}
	if set.Items[0].Answer != "第一条 为了保护专利权" {
		t.Fatalf("answer should be the passage body, got %q", set.Items[0].Answer)
	}
	// 内容寻址：两次装载同一文件，指纹相同
	set2, _ := LoadCNLaw(p, 10)
	if set.CorpusSHA != set2.CorpusSHA || set.ItemsSHA != set2.ItemsSHA {
		t.Fatal("same content must give same fingerprints")
	}
	// 样本号生效
	set3, _ := LoadCNLaw(p, 2)
	if len(set3.Items) != 2 {
		t.Fatalf("sampleN must cap items, got %d", len(set3.Items))
	}
}

func TestLoadCNLawMissingFile(t *testing.T) {
	if _, err := LoadCNLaw("/nonexistent/x.jsonl", 1); err == nil {
		t.Fatal("missing file must error")
	}
}

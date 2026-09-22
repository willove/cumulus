package minilm

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// refCase mirrors one entry of testdata/reference.json.
type refCase struct {
	Text      string    `json:"text"`
	InputIDs  []int     `json:"input_ids"`
	Attention []int     `json:"attention_mask"`
	Embedding []float64 `json:"embedding"`
}

func loadDir(t *testing.T) string {
	t.Helper()
	d := DefaultDir()
	if d == "" || !fileExists(filepath.Join(d, "model.safetensors")) || !fileExists(filepath.Join(d, "unigram.json")) {
		t.Skipf("minilm: model dir not available (%s) — spike tests need the weights", d)
	}
	return d
}

func loadRef(t *testing.T) []refCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs []refCase
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestViterbiFixture(t *testing.T) {
	// Always-on logic test with a synthetic vocab: score-optimal segmentation,
	// longer-piece tie preference, and the unk path.
	tk := &Tokenizer{
		score:   map[string]float64{"ab": -1, "a": -3, "b": -3, "ab!": -0.5},
		id:      map[string]int{"ab": 10, "a": 11, "b": 12, "ab!": 13},
		maxRune: 3,
		minScr:  -5,
	}
	tk.normMap = map[rune]string{}
	if got := tk.viterbi("ab"); len(got) != 1 || got[0] != 10 {
		t.Fatalf("viterbi(ab) = %v, want [10]", got)
	}
	// "ab!" via "ab!"(-0.5) beats "ab"+"!"-unk; "!": unk = min(-5)-10 = -15,
	// so ab+unk = -16 < ab! = -0.5.
	if got := tk.viterbi("ab!"); len(got) != 1 || got[0] != 13 {
		t.Fatalf("viterbi(ab!) = %v, want [13]", got)
	}
	// Pure unk: adjacent unknown runes FUSE into one <unk> (HF fuse_unk);
	// unknowns separated by a vocab piece stay distinct.
	tk2 := &Tokenizer{
		score:   map[string]float64{"▁": 0, "a": -1},
		id:      map[string]int{"▁": 0, "a": 5},
		maxRune: 1, minScr: -5, normMap: map[rune]string{},
	}
	if got := tk2.viterbi("▁€@"); len(got) != 2 {
		t.Fatalf("viterbi fused unknowns = %v, want [▁, unk]", got)
	}
	if got := tk2.viterbi("▁€a@"); len(got) != 4 {
		t.Fatalf("viterbi split unknowns = %v, want [▁, unk, a, unk]", got)
	}
}

func TestEncodeMatchesReference(t *testing.T) {
	dir := loadDir(t)
	tk, err := LoadTokenizer(dir)
	if err != nil {
		t.Fatal(err)
	}
	refs := loadRef(t)
	for _, r := range refs {
		got := tk.Encode(r.Text)
		if len(got) != len(r.InputIDs) {
			t.Fatalf("text %q: ids len %d, want %d\n got %v\nwant %v", r.Text, len(got), len(r.InputIDs), got, r.InputIDs)
		}
		for i := range got {
			if got[i] != r.InputIDs[i] {
				t.Fatalf("text %q: ids[%d]=%d want %d\n got %v\nwant %v", r.Text, i, got[i], r.InputIDs[i], got, r.InputIDs)
			}
		}
	}
}

func TestEncodeMatchesReferenceLarge(t *testing.T) {
	dir := loadDir(t)
	tk, err := LoadTokenizer(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/large_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs []refCase
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	idsBad := 0
	for _, r := range refs {
		got := tk.Encode(r.Text)
		if len(got) != len(r.InputIDs) {
			idsBad++
			t.Errorf("text %q: ids len %d want %d", truncate(r.Text, 20), len(got), len(r.InputIDs))
			continue
		}
		for i := range got {
			if got[i] != r.InputIDs[i] {
				idsBad++
				t.Errorf("text %q: ids[%d]=%d want %d", truncate(r.Text, 20), i, got[i], r.InputIDs[i])
				break
			}
		}
	}
	if idsBad > 0 {
		t.Fatalf("ids mismatches: %d/%d", idsBad, len(refs))
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	minCos := 2.0
	for _, r := range refs {
		vec := m.EncodeTokenVectors(r.InputIDs)
		var dot, na, nb float64
		for i := range vec {
			dot += float64(vec[i]) * r.Embedding[i]
			na += float64(vec[i]) * float64(vec[i])
			nb += r.Embedding[i] * r.Embedding[i]
		}
		cos := dot / math.Sqrt(na*nb)
		if cos < minCos {
			minCos = cos
		}
		if cos < 0.999 {
			t.Errorf("text %q: cos %.6f < 0.999", truncate(r.Text, 20), cos)
		}
	}
	t.Logf("large set: %d cases, ids exact, min cos = %.7f", len(refs), minCos)
}

func TestTokenizeFuzz(t *testing.T) {
	dir := loadDir(t)
	tk, err := LoadTokenizer(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("testdata/fuzz_ids.json")
	if err != nil {
		t.Fatal(err)
	}
	var refs []struct {
		Text     string `json:"text"`
		InputIDs []int  `json:"input_ids"`
	}
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, r := range refs {
		got := tk.Encode(r.Text)
		if len(got) != len(r.InputIDs) {
			bad++
			if bad <= 5 {
				t.Errorf("text %q: ids len %d want %d", truncate(r.Text, 24), len(got), len(r.InputIDs))
			}
			continue
		}
		for i := range got {
			if got[i] != r.InputIDs[i] {
				bad++
				if bad <= 5 {
					t.Errorf("text %q: ids[%d]=%d want %d", truncate(r.Text, 24), i, got[i], r.InputIDs[i])
				}
				break
			}
		}
	}
	if bad > 0 {
		t.Fatalf("fuzz ids mismatches: %d/%d", bad, len(refs))
	}
}

func TestForwardMatchesReference(t *testing.T) {
	dir := loadDir(t)
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	refs := loadRef(t)
	for _, r := range refs {
		vec := m.EncodeTokenVectors(r.InputIDs)
		if len(vec) != EmbeddingDim {
			t.Fatalf("text %q: dim %d", r.Text, len(vec))
		}
		var dot, na, nb float64
		for i := range vec {
			dot += float64(vec[i]) * r.Embedding[i]
			na += float64(vec[i]) * float64(vec[i])
			nb += r.Embedding[i] * r.Embedding[i]
		}
		cos := dot / math.Sqrt(na*nb)
		if cos < 0.999 {
			t.Fatalf("text %q: cos-sim %.6f < 0.999", r.Text, cos)
		}
		t.Logf("text %q: cos=%.6f", truncate(r.Text, 18), cos)
	}
}

func TestEmbedderShape(t *testing.T) {
	if !Available() {
		t.Skip("minilm: model not available")
	}
	e := New(DefaultDir())
	if e.Dims() != EmbeddingDim {
		t.Fatalf("dims=%d", e.Dims())
	}
	vecs, err := e.Embed(context.Background(), []string{"连接池", "语义缓存索引"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != EmbeddingDim {
		t.Fatalf("embed shape %dx%d", len(vecs), len(vecs[0]))
	}
	var norm float64
	for _, x := range vecs[0] {
		norm += x * x
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-6 {
		t.Fatalf("vector not normalized: %f", norm)
	}
}

func BenchmarkEncodeQuery(b *testing.B) {
	d := DefaultDir()
	if d == "" || !fileExists(filepath.Join(d, "model.safetensors")) {
		b.Skip("minilm: model not available")
	}
	m, err := Load(d)
	if err != nil {
		b.Fatal(err)
	}
	ids := m.Tokenizer().Encode("语义缓存索引的查询向量是多少")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.EncodeTokenVectors(ids)
	}
}

func BenchmarkEncodeDoc128(b *testing.B) {
	d := DefaultDir()
	if d == "" || !fileExists(filepath.Join(d, "model.safetensors")) {
		b.Skip("minilm: model not available")
	}
	m, err := Load(d)
	if err != nil {
		b.Fatal(err)
	}
	ids := m.Tokenizer().Encode("数据库索引与查询优化是一篇关于存储引擎内部结构的中文技术文档，涵盖 B+ 树、页布局、缓冲池管理与查询计划选择，面向后端工程师。" +
		"数据库索引与查询优化是一篇关于存储引擎内部结构的中文技术文档，涵盖 B+ 树、页布局、缓冲池管理与查询计划选择，面向后端工程师。" +
		"数据库索引与查询优化是一篇关于存储引擎内部结构的中文技术文档，涵盖 B+ 树、页布局、缓冲池管理与查询计划选择，面向后端工程师。")
	if len(ids) > 128 {
		ids = ids[:128]
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.EncodeTokenVectors(ids)
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

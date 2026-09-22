package minilm

import (
	"math"
	"testing"
)

// TestBatchEncodeMatchesIndividual locks the batch path (sync.Pool workspaces
// + goroutines): every batch vector must be bitwise-equal to the same text
// encoded alone. A regression here poisons the body_embed backfill with
// cross-text contamination (identical-looking vectors, wrong KNN ranking).
func TestBatchEncodeMatchesIndividual(t *testing.T) {
	dir := loadDir(t)
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{
		"title: 中华人民共和国专利法 第一条 | text: 第一条 为了保护专利权人的合法权益，鼓励发明创造，推动发明创造的应用",
		"title: 中华人民共和国专利法 第三条 | text: 第三条 国务院专利行政部门负责管理全国的专利工作",
		"title: 湖北省专利条例 第一条 | text: 第一条 为了鼓励发明创造，保护专利权人的合法权益",
		"completely different english text about buffer pools and databases",
	}
	batch, err := m.Encode(t.Context(), texts)
	if err != nil {
		t.Fatal(err)
	}
	cos := func(a, b []float32) float64 {
		var d, na, nb float64
		for i := range a {
			d += float64(a[i]) * float64(b[i])
			na += float64(a[i]) * float64(a[i])
			nb += float64(b[i]) * float64(b[i])
		}
		return d / math.Sqrt(na*nb)
	}
	for i, tText := range texts {
		single := m.EncodeTokenVectors(m.tok.Encode(tText))
		c := cos(batch[i], single)
		if c < 0.999999 {
			t.Fatalf("text %d: batch vs individual cos %.7f < 0.999999 (batch path corrupts vectors)", i, c)
		}
		ids := m.tok.Encode(tText)
		var n1 float64
		for _, x := range single {
			n1 += float64(x) * float64(x)
		}
		t.Logf("text %d: ids=%d batch-vs-single cos=%.7f norm=%.4f head=%v",
			i, len(ids), c, math.Sqrt(n1), head5(single))
	}
	for i := 0; i < len(batch); i++ {
		for j := i + 1; j < len(batch); j++ {
			if c := cos(batch[i], batch[j]); c > 0.999 {
				t.Fatalf("texts %d and %d are nearly identical vectors (cos %.6f) — batch contamination", i, j, c)
			}
		}
	}
}

func head5(v []float32) []float64 {
	out := make([]float64, 5)
	for i := range out {
		out[i] = float64(v[i])
	}
	return out
}

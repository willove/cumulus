package embed

import (
	"math"
	"testing"
)

func TestCosineBasics(t *testing.T) {
	same := Cosine([]float32{1, 2, 3}, []float32{1, 2, 3})
	if math.Abs(same-1) > 1e-6 {
		t.Fatalf("same vector must be 1, got %v", same)
	}
	orth := Cosine([]float32{1, 0}, []float32{0, 1})
	if orth != 0 {
		t.Fatalf("orthogonal must be 0, got %v", orth)
	}
	opp := Cosine([]float32{1, 0}, []float32{-1, 0})
	if math.Abs(opp+1) > 1e-6 {
		t.Fatalf("opposite must be -1, got %v", opp)
	}
	zero := Cosine([]float32{0, 0}, []float32{1, 1})
	if zero != 0 {
		t.Fatalf("zero vector must be 0, got %v", zero)
	}
	// 未归一化也要给对（内部除了法）
	un := Cosine([]float32{2, 0}, []float32{5, 0})
	if math.Abs(un-1) > 1e-6 {
		t.Fatalf("unnormalized parallel must be 1, got %v", un)
	}
	// 维度不符不 panic，按不相似
	if Cosine([]float32{1}, []float32{1, 2}) != 0 {
		t.Fatal("dim mismatch must be 0, not a crash")
	}
}

func TestL2Norm(t *testing.T) {
	v := L2Norm([]float32{3, 4})
	if math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Fatalf("want unit vector, got %v", v)
	}
	// 幂等
	v2 := L2Norm(append([]float32(nil), v...))
	if v2[0] != v[0] {
		t.Fatal("normalizing twice must be a no-op")
	}
	// 零向量不炸
	z := L2Norm([]float32{0, 0})
	if z[0] != 0 || z[1] != 0 {
		t.Fatal("zero vector must stay zero")
	}
}

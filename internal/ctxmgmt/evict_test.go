package ctxmgmt

import "testing"

func vec(x, y float64) []float32 { return []float32{float32(x), float32(y)} }

func win(id, span, text string, score float64) Window {
	return Window{SourceID: id, Span: span, Text: text, Score: score}
}

// 语义近重合并：cosine ≥ 0.92 的合并，且合并留痕（并进谁、余弦多少）。
func TestApplyMergesNearDuplicates(t *testing.T) {
	ws := []Window{
		win("a", "rune[0:5]", "连接池默认 100", 5),
		win("b", "rune[0:5]", "连接池默认 100（改写）", 4),
	}
	vecs := [][]float32{vec(1, 0), vec(0.99, 0.14)} // cosine ≈ 0.99
	kept, log := Apply(ws, vecs, Budget{DedupCosine: 0.92})
	if len(kept) != 1 || kept[0].SourceID != "a" {
		t.Fatalf("near-dup must merge into the first, got %v", kept)
	}
	if len(log.Merged) != 1 || log.Merged[0].Merged.SourceID != "b" {
		t.Fatalf("merge must be logged: %+v", log.Merged)
	}
}

// 低于阈值不合并（不是所有相似都合）。
func TestApplyKeepsBelowThreshold(t *testing.T) {
	ws := []Window{
		win("a", "rune[0:5]", "连接池", 5),
		win("b", "rune[0:5]", "财务报表", 4),
	}
	vecs := [][]float32{vec(1, 0), vec(0, 1)} // cosine 0
	kept, log := Apply(ws, vecs, Budget{DedupCosine: 0.92})
	if len(kept) != 2 {
		t.Fatalf("orthogonal must both survive, got %v", kept)
	}
	if len(log.Merged) != 0 {
		t.Fatal("no merge expected")
	}
}

// 向量缺席/不匹配：语义合并不做，其余纪律照做（降级可见，不失败）。
func TestApplyDegradesWithoutVectors(t *testing.T) {
	ws := []Window{
		win("a", "rune[0:5]", "x", 5),
		win("b", "rune[0:5]", "x", 4),
	}
	kept, _ := Apply(ws, nil, Budget{DedupCosine: 0.92})
	if len(kept) != 2 {
		t.Fatalf("no vectors must skip merging, got %v", kept)
	}
}

// 按源配额 + 窗口预算：丢弃留痕（原因 per-source / budget）。
func TestApplyPerSourceAndBudget(t *testing.T) {
	ws := []Window{
		win("a", "rune[0:5]", "1", 3),
		win("a", "rune[5:9]", "2", 2),
		win("a", "rune[9:12]", "3", 1),
		win("b", "run[0:5]", "4", 9),
	}
	kept, log := Apply(ws, nil, Budget{PerSourceMax: 2, MaxWindows: 3})
	if len(kept) != 3 {
		t.Fatalf("want 3 kept, got %v", kept)
	}
	reasons := map[string]int{}
	for _, d := range log.Dropped {
		reasons[d.Reason]++
	}
	if reasons["per-source"] != 1 {
		t.Fatalf("per-source drop must be logged: %+v", log.Dropped)
	}
}

// 预算按打分保留（同分保序）——只蒸干不加水。
func TestApplyBudgetKeepsTopScores(t *testing.T) {
	ws := []Window{
		win("a", "s1", "low", 1),
		win("b", "s1", "high", 9),
		win("c", "s1", "mid", 5),
	}
	kept, _ := Apply(ws, nil, Budget{MaxWindows: 2})
	if len(kept) != 2 || kept[0].SourceID != "b" || kept[1].SourceID != "c" {
		t.Fatalf("budget must keep top scores, got %v", kept)
	}
}

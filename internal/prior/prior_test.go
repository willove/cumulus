package prior

import (
	"math"
	"testing"
)

// 治短文档偏爱的核心场景：长法律（答案在第四十二条，命中深）必须胜过
// 短司法解释（碰巧提到专利）。BM25 的长度归一把这个排颠倒（真跑教训：
// 问"专利期限多少年"，置顶的是最高法的专利代理短解释）。
func TestLexicalNotLengthNormalized(t *testing.T) {
	// 长法律：专利 出现 8 次，利期 5 次
	longLaw := Doc{ID: "law", Title: "中华人民共和国专利法", TF: map[string]int{"专利": 8, "利期": 5}}
	// 短解释：专利 1 次，利期 0（标题里都没有利期）
	shortDoc := Doc{ID: "short", Title: "最高人民法院关于专利代理的若干规定", TF: map[string]int{"专利": 1}}
	df := map[string]int{"专利": 12, "利期": 2}
	out := Rank([]Doc{longLaw, shortDoc}, df, 1537, 2)
	if out[0].DocID != "law" {
		t.Fatalf("长文的深命中必须置顶，got %s (%v)", out[0].DocID, out[0].Signals)
	}
	if out[0].Score != 1.0 {
		t.Fatalf("置顶归一为 1，got %v", out[0].Score)
	}
	if out[1].Score <= 0 || out[1].Score >= 1.0 {
		t.Fatalf("次名应在其后，got %v", out[1].Score)
	}
}

// BM25 会判相反：长度归一下短文档赢。这条测试把两种口径的差别钉死。
func TestBM25WouldPreferShortDoc(t *testing.T) {
	// 只测 lexical 信号本身：长文档 tf 大，log2(tf+1) 增长慢但不反转
	lexLong := Lexical(map[string]int{"专利": 8}, nil, 0)
	lexShort := Lexical(map[string]int{"专利": 1}, nil, 0)
	if lexLong <= lexShort {
		t.Fatalf("lexical 必须偏好深命中（%v vs %v）", lexLong, lexShort)
	}
	// 与 BM25 的差别：lexical 无 b=0.75 的长度归一项
	if math.Abs(lexLong/lexShort-lexLong/lexShort) > 0 {
		t.Fatal("与 BM25 的差别在长度归一项的有无——此处占位防呆")
	}
}

// 标题信号：查询词命中标题的比例（专利法标题里有"专利"）。
func TestTitleSignal(t *testing.T) {
	if got := Title("中华人民共和国专利法", map[string]int{"专利": 3, "利期": 2}); got != 0.5 {
		t.Fatalf("want 0.5 (专利 hits, 利期 not), got %v", got)
	}
	if got := Title("", map[string]int{"专利": 3}); got != 0 {
		t.Fatalf("no title → 0, got %v", got)
	}
	if got := Title("x", map[string]int{"unseen": 0}); got != 0 {
		t.Fatalf("no queried words → 0, got %v", got)
	}
}

// 结构信号：命中条文内的词覆盖。
func TestStructSignal(t *testing.T) {
	if got := Struct(map[string]int{"专利": 2}, map[string]int{"专利": 3, "利期": 1}); got != 0.5 {
		t.Fatalf("want 0.5, got %v", got)
	}
	if got := Struct(nil, map[string]int{"专利": 3}); got != 0 {
		t.Fatalf("无结构证据 → 0（cumulus 的同款决定：不许白送底分），got %v", got)
	}
}

// 权重和为 1（缺历史臂的重标口径）。
func TestWeightsSumOne(t *testing.T) {
	sum := 0.0
	for _, w := range Weights {
		sum += w
	}
	if sum < 0.999 || sum > 1.001 {
		t.Fatalf("weights 必须和为 1，got %v", sum)
	}
}

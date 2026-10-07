// Package prior 是文档级的多信号置信（cumulus internal/prior 的移植，
// LENS Eq.6 的融合口径）。
//
// 它治的是 BM25 的**短文档偏见**：BM25 的长度归一把"答案在长法律里"的
// 真实命中压到"短解释里碰巧提到"的下面——真跑教训：问"专利期限多少年"，
// 答案在专利法第四十二条（20 年），置顶的是最高法一篇讲专利代理的短
// 司法解释。本包的 lexical 信号**不做长度归一**：命中多就是多。
//
// 信号族（按本项目语料可得性取舍，权重照 cumulus 等比重标）：
//   - lexical 0.50：Σ idf(t) × (1 + log2(tf+1))——长文档命中深者胜；
//   - title    0.25：查询词命中文档标题的比例（专利法的标题里有"专利"）；
//   - struct   0.25：查询词命中**命中所在条文**的比例（第 X 条内的覆盖）。
//
// 历史信号（cumulus 的 SigHistory）本轮回接口不接——复用库存着历史，
// 但先让三个词面信号自己证明能治偏爱，再谈加料。
package prior

import (
	"math"
	"sort"
)

// Signal 名字（响应里逐信号可查——取舍要看得见，同 query 包的先例）。
const (
	SigLexical = "lexical"
	SigTitle   = "title"
	SigStruct  = "struct"
)

// Weights 融合权重（缺历史臂时重标：原 0.40+0.20(path→title)+0.15(struct)
// +0.15(history) 按比例归到三臂）。
var Weights = map[string]float64{
	SigLexical: 0.60,
	SigTitle:   0.25,
	SigStruct:  0.15,
}

// Doc 是 prior 需要的最小文档视图（retrieval.Index 喂给它）。
type Doc struct {
	ID    string
	Title string
	// TF 查询词的文档内词频（0 = 没命中）
	TF map[string]int
	// Struct 命中所在条文内的词频（结构信号）
	Struct map[string]int
}

// FileScore 是一个文档的融合分与逐信号值。
type FileScore struct {
	DocID   string             `json:"doc_id"`
	Score   float64            `json:"score"` // 融合后，按置顶归一到 (0,1]
	Signals map[string]float64 `json:"signals"`
}

// Rank 对候选文档融合多信号分，按分排序（同分按 ID 保确定性），
// topK 截断。置顶分归一：分值是相对的，绝对数没有意义。
func Rank(docs []Doc, df map[string]int, n int, topK int) []FileScore {
	if len(docs) == 0 {
		return nil
	}
	out := make([]FileScore, 0, len(docs))
	for _, d := range docs {
		sig := map[string]float64{
			SigLexical: Lexical(d.TF, df, n),
			SigTitle:   Title(d.Title, d.TF),
			SigStruct:  Struct(d.Struct, d.TF),
		}
		fused := 0.0
		for k, w := range Weights {
			fused += w * sig[k]
		}
		out = append(out, FileScore{DocID: d.ID, Score: fused, Signals: sig})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].DocID < out[j].DocID
	})
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	if out[0].Score > 0 {
		mx := out[0].Score
		for i := range out {
			out[i].Score /= mx
		}
	}
	return out
}

// Lexical：idf 加权词频，**无长度归一**——这是治短文档偏见的核心。
func Lexical(tf map[string]int, df map[string]int, n int) float64 {
	sc := 0.0
	for term, f := range tf {
		if f == 0 {
			continue
		}
		idf := 1.0
		if d := df[term]; d > 0 && n > 0 {
			idf = 1.0 + math.Log2(float64(n)/float64(d))
		}
		sc += idf * (1.0 + math.Log2(float64(f)+1))
	}
	return sc
}

// Title：查询词命中标题的比例（标题是文档身份——专利法的标题里有"专利"，
// 最高法的司法解释标题里没有）。
func Title(title string, tf map[string]int) float64 {
	hits, total := 0, 0
	low := lower(title)
	for term, f := range tf {
		if f == 0 {
			continue // 只数真正查了的词
		}
		total++
		if low != "" && contains(low, lower(term)) {
			hits++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

// Struct：命中所在条文内的查询词覆盖（同 Title 的比例口径，作用域从
// 标题换成条文——法律语料的结构信号）。
func Struct(structTF map[string]int, tf map[string]int) float64 {
	if len(structTF) == 0 {
		return 0
	}
	hits, total := 0, 0
	for term, f := range tf {
		if f == 0 {
			continue
		}
		total++
		if structTF[term] > 0 {
			hits++
		}
	}
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total)
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

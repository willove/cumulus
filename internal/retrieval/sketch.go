package retrieval

// sketch.go —— 每篇文档的**词项指纹**（Bloom sketch），让冷文档能被"扫"出来而不必
// 把它的倒排放进内存。
//
// 为什么需要它（实测）：1800 篇 / 1.49 MB 语料的索引占 **26.5 MB**，其中
// **倒排 26.28 MB（99.4%）**、正文副本只占 0.6%。也就是说"全文放内存"的成本几乎
// 全在倒排上；而文档是线性增长的（每 MB 语料 ≈ 17 MB 内存），所以不做分层就迟早
// OOM。
//
// 硬要求：**不能有假阴性**。
//
// 指纹用于"这篇可能命中吗"的**保守筛选**：说"不命中"就等于让文档从语料里消失，
// 引擎会开始答"语料里没有依据"——而语料里其实有。那是我们整场都在对抗的失败模式
// （不知道就说不知道的前提是**不把知道的当成不知道**），所以这里用 Bloom（保证
// 假阳性、不会假阴性），不用"只存高频词"那种省内存但会漏的方案。
//
// 假阳性怎么办：**取回正文后按真实 BM25 精算**。多取回来几篇只是慢一点，
// 漏掉一篇是答错——两者的代价根本不对等。

import (
	"hash/fnv"
	"sort"
)

// sketchK 是哈希函数个数（Bloom 经典取值 2：内存与速度的平衡点）。
const sketchK = 2

// DocSketch 是一篇文档的词项指纹（定长位数组）。
//
// 尺寸是**按语料词表估的**（见 NewSketcher），不是固定常量：词表越大要越多位，
// 否则位数组满了之后每个查询都"可能命中"，筛选就失效了（那等于没筛选）。
type DocSketch struct {
	Bits []uint64 // 位数组
}

// Has 判断一个词是否**可能**出现在该文档里。
//
// 双哈希（一个 32 位哈希拆成两半）而不是两个独立哈希：省一半哈希计算，且
// "同族哈希"对 Bloom 的表现足够（false positive 略高一点点，但代价是"多取回
// 一篇正文"，可接受）。
func (s *DocSketch) Has(term string) bool {
	if s == nil || len(s.Bits) == 0 {
		return false
	}
	h1, h2 := hashPair(term)
	for i := 0; i < sketchK; i++ {
		pos := (h1 + uint64(i)*h2) % uint64(len(s.Bits)*64)
		if s.Bits[pos/64]&(1<<(pos%64)) == 0 {
			return false // 该位没置 = 这个词一定不在（**无假阴性**）
		}
	}
	return true
}

// Set 把词加进指纹。
func (s *DocSketch) Set(term string) {
	if s == nil || len(s.Bits) == 0 {
		return
	}
	h1, h2 := hashPair(term)
	for i := 0; i < sketchK; i++ {
		pos := (h1 + uint64(i)*h2) % uint64(len(s.Bits)*64)
		s.Bits[pos/64] |= 1 << (pos % 64)
	}
}

// Bytes 是指纹的字节数（容量统计用）。
func (s *DocSketch) Bytes() int {
	if s == nil {
		return 0
	}
	return len(s.Bits) * 8
}

// hashPair 是 FNV-1a + 一个二次混合，拆成 (h1, h2)。纯 CPU、无分配。
func hashPair(term string) (uint64, uint64) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(term))
	x := h.Sum64()
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x, x | 1 // h2 必须是奇数，否则 (h1 + i*h2) 永远落在同一组位上
}

// newSketchFor 按**这一篇自己的词数**给指纹定尺。
//
// 为什么按篇而不是按全语料词表（真跑教训）：第一版按 corpus 词表定尺，于是
// 每篇固定几 KB——一篇只有 30 个不同词的文档也拿到 vocab×10 位。实测那批 2000 篇
// 语料上：**分层反而比全内存多花 16%**（指纹 7.4MB > 省下的倒排）。改成按篇定尺后，
// 指纹总量与"被省掉的倒排"成正比（约 1.25 字节/词 vs 倒排约 40 字节/词）。
//
// bitsPerTerm 取 10（Bloom 常用的 1.44×ln2≈1 位/词定理给足余量后仍很宽松），
// 并给上下限：太小会因位满而"全命中"（筛选失效），太大就白占内存。
const (
	bitsPerTerm    = 10
	minSketchWords = 4    // 32 字节：极短文档的下限
	maxSketchWords = 1024 // 8 KB/篇：超过这个不如直接放倒排
)

func newSketchFor(terms []string) *DocSketch {
	distinct := 0
	seen := make(map[string]struct{}, len(terms))
	for _, t := range terms {
		if t == "" {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		distinct++
	}
	words := (distinct*bitsPerTerm + 63) / 64
	if words < minSketchWords {
		words = minSketchWords
	}
	if words > maxSketchWords {
		words = maxSketchWords
	}
	s := &DocSketch{Bits: make([]uint64, words)}
	for _, t := range terms {
		s.Set(t)
	}
	return s
}

// coldCandidate 是冷区候选（一篇文档 + 它"命中"了几个查询词）。
type coldCandidate struct {
	id   string
	hits int // 命中的查询词个数（不是分数，只是候选序）
}

// sketchCandidates 从**冷区**里挑出可能命中的文档，按命中词数降序。
//
// 上限 `limit` 是**性能护栏**：sketch 筛不出精确分数，只能按"命中几个词"排，多取就是
// 白取（每多取一篇就要从磁盘读正文）。默认取 3×limit+5，给假阳性留余量。
func sketchCandidates(sk map[string]*DocSketch, cold map[string]int, terms []string, limit int) []coldCandidate {
	if len(sk) == 0 || len(terms) == 0 {
		return nil
	}
	want := 3*limit + 5
	out := make([]coldCandidate, 0, 64)
	for id, s := range sk {
		h := 0
		for _, t := range terms {
			if s.Has(t) {
				h++
			}
		}
		if h == 0 {
			continue // 一个查询词都没有 → **一定不命中**（无假阴性）
		}
		out = append(out, coldCandidate{id: id, hits: h})
	}
	// 命中词数降序；同数按 id 排序（读数稳定，不抖动）
	sort.Slice(out, func(i, j int) bool {
		if out[i].hits != out[j].hits {
			return out[i].hits > out[j].hits
		}
		return out[i].id < out[j].id
	})
	if len(out) > want {
		out = out[:want]
	}
	return out
}

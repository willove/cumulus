package retrieval

// tier.go —— 热区/冷区分层索引：倒排只放**热区**文档，冷文档靠词项指纹 + 按需取原文。
//
// 为什么（实测数字）：1800 篇 / 1.49 MB 语料 → 索引 26.5 MB，其中倒排 26.28 MB
// （99.4%）。每 MB 语料 ≈ 17 MB 内存，**线性增长且没有上限**，所以不能全放内存。
//
// 三条纪律：
//
//  1. **冷文档不许消失**：筛选只做"可能命中吗"，命中就取回正文**按真实 BM25 精算**。
//     假阳性只浪费一次读盘；假阴性会让引擎答"语料里没有"——那是最贵的错误。
//  2. **全局统计量不随分层变化**：df（IDF 用）、doc 长度、N/AvgLen 都是**全量**的。
//     否则热区文档会因为"邻居少了"而算得比冷文档高，同一份语料里分数不可比。
//  3. **分层是内存策略，不是正确性开关**：把预算调到 ≥ 语料规模时行为与旧的
//     全内存索引**逐字节一致**（小个人库永远走老路），所以这条改动对现有读数零影响。

import (
	"fmt"
	"math"
	"sort"
	"sync"
)

// TierPolicy 是分层的预算策略（记忆有界的那部分）。
type TierPolicy struct {
	// HotDocs 是热区文档数上限（0 = 不限，即全内存，与旧行为一致）。
	// 用**篇数**而不是字节当主口径：篇数可预期、可解释（"最多 2000 篇在内存里"），
	// 而字节数取决于词形分布，讲不清楚。
	HotDocs int
	// PromoteScore 是**升权门槛**：冷文档的精算分达到它才进热区。
	// 0 = 只要进了 top-k 就升（默认）。调高它能让热区只装"真的常被命中"的文档。
	PromoteScore float64
}

// DefaultTierPolicy 是默认策略。
//
// HotDocs=2000 的依据：2000 篇 × 2k 字 ≈ 4 MB 语料 ≈ **70 MB 内存**（按实测的
// 17.6 倍），在一台普通开发机上很宽裕；而再往上加内存收益递减（个人库里真正被
// 反复访问的通常就是那几百篇）。想改就 `CUMULUS_HOT_DOCS`。
func DefaultTierPolicy() TierPolicy { return TierPolicy{HotDocs: 2000} }

// Loader 按文档 id 取回正文（冷文档升权/精算时用）。取不到返回 ok=false。
type Loader func(docID string) (body string, ok bool)

// tiered 是 Index 的分层状态（Build 出来的索引没有它 = 全内存）。
type tiered struct {
	policy TierPolicy
	loader Loader

	mu sync.Mutex
	// hot 是当前在内存里有倒排的文档集合。
	hot map[string]bool
	// sketches 是**全量**文档的词项指纹（冷热都有：热文档被降权时直接可用）。
	sketches map[string]*DocSketch
	// lru 是热区文档的最近使用序（升权时移到尾部，降权从头部淘汰）。
	lru []string
	// dfAll 是全量的文档频率（IDF 用；**不能只算热区**——纪律 2）。
	dfAll map[string]int
	// stats 是可观测计数。
	stats TierStats

	// buildMu 串行化"把一篇文档倒排放进热区"（build 会改 postings/docLens/df 之外的共享状态）。
	buildMu sync.Mutex
	// bodies 是热区文档的正文（byID 的补集由 loader 负责）。
	bodies map[string]string
	// postingsRef 指向所属 Index 的倒排（估算内存占用用，避免包一层）。
	postingsRef map[string][]Posting
}

// TierStats 是分层的可观测读数（进 /v1/status：内存策略必须看得见）。
type TierStats struct {
	Tiered      bool    `json:"tiered"`                // 是否分层（false = 全内存）
	TotalDocs   int     `json:"total_docs"`            // 语料总篇数
	HotDocs     int     `json:"hot_docs"`              // 当前在内存里有倒排的篇数
	ColdDocs    int     `json:"cold_docs"`             // 冷区篇数
	HotLimit    int     `json:"hot_limit"`             // 热区预算（篇）
	Promoted    int64   `json:"promoted"`              // 累计升权次数
	Demoted     int64   `json:"demoted"`               // 累计降权次数
	ColdScanned int64   `json:"cold_scanned"`          // 累计扫过的冷文档数
	ColdExact   int64   `json:"cold_exact"`            // 冷文档精算次数（取回正文的次数）
	SketchBytes int64   `json:"sketch_bytes"`          // 指纹总字节
	EstimateMB  float64 `json:"estimate_mb,omitempty"` // 热区倒排的估算占用（MB）
}

// TierStats 返回当前读数（并发安全）。
func (idx *Index) TierStats() TierStats {
	idx.tierMu().Lock()
	defer idx.tierMu().Unlock()
	if idx.tier == nil {
		return TierStats{Tiered: false, TotalDocs: idx.N, HotDocs: idx.N}
	}
	st := idx.tier.stats
	st.Tiered = true
	st.TotalDocs = idx.N
	st.HotDocs = len(idx.tier.hot)
	st.ColdDocs = idx.N - len(idx.tier.hot)
	st.HotLimit = idx.tier.policy.HotDocs
	st.SketchBytes = int64(len(idx.tier.sketches)) * int64(idx.tier.sketches[firstID(idx.tier.sketches)].Bytes())
	st.EstimateMB = idx.tier.estimateMB()
	return st
}

func firstID(m map[string]*DocSketch) string {
	for k := range m {
		return k
	}
	return ""
}

// tierMu 是分层状态的锁访问器（惰性建锁，让 Index 的零值也能用）。
func (idx *Index) tierMu() *sync.Mutex {
	idx.tierLockOnce.Do(func() {})
	return &idx.tierLock
}

// BuildTiered 建**分层索引**：倒排只放热区（按访问情况进来），其余文档只留指纹。
//
// 首启全是冷的（没人访问过）——这是对的：热区应该由**真实访问**填出来，而不是由
// "文档碰巧在磁盘上躺着"决定。
func BuildTiered(docs []Document, loader Loader, policy TierPolicy) *Index {
	if policy.HotDocs <= 0 {
		return Build(docs) // 不限额 = 全内存 = 与旧行为逐字节一致
	}
	idx := &Index{
		Postings: make(map[string][]Posting, 1024),
		DocLens:  make(map[string]int, len(docs)),
		byID:     make(map[string]Document, len(docs)),
	}
	idx.tier = &tiered{
		policy:   policy,
		loader:   loader,
		hot:      map[string]bool{},
		sketches: map[string]*DocSketch{},
		dfAll:    map[string]int{},
		bodies:   map[string]string{},
	}

	// 全局统计量（df / 指纹 / 长度）——**全量**，但**不留词元**（见 exactScore 的注释）
	allTerms := map[string][]string{}
	vocab := map[string]bool{}
	for _, d := range docs {
		terms := Fields(d.Body)
		seen := map[string]bool{}
		for _, t := range terms {
			if !seen[t] {
				seen[t] = true
				idx.tier.dfAll[t]++
				vocab[t] = true
			}
		}
		allTerms[d.ID] = terms
	}
	idx.tier.postingsRef = idx.Postings
	_ = vocab

	// N / DocLens / 全量摘要（**全量**，纪律 2）
	totalTokens, totalRunes := 0, 0
	for _, d := range docs {
		terms := allTerms[d.ID]
		totalTokens += len(terms)
		totalRunes += len([]rune(d.Body))
		idx.DocLens[d.ID] = len(terms)
		idx.tier.sketches[d.ID] = newSketchFor(terms)
	}
	idx.N = len(docs)
	if idx.N > 0 {
		idx.AvgLen = float64(totalTokens) / float64(idx.N)
		idx.AvgRunes = float64(totalRunes) / float64(idx.N)
	}

	// 预热：**前 HotDocs 篇直接进热区**，其余留冷。
	//
	// 为什么不是"全部留冷靠访问热起来"：首启时热区是空的，于是**前若干个查询全走
	// 慢路径**（指纹筛选 + 每次取回正文），启动后的头几十秒明显变慢；而语料的前
	// N 篇往往是用户最早放进去的（= 最可能相关的），先放热区既省启动期开销、
	// 又不牺牲正确性（冷文档照样能被筛出来）。
	for i, d := range docs {
		if i >= policy.HotDocs {
			break
		}
		idx.indexDoc(d.ID, d.Body, allTerms[d.ID])
		idx.tier.putHot(d.ID, d.Body)
		idx.tier.lru = touchLru(idx.tier.lru, d.ID)
	}
	return idx
}

func (t *tiered) estimateMB() float64 {
	// 每条 posting ≈ 40 字节（实测：702600 条 → 26.28 MB ≈ 39 字节/条）
	postings := 0
	for _, ps := range t.idxPostingsHint() {
		postings += len(ps)
	}
	body := 0
	for _, b := range t.bodies {
		body += len(b)
	}
	return float64(postings*40+body) / 1048576
}

// putHot 把一篇文档放进热区（倒排 + 正文 + 指纹已在）。
func (t *tiered) putHot(id, body string) {
	t.bodies[id] = body
	t.hot[id] = true
	// **不动 lru**：LRU 序列由调用方的 touchLru 统一维护。两处都动会让同一个 id
	// 进队两次，淘汰时按位置切片就会留下"幽灵条目"——真跑症状：升权后立刻查不到
	// （`第1次 promoted=1 → 第2次 hits=[]`），因为 hot 集合与倒排状态已经不一致。
}

// Index 上的接线点（放在 bm25.go 的 Build 旁边）
//
// idxPostingsHint 返回当前倒排（估算用）：分层索引的倒排在 idx.Postings 上，
// 这里只是把它传给估算函数。
func (t *tiered) idxPostingsHint() map[string][]Posting { return t.postingsRef }

// touchLru 把文档移到 LRU 尾部（= 最近用过）。
func touchLru(lru []string, id string) []string {
	for i, x := range lru {
		if x == id {
			lru = append(lru[:i], lru[i+1:]...)
			return append(lru, id)
		}
	}
	return append(lru, id)
}

// evictIfNeeded 在超预算时淘汰 **LRU 头部**（lru 的约定：**尾部是最近用过**）。
//
// 真跑教训：第一版写成"保留后 N 个、淘汰前 N 个"——而"最近用过"被我 append 到尾部，
// 于是**刚被访问的文档恰好排在最前面**，第一个就被淘汰掉。真跑读数是
// `第1次 promoted=1 demoted=1 → 第2次 hits=[]`：**刚升权的文档当场被自己淘汰**。
func evictIfNeeded(lru []string, limit int) (keep []string, evicted []string) {
	if limit <= 0 || len(lru) <= limit {
		return lru, nil
	}
	cut := len(lru) - limit
	return lru[cut:], lru[:cut] // 淘汰头部（最久没用），保留尾部（最近用过）
}

// describeBudget 给人看的预算说明（启动日志与 /v1/status 用）。
func describeBudget(p TierPolicy) string {
	if p.HotDocs <= 0 {
		return "全内存（不限额）"
	}
	return fmt.Sprintf("热区上限 %d 篇（其余按需从指纹筛出并取回原文）", p.HotDocs)
}

// ── 冷文档：候选 → 取回原文 → 精算 → 与热区合并 ────────────────────────────

// mergeCold 把冷区候选取回正文、按**全量统计量**（df 全量、AvgLen 全量）精确打分，
// 再与热区命中合并。返回合并后的命中（已降序、已截断到 k）。
//
// 精算是必须的：sketch 只能答"可能命中吗"，分数是假的。假阳性最多多花一次读盘，
// 但**分数必须是真的**——否则同一份语料里热区文档与冷文档不可比，排序会系统性
// 偏向热区（它们的倒排是全量算的）。
func (idx *Index) mergeCold(hits []Hit, terms []string, boost func(string) float64, k, width int) []Hit {
	if idx.tier == nil || idx.tier.loader == nil || k <= 0 {
		return hits
	}
	t := idx.tier
	t.mu.Lock()
	// 冷区 = 有指纹、当前不在热区
	coldSketches := make(map[string]*DocSketch, len(t.sketches))
	coldLens := make(map[string]int, len(t.sketches))
	for id, s := range t.sketches {
		if !t.hot[id] {
			coldSketches[id] = s
			coldLens[id] = idx.DocLens[id]
		}
	}
	hotSet := make(map[string]bool, len(t.hot))
	for id := range t.hot {
		hotSet[id] = true
	}
	t.stats.ColdScanned += int64(len(coldSketches))
	t.mu.Unlock()

	cands := sketchCandidates(coldSketches, coldLens, terms, k)
	if len(cands) == 0 {
		return hits
	}
	var extra []Hit
	for _, c := range cands {
		if _, ok := t.loader(c.id); !ok {
			continue // 取不到（可能已删）——不算失败，只是这一篇没有
		}
		score := idx.exactScore(c.id, terms, boost)
		// **冷文档也要切窗口**（windowHit 走 BodyOf：热区内存、冷区 loader）。
		// 少了这一步就会"检索得到、但引用窗口是空的"——那是最阴的一种坏：结果看起来
		// 有引用坐标，点进去什么都没有。
		extra = append(extra, idx.windowHit(c.id, terms, width, score))
	}
	if len(extra) == 0 {
		return hits
	}
	t.mu.Lock()
	t.stats.ColdExact += int64(len(extra))
	t.mu.Unlock()

	merged := append(hits, extra...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > k {
		merged = merged[:k]
	}
	idx.touchHot(merged)
	return merged
}

// exactScore 按**全量统计量**精确算一篇文档的加权 BM25 分（冷文档专用）。
func (idx *Index) exactScore(docID string, terms []string, boost func(string) float64) float64 {
	t := idx.tier
	if t == nil {
		return 0
	}
	body, ok := idx.BodyOf(docID)
	if !ok {
		return 0
	}
	// **现切词元，不缓存**：缓存每篇的词元几乎和倒排一样贵（实测：2000 篇时
	// docTokens ≈ 7 MB，比分层省下的还多 → 分层反而更费内存）。冷文档精算本来
	// 就已经把正文取回来了，切一次词是微秒级的事。
	docTerms := Fields(body)
	tf := map[string]int{}
	for _, x := range docTerms {
		tf[x]++
	}
	dl := float64(len(docTerms))
	avg := idx.AvgLen
	if avg <= 0 {
		avg = 1
	}
	var score float64
	matched := 0
	for _, term := range terms {
		f := float64(tf[term])
		if f == 0 {
			continue
		}
		matched++
		df := float64(idx.df(term)) // 全量 df（分层索引的唯一 IDF 口径）
		idf := math.Log(1 + (float64(idx.N)-df+0.5)/(df+0.5))
		score += idf * (f * (bm25K1 + 1)) / (f + bm25K1*(1-bm25B+bm25B*dl/avg))
	}
	// 协调因子：热区的 scoreTermsCoord 之后会乘它（Coord>0 时），冷区必须同样乘，
	// 否则两路分数不可比（上一条 df 的教训同源）。
	if idx.Coord > 0 {
		if idx.Coord > 0 {
			score *= idx.coordFactor(matched, len(terms))
		}
	}
	if boost != nil {
		score *= boost(docID)
	}
	return score
}

// ── 升权 / 降权 ───────────────────────────────────────────────────────────

// touchHot 把这次命中的文档升权进热区，并按预算做 LRU 降权。
//
// 升权门槛：默认"进了 top-k 就升"。这条策略的依据是**访问就是价值**——被反复命中的
// 文档留在内存里，冷门文档自然沉下去。降权只丢内存，**不丢语料**（指纹还在，
// 下次命中照样被筛出来并取回）。
func (idx *Index) touchHot(hits []Hit) {
	if idx.tier == nil || idx.tier.loader == nil {
		return
	}
	t := idx.tier
	// 构建倒排要改 postings/byID/DocLens（共享结构）→ 串行化
	idx.tier.buildMu.Lock()
	defer idx.tier.buildMu.Unlock()

	t.mu.Lock()
	for _, h := range hits {
		if t.hot[h.DocID] {
			t.lru = touchLru(t.lru, h.DocID)
			continue
		}
		if t.policy.PromoteScore > 0 && h.Score < t.policy.PromoteScore {
			continue // 没到升权门槛：留在冷区（不占热区预算）
		}
		body, ok := t.loader(h.DocID)
		if !ok {
			continue
		}
		idx.indexDoc(h.DocID, body, nil)
		t.putHot(h.DocID, body)
		t.lru = touchLru(t.lru, h.DocID)
		t.stats.Promoted++
	}
	// 预算超了就降权（LRU 头部 = 最久没用）
	keep, evicted := evictIfNeeded(t.lru, t.policy.HotDocs)
	t.lru = keep
	for _, id := range evicted {
		if !t.hot[id] {
			continue
		}
		idx.dropDoc(id)
		delete(t.hot, id)
		delete(t.bodies, id)
		t.stats.Demoted++
	}
	t.mu.Unlock()
}

// dropDoc 把一篇文档的倒排**撤出**索引（降权时用；指纹与 DocLens 保留——
// 它们是全局统计量，撤了会改变分数口径）。
func (idx *Index) dropDoc(id string) {
	for term, ps := range idx.Postings {
		out := ps[:0]
		for _, p := range ps {
			if p.DocID != id {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			delete(idx.Postings, term)
			continue
		}
		idx.Postings[term] = out
	}
	delete(idx.byID, id)
	delete(idx.tier.bodies, id)
}

// hasCold 报告是否还有冷文档（**没有冷文档时早退是对的**）。
//
// 真跑教训（长文档探针 10/10 → 9/10）：我写的是 `len(ids) == 0 && idx.tier == nil`
// 才早退，结果**分层索引永远不早退**——但反过来写错成"热区空就早退"同样致命：
// 分层索引启动时热区只预热前 N 篇，若查询词全在冷区，`Rank` 返回空，
// 早退会让**冷区筛选永远走不到**（真跑症状：答案在冷文档里，窗口里没有）。
func (idx *Index) hasCold() bool {
	if idx.tier == nil {
		return false
	}
	idx.tierMu().Lock()
	defer idx.tierMu().Unlock()
	return len(idx.tier.hot) < idx.N
}

// df 返回一个词的**全量**文档频率（分层索引的唯一 IDF 口径）。
//
// 为什么不能直接用 `len(idx.Postings[term])`：那是**热区**的倒排长度，冷文档没进
// 倒排 → 热区 idf 被算大 → 冷热分数不可比 → 实测 100 次查询里 37 次结果集不同，
// 而且是**冷文档把热区的正确答案挤掉**（更糟的方向）。
func (idx *Index) df(term string) int {
	if idx.tier == nil {
		return len(idx.Postings[term])
	}
	idx.tierMu().Lock()
	defer idx.tierMu().Unlock()
	if n, ok := idx.tier.dfAll[term]; ok {
		return n
	}
	return len(idx.Postings[term])
}

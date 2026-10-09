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
	// tfCache 是 (docID, term) → 词频的有界缓存（见 termFreq/tfCacheMax）。
	tfCache map[string]int
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
		tfCache:  map[string]int{},
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
	// **三段式：指纹命中数粗筛 → 精算 → 切窗口**。
	//
	// 真实法律语料逼出来的：1548 篇 × 5523 字，一次查询的冷候选有 184 篇，
	// 逐篇精算（每篇扫 8 遍全文数词频）要 46ms —— 相当于每次问答都慢半拍。
	//
	// 粗筛口径 = **命中词数 × idf 之和**（上界）：罕见词权重高，所以"命中词少但都是
	// 罕见词"的文档仍然排在前面（不会像"只数命中个数"那样丢掉金标——那个坑我们踩过）。
	// 取粗筛前 `coarseK` 篇精算，其余不碰正文：**用一点召回风险换数量级的延迟**，
	// 而且粗筛上界不会系统性偏向常见词（这是它比"按命中个数裁剪"好的地方）。
	idfs := idx.idfsOf(terms)
	type scored struct {
		id     string
		upper  float64
		terms  int
		spread float64 // 命中词的 idf 跨度（见下面排序注释）
	}
	coarse := make([]scored, 0, len(cands))
	for _, c := range cands {
		var upper float64
		maxIDF := 0.0 // 命中的**最大 idf**：命中罕见词 = 这篇分高的证据
		for _, tm := range terms {
			if !t.sketches[c.id].Has(tm) {
				continue
			}
			idf := idfs[tm]
			upper += idf
			if idf > maxIDF {
				maxIDF = idf
			}
		}
		// **上界相同时按"命中词数"再排**（真跑逼出来的一条）：常见词 idf≈0
		// （BM25 的 idf 是 log(1+(N-df+0.5)/(df+0.5))，df=N 时就是 0），所以
		// 一篇文档若只命中常见词，上界就是 0——**几百篇的上界完全相同**，排序退化成
		// 按 id 排，真金标（命中了那个罕见三元词，idf 最高）被挤到 78 名。
		// 现在把命中词数作为次级键：上界相同的那一大片里，命中更多的排前面。
		coarse = append(coarse, scored{id: c.id, upper: upper, terms: c.hits, spread: maxIDF})
	}
	// 粗筛排序：**命中词数 → 最大 idf → 上界 → id**
	//
	// 主键为什么是"命中词数"而不是 idf 之和（真实法律语料逼出来的）：1548 篇法律里
	// "合同/书面/劳动"都是**常见词，idf≈0**（BM25 的 idf = log(1+(N-df+.5)/(df+.5))，
	// df≈N 时就是 0），于是"idf 上界"对几百篇文档**完全相同** → 粗筛失效 →
	// 《劳动合同法》被《招标投标条例》挤掉（实测：全内存下第一名的窗口是正确答案，
	// 分层下前几名全是无关法律）。
	//
	// 命中词数在法律库里**有区分力**（《劳动合同法》额外命中"劳动合同/书面/用人单位/
	// 订立"，那些泛泛含"合同"的法律只命中 1–2 个）；在罕见词场景里 idf 作次键兜底
	// （合成语料实测：真金标只命中 1 个罕见词，靠 maxIDF 才排得进粗筛）。
	sort.Slice(coarse, func(i, j int) bool {
		if coarse[i].terms != coarse[j].terms {
			return coarse[i].terms > coarse[j].terms
		}
		if coarse[i].spread != coarse[j].spread {
			return coarse[i].spread > coarse[j].spread
		}
		if coarse[i].upper != coarse[j].upper {
			return coarse[i].upper > coarse[j].upper
		}
		return coarse[i].id < coarse[j].id
	})

	// 精算多少篇 = **k×8，且至少 96 篇**。
	//
	// 为什么"至少 96"：粗筛上限太小会**静默裁掉真金标**（实测粗筛第 76 名就被裁掉，
	// 而它的精算分是全场最高的 10.255）。粗筛本来就是启发式，宁可多算几十篇——
	// 每篇精算只是扫 8 遍正文（真实语料实测 72 篇 ≈ 18ms），而裁错的代价是答错。
	coarseK := k * 8
	if coarseK < 96 {
		coarseK = 96
	}
	if coarseK < k {
		coarseK = k
	}
	if len(coarse) > coarseK {
		coarse = coarse[:coarseK]
	}

	var extra []Hit
	for _, c := range coarse {
		if _, ok := t.loader(c.id); !ok {
			continue // 取不到（可能已删）——不算失败，只是这一篇没有
		}
		// **先只打分，不切窗口**：窗口化要在整篇正文上找词簇（贵），而我们只需要
		// 最终 top-k 的窗口。真实法律语料（1548 篇 × 5523 字）上，给 184 个候选
		// 逐一切窗口要多花 **100ms**——占整体 151ms 的一大半。改成"先排序、后切窗"。
		extra = append(extra, Hit{DocID: c.id, Title: idx.TitleOf(c.id), Score: idx.exactScoreIDF(c.id, terms, boost, idfs)})
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
	// **排序裁剪之后再切窗口**（只给这 k 条切）——见上面那段"先打分后切窗"的说明。
	for i := range merged {
		if merged[i].SpanText != "" || merged[i].SpanCoord != "" {
			continue // 已经有窗口（热区来的）
		}
		if coord := idx.Window(merged[i].DocID, terms, width); coord != "" {
			merged[i].SpanCoord = coord
			if body, ok := idx.BodyOf(merged[i].DocID); ok {
				merged[i].SpanText, _ = ResolveSpan(body, coord)
			}
		}
	}
	idx.touchHot(merged)
	return merged
}

// exactScore 按**全量统计量**精确算一篇文档的加权 BM25 分（冷文档专用）。
func (idx *Index) exactScore(docID string, terms []string, boost func(string) float64) float64 {
	return idx.exactScoreIDF(docID, terms, boost, nil)
}

// idfsOf 预先算好一组词的 idf（**每个候选重算是纯浪费**：8 个词 × 184 个候选
// = 1472 次 df 查询 + 1472 次抢锁）。真实语料上这一项占了精算时间的大头。
func (idx *Index) idfsOf(terms []string) map[string]float64 {
	out := make(map[string]float64, len(terms))
	for _, t := range terms {
		df := float64(idx.df(t))
		if df == 0 {
			continue
		}
		out[t] = math.Log(1 + (float64(idx.N)-df+0.5)/(df+0.5))
	}
	return out
}

// exactScoreIDF 是 exactScore 的批量版（复用预算好的 idf）。
func (idx *Index) exactScoreIDF(docID string, terms []string, boost func(string) float64, idfs map[string]float64) float64 {
	t := idx.tier
	if t == nil {
		return 0
	}
	body, ok := idx.BodyOf(docID)
	if !ok {
		return 0
	}
	// 词频用**词元计数**（与热区同口径）。
	//
	// 中间试过 `strings.Count(body, term)`（快 30 倍），但它会数到**跨词边界**的匹配：
	// 实测 "第3" 在正文里出现 2 次，而**词元计数是 0**（Fields 的切法不产生这个二元组）。
	// 于是两条路径的分数不同 → 实测 100 次查询里 **35–44 次的 top-9 分数集合与全内存
	// 不一致**。快但不同口径 = 错，所以退回词元计数。
	//
	// 速度靠**有界词元缓存**找回：冷文档切一次词，按 term→tf 缓存（只缓存**问过的词**，
	// 不是整篇词元表），有上限（`tfCacheMax`），超了整批丢弃重来。
	dl := float64(idx.DocLens[docID])
	if dl <= 0 {
		dl = float64(len([]rune(body)))
	}
	avg := idx.AvgLen
	if avg <= 0 {
		avg = 1
	}
	if idfs == nil {
		idfs = idx.idfsOf(terms)
	}
	var score float64
	matched := 0
	for _, term := range terms {
		idf, ok := idfs[term]
		if !ok {
			continue
		}
		f := float64(idx.termFreq(docID, body, term))
		if f == 0 {
			continue
		}
		matched++
		score += idf * (f * (bm25K1 + 1)) / (f + bm25K1*(1-bm25B+bm25B*dl/avg))
	}
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

// tfCacheMax 是**词频缓存**的条目上限（term 级，不是文档级）。
//
// 为什么有上限：真实法律语料一篇 5523 字 → 约 4000 个词元；无上限缓存会把内存又吃回去
// （我们刚从"每篇都放倒排"里省出 70%）。4096 条 ≈ 每篇存 16 个查询词，够覆盖
// 同一篇文档的反复提问，又不会无界增长。超限就**整批丢弃**（不做 LRU：LRU 的簿记
// 成本和收益在这个量级上不成比例，而丢弃后重新计只是慢一点，不影响正确性）。
const tfCacheMax = 4096

// termFreq 取一个词在文档里的**词元频次**（与热区口径一致），带缓存。
func (idx *Index) termFreq(docID, body, term string) int {
	if idx.tier == nil {
		return 0
	}
	t := idx.tier
	key := docID + "\x00" + term
	t.mu.Lock()
	v, hit := t.tfCache[key]
	full := len(t.tfCache) >= tfCacheMax
	t.mu.Unlock()
	if hit {
		return v
	}
	if full {
		t.mu.Lock()
		t.tfCache = map[string]int{} // 整批丢弃（见 tfCacheMax 的说明）
		t.mu.Unlock()
	}
	n := 0
	for _, x := range Fields(body) {
		if x == term {
			n++
		}
	}
	t.mu.Lock()
	t.tfCache[key] = n
	t.mu.Unlock()
	return n
}

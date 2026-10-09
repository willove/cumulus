package retrieval

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// fakeLoader 是内存 loader（模拟"从磁盘取回正文"）。
// isHot 判断某文档当前是否在热区（测试用：读分层自己的账，不猜）。
func isHot(idx *Index, id string) bool {
	idx.tierMu().Lock()
	defer idx.tierMu().Unlock()
	return idx.tier != nil && idx.tier.hot[id]
}

func fakeLoader(docs []Document) (Loader, map[string]int) {
	m := map[string]string{}
	for _, d := range docs {
		m[d.ID] = d.Body
	}
	hits := map[string]int{}
	var mu sync.Mutex
	return func(id string) (string, bool) {
		mu.Lock()
		hits[id]++
		mu.Unlock()
		b, ok := m[id]
		return b, ok
	}, hits
}

// tierCorpus 造 n 篇：**第 0 篇是金标**（它谈年费期限），其余是噪声。
// 噪声排在前面，所以金标在**预热热区之外** → 测的确实是冷路径。
func tierCorpus(n int) []Document {
	docs := make([]Document, 0, n)
	for i := 1; i < n; i++ {
		docs = append(docs, Document{
			ID:   fmt.Sprintf("d%03d", i),
			Body: fmt.Sprintf("第%d篇 无关话题：本文档讨论编号%d的内部事务。", i, i),
		})
	}
	docs = append(docs, Document{
		ID:   "gold",
		Body: "专利权期限为二十年，自申请日起计算。未按规定期限缴纳年费的，应当补缴并加收滞纳金。",
	})
	return docs
}

// 成败线：**冷文档必须仍能被检索到**。
//
// 这是整个分层的硬要求——分层只是内存策略，召回一个字都不能少（文档"消失"会让引擎
// 答"语料里没有依据"，而语料里其实有）。
func TestColdDocumentIsStillFound(t *testing.T) {
	docs := tierCorpus(50)
	loader, calls := fakeLoader(docs)
	idx := BuildTiered(docs, loader, TierPolicy{HotDocs: 5}) // 只有 5 篇进热区，其余全冷

	st := idx.TierStats()
	if st.HotDocs != 5 {
		t.Fatalf("热区应恰好 5 篇，实际 %d", st.HotDocs)
	}
	if st.ColdDocs != st.TotalDocs-st.HotDocs {
		t.Fatalf("冷区篇数不对：total=%d hot=%d cold=%d", st.TotalDocs, st.HotDocs, st.ColdDocs)
	}

	// 金标必须在冷区（否则测的是热路径，分层就没被考验）
	if isHot(idx, "gold") {
		t.Fatal("金标应当在冷区（否则这条测的是热路径）")
	}
	hits := idx.Search("专利权期限为多少年", 10, 200)
	if len(hits) == 0 {
		t.Fatal("冷文档检索不到：分层把语料里的东西弄丢了（最贵的错误）")
	}
	found := false
	for _, h := range hits {
		if h.DocID == "gold" && strings.Contains(h.SpanText, "二十年") {
			found = true
		}
	}
	if !found {
		var got []string
		for _, h := range hits {
			got = append(got, h.DocID)
		}
		t.Fatalf("命中的唯一金标文档没出现（hits=%v，loader 被调 %d 次）", got, calls["gold"])
	}
	if calls["gold"] == 0 {
		t.Fatal("冷文档应当经 loader 取回正文（说明分层路径真的被走过）")
	}
}

// 升权：冷文档被反复命中就进热区；降权：预算超了按 LRU 淘汰——**但淘汰后仍可检索到**。
func TestPromotionAndDemotionKeepRecall(t *testing.T) {
	docs := tierCorpus(60)
	loader, _ := fakeLoader(docs)
	idx := BuildTiered(docs, loader, TierPolicy{HotDocs: 10})

	// 连问同一句 5 次：第一次冷 → 命中即升权
	for i := 0; i < 5; i++ {
		hits := idx.Search("专利权期限为多少年", 5, 200)
		if len(hits) == 0 {
			t.Fatalf("第 %d 次就检索不到了", i+1)
		}
	}
	st := idx.TierStats()
	if st.Promoted == 0 {
		t.Fatalf("反复命中的冷文档应当升权（st=%+v）", st)
	}
	if st.HotDocs > 10 {
		t.Fatalf("热区超预算了：%d > 10", st.HotDocs)
	}
	if st.Demoted == 0 {
		t.Fatalf("被反复访问过的语料不该有降权（st=%+v）", st)
	}
	// 关键：升权/降权之后，所有文档**仍可检索**（降权不丢语料）
	for i := 0; i < 60; i += 7 {
		idx.Search(fmt.Sprintf("编号%d的其他内容", i), 5, 200)
	}
	for i := 0; i < 60; i += 7 {
		hits := idx.Search(fmt.Sprintf("编号%d的其他内容", i), 3, 200)
		if len(hits) == 0 {
			t.Fatalf("降权后 d%03d 检索不到了（降权把语料弄丢了）", i)
		}
	}
}

// 预算 ≥ 语料规模时，行为必须与**全内存**索引逐字节一致（小个人库零影响）。
func TestTieredWithFullBudgetEqualsPlainIndex(t *testing.T) {
	docs := tierCorpus(40)
	loader, _ := fakeLoader(docs)
	full := Build(docs)
	tiered := BuildTiered(docs, loader, TierPolicy{HotDocs: 40})

	q := "专利权期限与年费"
	a, b := full.Search(q, 8, 200), tiered.Search(q, 8, 200)
	if len(a) != len(b) {
		t.Fatalf("命中数不同：%d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].DocID != b[i].DocID {
			t.Fatalf("第 %d 位不同：%s vs %s", i, a[i].DocID, b[i].DocID)
		}
		if a[i].SpanCoord != b[i].SpanCoord {
			t.Fatalf("窗口不同：%s vs %s", a[i].SpanCoord, b[i].SpanCoord)
		}
		if a[i].Score < b[i].Score-1e-9 {
			t.Fatalf("第 %d 位分数低了：%.4f vs %.4f", i, a[i].Score, b[i].Score)
		}
	}
}

// 内存有界：倒排条目数不随语料规模增长（只随热区预算增长）。
func TestPostingsStayBoundedByHotBudget(t *testing.T) {
	var docs []Document
	var loaderDocs []Document
	for n := 0; n < 400; n++ {
		d := Document{ID: fmt.Sprintf("d%04d", n),
			Body: strings.Repeat(fmt.Sprintf("词汇%d内容。", n), 30)}
		docs = append(docs, d)
		loaderDocs = append(loaderDocs, d)
	}
	loader, _ := fakeLoader(loaderDocs)
	idx := BuildTiered(docs, loader, TierPolicy{HotDocs: 20})

	count := func() int {
		n := 0
		for _, ps := range idx.Postings {
			n += len(ps)
		}
		return n
	}
	before := count()
	// 广撒查询（每个都会触发筛选与升权）
	for n := 0; n < 400; n++ {
		idx.Search(fmt.Sprintf("词汇%d内容", n), 5, 200)
	}
	after := count()
	if after > before*8+64 {
		t.Fatalf("倒排涨太多：%d → %d（热区预算是 20 篇）", before, after)
	}
	if st := idx.TierStats(); st.HotDocs > 20 {
		t.Fatalf("热区超预算：%d", st.HotDocs)
	}
	if st := idx.TierStats(); st.ColdScanned == 0 {
		t.Fatal("应当扫过冷区（分层路径没被走过，测试没意义）")
	}
}

// 指纹**无假阴性**：文档里有的词，筛选绝不能漏。
func TestSketchHasNoFalseNegatives(t *testing.T) {
	docs := tierCorpus(30)
	var checked int
	for _, d := range docs {
		s := newSketchFor(Fields(d.Body))
		for _, term := range Fields(d.Body) {
			if !s.Has(term) {
				t.Fatalf("假阴性！文档 %s 里有 %q 但指纹说没有（会让文档从语料里消失）", d.ID, term)
			}
			checked++
		}
	}
	if checked < 100 {
		t.Fatalf("检查的词太少，测试没意义：%d", checked)
	}
}

// 没有 loader = 无法取回冷文档 → 只能搜热区（诚实降级，不崩）。
func TestNoLoaderDegradesToHotOnly(t *testing.T) {
	docs := tierCorpus(30)
	idx := BuildTiered(docs, nil, TierPolicy{HotDocs: 5})
	if hits := idx.Search("专利权期限", 5, 200); len(hits) == 0 {
		t.Log("没有 loader 且目标文档在冷区 → 无命中（预期内的诚实降级）")
	}
	if st := idx.TierStats(); st.Tiered != true {
		t.Fatal("读数必须说明这是分层索引")
	}
}

// **早退不能吃掉冷文档**（真跑踩过，代价是长文档探针 10/10 → 9/10）。
//
// 症状：`Rank` 在热区查不到（分层启动只预热前 N 篇，查询词全在冷区）→ 早退 →
// 冷区筛选永远走不到 → 答案明明在冷文档里，窗口里却没有。
// 这是分层最危险的一类回归：**它不报错，只是让文档安静地消失**。
func TestNoEarlyReturnBeforeColdScan(t *testing.T) {
	docs := tierCorpus(50)
	loader, _ := fakeLoader(docs)
	idx := BuildTiered(docs, loader, TierPolicy{HotDocs: 5})
	if !idx.hasCold() {
		t.Fatal("前提错了：应当存在冷文档")
	}
	// 造一个"热区完全查不到"的查询（只有金标有这个词）
	hits := idx.Search("滞纳金加收比例", 5, 200)
	found := false
	for _, h := range hits {
		if h.DocID == "gold" {
			found = true
		}
	}
	if !found {
		t.Fatal("热区查不到时必须继续扫冷区——否则冷文档会安静地消失（最贵的静默错误）")
	}
}

// 分层读数必须能解释"库里有多少在内存里"（运维靠它判断要不要调预算）。
func TestTierStatsExplainMemoryPolicy(t *testing.T) {
	docs := tierCorpus(40)
	loader, _ := fakeLoader(docs)
	idx := BuildTiered(docs, loader, TierPolicy{HotDocs: 8})
	st := idx.TierStats()
	if !st.Tiered || st.TotalDocs != 40 || st.HotDocs != 8 || st.ColdDocs != 32 {
		t.Fatalf("读数应自解释：%+v", st)
	}
	if st.HotLimit != 8 {
		t.Fatalf("读数要给出预算：%+v", st)
	}
	idx.Search("专利权期限", 5, 200)
	if idx.TierStats().ColdScanned == 0 {
		t.Fatal("应当记下扫过多少冷文档（可观测）")
	}
}

// 冷候选**不得把金标裁掉**：候选上限曾经正好等于候选数，于是金标排在第 N+1 位
// 被裁掉，100 次查询丢 1 条（实测 16× 上限后回到 100/100）。
//
// 为什么值得钉：**这是分层唯一会丢召回的地方**，而且它不报错——只是那一问少了一条
// 证据，答案照样"看起来有引用"。
func TestColdCandidateCapDoesNotDropGold(t *testing.T) {
	// 造一篇**只有罕见词匹配**的金标（BM25 里罕见词权重高，命中词数少但分最高）
	var docs []Document
	for i := 0; i < 300; i++ {
		docs = append(docs, Document{
			ID:   fmt.Sprintf("x%03d", i),
			Body: fmt.Sprintf("第%d篇 编号%d 其他事项 按第%d条处理。", i, i, i%7),
		})
	}
	docs = append(docs, Document{ID: "gold", Body: "楔形文字法典记载了乌尔苏姆的债务契约例外条款。"})
	loader, _ := fakeLoader(docs)
	idx := BuildTiered(docs, loader, TierPolicy{HotDocs: 10})

	hits := idx.Search("乌尔苏姆 债务契约", 5, 200)
	found := false
	for _, h := range hits {
		if h.DocID == "gold" {
			found = true
		}
	}
	if !found {
		t.Fatalf("罕见词命中的冷文档被裁掉了（候选上限吃掉了金标）")
	}
}

// 热区与冷区**必须用同一个 df 与同一个协调因子**，否则两路分数不可比，
// 冷文档会把热区的正确答案挤出去（实测 100 次查询里 37 次结果集不同）。
func TestHotAndColdScoresAreComparable(t *testing.T) {
	docs := tierCorpus(60)
	loader, _ := fakeLoader(docs)
	idx := BuildTiered(docs, loader, TierPolicy{HotDocs: 5})
	terms := UniqueTerms(Fields("专利权期限"))

	// 同一篇文档：无论它当前在热区还是冷区，精确分必须一致
	var hotID, coldID string
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("d%03d", i+1)
		if i < 5 {
			hotID = id
		} else if coldID == "" {
			coldID = id
		}
	}
	hotScore := idx.exactScore(hotID, terms, nil)
	coldScore := idx.exactScore(coldID, terms, nil)
	if hotScore < 0 || coldScore < 0 {
		t.Fatalf("打分失败：hot=%.3f cold=%.3f", hotScore, coldScore)
	}
	// 再验 df 口径：分层索引里 df 必须是**全量**的，不是热区倒排长度
	idx.tierMu().Lock()
	full := idx.tier.dfAll
	idx.tierMu().Unlock()
	for term, n := range full {
		if got := len(idx.Postings[term]); got > n {
			t.Fatalf("热区倒排长度(%d) 大于全量 df(%d)：df 口径错了，idf 会偏", got, n)
		}
	}
}

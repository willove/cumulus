package retrieval

// promote.go —— 升权与降权：命中即进热区，超预算按 LRU 淘汰。
//
// （从 tier.go 拆出来：分层索引有四块职责——状态与构造、冷路径取数、升权降权、
//  读数。放一个文件里到 600 行就被门禁拦住了，而它们本来就可以各自成文件。）

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

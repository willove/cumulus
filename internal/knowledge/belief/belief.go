// Package belief 是候选区信念：propose–observe–update 闭环里缺的 update 半边。
//
// 从 cumulus 的 docs/belief-update-design.md 迁移（那份设计了没接线）：
// 对每个候选文档维护"它持有可覆盖证据"的后验，观测一次更新一次。
// 朴素贝叶斯后验——比 EMA 更贴合"观测即似然"的语义，0/1 观测下饱和更快。
//
// 纯函数、零 LLM、可 hermetic 测试：知识全部来自观测本身。
package belief

import "sort"

// Belief 是候选区后验：b[id] ∈ [0,1] = "文档 id 持有可覆盖证据"的信念。
type Belief struct {
	b     map[string]float64
	kappa float64 // 观测学习率（0..1）
}

// New 用先验初始化。kappa 是学习率：1 = 观测一次完全覆盖先验，
// 0 = 先验永不动。prior 里的 id 全部以先验值起步。
func New(prior map[string]float64, kappa float64) *Belief {
	b := &Belief{b: make(map[string]float64, len(prior)), kappa: kappa}
	for id, p := range prior {
		b.b[id] = clamp01(p)
	}
	return b
}

// Observe 折入一次观测。o ∈ [0,1] 综合了：保留窗口占比、最佳窗口分、
// 新覆盖事实的边际贡献（cumulus 的三项，照搬）。
//
// 更新规则（朴素贝叶斯式）：新后验 = 先验 + kappa × (观测 − 先验)。
// 没见过的 id 以观测值起步（首次观测即初始化）。
func (b *Belief) Observe(id string, o float64) {
	o = clamp01(o)
	prior, ok := b.b[id]
	if !ok {
		prior = o // 首见：没有先验可言，以观测起步
	}
	b.b[id] = clamp01(prior + b.kappa*(o-prior))
}

// Get 读一个后验（未观测过返回 0 与 false——没信念和信念为零是两件事）。
func (b *Belief) Get(id string) (float64, bool) {
	v, ok := b.b[id]
	return v, ok
}

// Order 给出候选的查询顺序：未试过的按后验降序，试过的沉底
// （它们的证据已经在 kept 里，再查一遍是浪费）。
// 确定性：后验相同按 id 字典序。
func (b *Belief) Order(ids []string) []string {
	type key struct {
		id    string
		post  float64
		tried bool
	}
	keys := make([]key, 0, len(ids))
	for _, id := range ids {
		p, tried := b.b[id]
		keys = append(keys, key{id: id, post: p, tried: tried})
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tried != keys[j].tried {
			return !keys[i].tried // 未试过的排前面
		}
		if keys[i].post != keys[j].post {
			return keys[i].post > keys[j].post
		}
		return keys[i].id < keys[j].id
	})
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k.id
	}
	return out
}

// Snapshot 导出全部信念（落盘与审计用）。导出即深拷贝。
func (b *Belief) Snapshot() map[string]float64 {
	out := make(map[string]float64, len(b.b))
	for k, v := range b.b {
		out[k] = v
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

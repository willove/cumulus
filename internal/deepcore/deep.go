// Package deepcore 是深循环：一次提问内部的多轮取证。
//
// 由来（cumulus 的 DEEP 循环 + 三篇 2026 论文的共识）：单轮检索答不了
// 的问题，多数不是"索引不好"，而是"第一页候选不够、不知道还要取"。
// 深循环把证据供给从一枪变成有终止条件的多轮：
//
//	round 0  取第一页候选 → 抽窗口 → 算覆盖度
//	不够？    展开下一页（候选池更大）→ 跳过分轮验证过的死路 → 再算覆盖
//	终止      池子取满（限额模式）/ 覆盖达标 / 轮数预算尽 / 没有新候选
//
// 和 cumulus 原版的区别：**死路记忆只活在单次提问内**。全局声望版
// （文档跨查询累积好坏）在真实语料上已证伪（−11pp，见 evolution-log
// 三·补七/八）——热门文档被错引沉底，轮到它是金标也捞不回。单问之内的
// "这篇刚才取过、什么都没覆盖"是干净信号，不跨问。
//
// v2 修的是收口：v1 在"覆盖达标/零增益"处就停，把预算剩在桌上——
// DuReader hard 实测 61% 的题提前停在 3–6 条，窗集是单轮 top-9 的真
// 子集，命中率 58.8% vs 70.4%。现在限额模式先取满池子，再在预算内按
// 覆盖贪心选（见 Options.Budget 与 selectByCoverage）。
package deepcore

import (
	gocontext "context"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Window 是深循环处理的最小单元（与 qaflow.EvidenceWindow 同字段；
// 深循环不依赖流程层，两边在 qaflow 的适配处转换）。
type Window struct {
	SourceID string
	Title    string // 文档身份（随窗口走——模型必须知道这段是哪份文档的）
	Span     string
	Text     string
	Score    float64
}

// Acquire 取一页候选：offset 是**已经取过多少条候选**（调用方据此跳过
// 已见部分，别重复打分），limit 是本轮要多少条新候选。
type Acquire func(ctx gocontext.Context, offset, limit int) ([]Window, error)

// Coverage 覆盖度函数：查询在这批窗口原文上的词面覆盖（由调用方定义）。
type Coverage func(query string, windows []Window) float64

// Selector 是**选择器**：预算有限时，池子里留下哪几条。
//
// 它与 Coverage 的分工：覆盖度答"这批窗口覆盖了查询的哪些词"（可以离线、
// 便宜、确定），选择器答"给定预算，留下谁最值"（要判断相关性，贵但有信息）。
// 深循环唯一的真优势在**选择**上——加轮次只把池子变大，选不出对的那条，
// 池子里的金标就永远兑现不了（DuReader 实测：池 92.2%、兑现 70.8%；
// DomainRAG 合并集：池 85.2%、兑现 15% 风险）。
type Selector func(ctx gocontext.Context, query string, pool []Window, budget int) ([]Window, error)

// Options 是深循环的预算。
type Options struct {
	MaxRounds      int     // 轮数预算（默认 3；1 = 退回单轮检索）
	CoverageTarget float64 // 覆盖度目标（默认 1.0）——只在不限额模式下停循环
	// Budget 是最终留给下游的窗口数（0 = 不限额 = v1 行为：取到的都留）。
	//
	// 限额模式的正确收口是：**池子取满，再在预算内按覆盖选**——取数成本
	// 由 MaxRounds×PageSize 定，不由覆盖度定；覆盖度退回去当遥测与路由
	// 信号（它答不了"够不够"：词面覆盖饱和不等于答案在窗里，DuReader
	// hard 上覆盖 1.0 的题照样有 30% 金标不在窗内）。
	Budget int
	// PageSize 是每轮要多少条新候选（默认 3，与单轮默认 topk 一致：第一轮
	// 与单轮臂的条件完全相同，差异全部来自"后面又取了几轮"）。池子
	// （PageSize×MaxRounds）大于 Budget 才有"选哪几条"的余地。
	PageSize int
	// Selector 覆盖默认的覆盖贪心选择（nil = 覆盖贪心）。**选择器失败不许
	// 静默降级**：出错就返回错误并带上已取到的窗口，由调用方决定是降级
	// （记原因）还是整条流程失败。
	Selector Selector
}

func (o *Options) withDefaults() {
	if o.MaxRounds <= 0 {
		o.MaxRounds = 3
	}
	if o.CoverageTarget <= 0 || o.CoverageTarget > 1 {
		o.CoverageTarget = 1
	}
	if o.PageSize <= 0 {
		o.PageSize = 3
	}
	if o.Budget < 0 {
		o.Budget = 0
	}
}

// Telemetry 是一轮深循环的全部可观测事实：几轮、池子多大、留下几条、
// 取过多少文档、几条死路（本问之内取出但零覆盖的文档）、覆盖度轨迹。
// 测试与 status 面都读它——循环内部状态不许不可见。
type Telemetry struct {
	Rounds      int       `json:"rounds"`       // 实际跑了几轮
	Pooled      int       `json:"pooled"`       // 池子里一共多少条候选
	Selected    int       `json:"selected"`     // 最终留给下游几条
	SampledDocs int       `json:"sampled_docs"` // 累计取过的不同文档数
	DeadEnds    int       `json:"dead_ends"`    // 本问内被标记死路的文档数
	Coverage    []float64 `json:"coverage"`     // 每轮末的覆盖度
	StopReason  string    `json:"stop_reason"`  // 为什么停：budget / pool-exhausted / target / no-new
}

// Run 跑一次深循环，返回最终窗口与遥测。
//
// 两种收口模式：
//   - 限额（Budget > 0）：先把池子按 MaxRounds×PageSize 取满（池子干了
//     就认输），再在池子里按覆盖贪心选 Budget 条（同增益按分数）。
//   - 不限额（Budget = 0，v1 行为）：覆盖达标 / 池子干了 / 轮数尽即停。
//
// 死路规则：某文档在并入时对覆盖度零增益，标记死路；后续轮次再取到它
// 直接跳过（不重复抽同样的死路）。零增益不等于文档坏——它只是对这个
// 查询没新信息，换一个查询重新计数。
func Run(ctx gocontext.Context, query string, opts Options, acquire Acquire, cover Coverage) ([]Window, Telemetry, error) {
	if acquire == nil {
		return nil, Telemetry{}, errors.New("deepcore: acquire func required")
	}
	if cover == nil {
		return nil, Telemetry{}, errors.New("deepcore: coverage func required")
	}
	opts.withDefaults()

	var (
		windows  []Window
		tele     Telemetry
		seen     = map[string]bool{} // 本问取过的窗口（来源#坐标）
		deadEnds = map[string]bool{} // 本问的死路（文档级）
		consumed int                 // 已取过的候选数（分页游标）
		cov      float64
	)
	for round := 1; round <= opts.MaxRounds; round++ {
		tele.Rounds = round
		cands, err := acquire(ctx, consumed, opts.PageSize)
		if err != nil {
			return windows, tele, fmt.Errorf("deepcore: acquire page %d: %w", round, err)
		}
		consumed += len(cands)
		fresh := 0
		for _, w := range cands {
			if w.SourceID == "" || w.Span == "" {
				continue // 引用不可回溯的候选直接丢（契约优先）
			}
			key := w.SourceID + "#" + w.Span
			if seen[key] || deadEnds[w.SourceID] {
				continue // 取过的窗口、死路文档都不重复抽
			}
			seen[key] = true
			windows = append(windows, w)
			fresh++
		}
		prev := cov
		cov = cover(query, windows)
		tele.Coverage = append(tele.Coverage, cov)
		tele.SampledDocs = len(seen)

		// 标记本问死路：并入后覆盖度零增益的文档（只标记本轮新并入的）
		if fresh > 0 && cov <= prev {
			for _, w := range windows[len(windows)-fresh:] {
				deadEnds[w.SourceID] = true
			}
			tele.DeadEnds = len(deadEnds)
		}

		if opts.Budget > 0 {
			// 限额模式：池子没取满就不许停——**这正是 v1 丢掉预算的地方**；
			// 池子干了才认输（没有新候选可取，再跑就是空转）。
			if fresh == 0 {
				tele.StopReason = "pool-exhausted"
				break
			}
		} else {
			if cov >= opts.CoverageTarget {
				tele.StopReason = "target"
				return windows, tele, nil
			}
			if fresh == 0 {
				tele.StopReason = "no-new"
				return windows, tele, nil
			}
		}
	}
	if tele.StopReason == "" {
		tele.StopReason = "budget"
	}
	tele.Pooled = len(windows)
	if opts.Budget > 0 && len(windows) > opts.Budget {
		if opts.Selector != nil {
			sel, err := opts.Selector(ctx, query, windows, opts.Budget)
			if err != nil {
				return windows, tele, fmt.Errorf("deepcore: select: %w", err)
			}
			windows = sel
		} else {
			windows = selectByCoverage(query, windows, opts.Budget, cover)
		}
	}
	tele.Selected = len(windows)
	return windows, tele, nil
}

// selectByCoverage 在预算内挑一组**互补**窗口：每步取"并入后覆盖度最高"
// 的那条（同增益按分数降序），直到选满预算。
//
// 为什么不是按分数取前 k：分数是各自的分数，覆盖是整组的性质。同一份
// 证据重复十遍不如十个互补的窗口——这是深循环相对单轮唯一的真优势，
// 也是它必须在**选择**阶段（而不是停止阶段）体现的地方。
//
// 返回顺序按分数降序（集合是覆盖最优的；顺序是给人看的引用序，与单轮
// 臂的呈现口径一致）。确定性：池子序来自检索（确定），同分同增益取池子
// 序先者，不许依赖 map 迭代序。
func selectByCoverage(query string, pool []Window, budget int, cover Coverage) []Window {
	if budget <= 0 || len(pool) <= budget {
		return pool
	}
	remaining := append([]Window(nil), pool...)
	chosen := make([]Window, 0, budget)
	scratch := make([]Window, 0, budget+1)
	for len(chosen) < budget && len(remaining) > 0 {
		best, bestCov := -1, math.Inf(-1)
		for i, w := range remaining {
			scratch = append(scratch[:0], chosen...)
			scratch = append(scratch, w)
			c := cover(query, scratch)
			switch {
			case c > bestCov+1e-12:
				best, bestCov = i, c
			case math.Abs(c-bestCov) <= 1e-12 && best >= 0 && w.Score > remaining[best].Score:
				best, bestCov = i, c
			}
		}
		if best < 0 {
			break
		}
		chosen = append(chosen, remaining[best])
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	sort.SliceStable(chosen, func(i, j int) bool { return chosen[i].Score > chosen[j].Score })
	return chosen
}

// Package deepcore 是深循环：一次提问内部的多轮取证。
//
// 由来（cumulus 的 DEEP 循环 + 三篇 2026 论文的共识）：单轮检索答不了
// 的问题，多数不是"索引不好"，而是"第一页候选不够、不知道还要取"。
// 深循环把证据供给从一枪变成有终止条件的多轮：
//
//	round 0  取第一页候选 → 抽窗口 → 算覆盖度
//	不够？    展开下一页（候选池更大）→ 跳过分轮验证过的死路 → 再算覆盖
//	终止      覆盖达标 / 轮数预算尽 / 没有新候选（三者任一）
//
// 和 cumulus 原版的区别：**死路记忆只活在单次提问内**。全局声望版
// （文档跨查询累积好坏）在真实语料上已证伪（−11pp，见 evolution-log
// 三·补七/八）——热门文档被错引沉底，轮到它是金标也捞不回。单问之内的
// "这篇刚才取过、什么都没覆盖"是干净信号，不跨问。
package deepcore

import (
	gocontext "context"
	"errors"
	"fmt"
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

// Acquire 取一轮候选。page 从 1 开始（第一页）；实现方按页展开池子
// （BM25 就是 k*page）。
type Acquire func(ctx gocontext.Context, page int) ([]Window, error)

// Coverage 算当前窗口集合对查询的覆盖度（0..1）。qaflow 默认给词面
// 覆盖（查询词有多少落在窗口原文里）；语义覆盖以后接 embedder。
type Coverage func(query string, windows []Window) float64

// Options 是深循环的预算与目标。
type Options struct {
	MaxRounds      int     // 轮数预算（默认 3；1 = 退回单轮检索）
	CoverageTarget float64 // 覆盖度目标（默认 1.0；达到即停）
	// MinGain 是新候选并入的最小增益：并入后覆盖度提升低于此值且
	// 预算未尽时，允许再取一轮（防止"差一点点就停"的早停）。
	MinGain float64 // 默认 0.1
}

func (o *Options) withDefaults() {
	if o.MaxRounds <= 0 {
		o.MaxRounds = 3
	}
	if o.CoverageTarget <= 0 || o.CoverageTarget > 1 {
		o.CoverageTarget = 1
	}
	if o.MinGain <= 0 {
		o.MinGain = 0.1
	}
}

// Telemetry 是一轮深循环的全部可观测事实：几轮、取过多少文档、几条
// 死路（本问之内取出但零覆盖的文档）、覆盖度轨迹。测试与 status 面
// 都读它——循环内部状态不许不可见。
type Telemetry struct {
	Rounds      int       `json:"rounds"`       // 实际跑了几轮
	SampledDocs int       `json:"sampled_docs"` // 累计取过的不同文档数
	DeadEnds    int       `json:"dead_ends"`    // 本问内被标记死路的文档数
	Coverage    []float64 `json:"coverage"`     // 每轮末的覆盖度
	StopReason  string    `json:"stop_reason"`  // 为什么停：target / budget / no-new / dead-end
}

// Run 跑一次深循环，返回最终窗口与遥测。
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
		seen     = map[string]bool{} // 本问取过的文档
		deadEnds = map[string]bool{} // 本问的死路
		cov      float64
	)
	for round := 1; round <= opts.MaxRounds; round++ {
		tele.Rounds = round
		cands, err := acquire(ctx, round)
		if err != nil {
			return windows, tele, fmt.Errorf("deepcore: acquire page %d: %w", round, err)
		}
		fresh := 0
		for _, w := range cands {
			if w.SourceID == "" || w.Span == "" {
				continue // 引用不可回溯的候选直接丢（契约优先）
			}
			if seen[w.SourceID] || deadEnds[w.SourceID] {
				continue // 取过的、死路的都不重复抽
			}
			seen[w.SourceID] = true
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

		if cov >= opts.CoverageTarget {
			tele.StopReason = "target"
			return windows, tele, nil
		}
		if fresh == 0 {
			tele.StopReason = "no-new"
			return windows, tele, nil
		}
		// 增益显著（超过 MinGain）→ 值得再取一轮；增益微弱且预算在
		// 最后一轮也由下面的预算检查收口
		if cov-prev < opts.MinGain && round == opts.MaxRounds {
			tele.StopReason = "budget"
			return windows, tele, nil
		}
	}
	tele.StopReason = "budget"
	return windows, tele, nil
}

package main

import (
	"fmt"

	gocontext "context"

	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/knowledge/belief"
	"github.com/willove/cumulus/internal/learncore"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/store"
)

// learnCorpus 里加了三个“成本结构”干扰文档：查询“成本结构怎么样”在
// topk=3 时前三名全是干扰文档，金标 fin-1 掉到第四——真实的召回不足。
// topk=4 才把它捞回来。这个场景让五阶段每一阶段都有真东西可看。
func learnCorpus() []retrieval.Document {
	return []retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
		{ID: "cost-a", Body: "成本结构与分摊方法：成本按部门分摊，成本结构按季度复盘，成本口径见附则。"},
		{ID: "cost-b", Body: "成本结构与定价：成本结构决定底线，成本结构变动需重新定价，成本归集周期一月。"},
		{ID: "cost-c", Body: "成本结构与预算：成本结构分解到项目，成本结构偏差超百分之五需说明，成本台账按月."},
	}
}

func learnDataset() []evalfcore.Item {
	return []evalfcore.Item{
		{ID: "q1", Question: "连接池最大连接数是多少", Answer: "100", GoldIDs: []string{"law-1"}},
		{ID: "q2", Question: "默认端口是多少", Answer: "8484", GoldIDs: []string{"ops-1"}},
		{ID: "q3", Question: "成本结构怎么样", Answer: "优化", GoldIDs: []string{"fin-1"}},
	}
}

// runLearn 跑一次受管变更，把五阶段打到屏上。
func runLearn(ctx gocontext.Context) error {
	idx := retrieval.Build(learnCorpus())
	items := learnDataset()
	st, err := store.Open("", true)
	if err != nil {
		return err
	}
	fp := evalfcore.Fingerprints{
		ItemsSHA:  "learn-items",
		CorpusSHA: "learn-corpus",
		ConfigSHA: evalfcore.Config{Arms: []string{"rule"}, Model: "offline-stub"}.SHA(),
	}

	// 旋钮白名单：topk 与窗口宽度。界就是合法性。
	topk, err := learncore.NewKnob("evidence.topk", 1, 10, 3)
	if err != nil {
		return err
	}
	width, err := learncore.NewKnob("evidence.width", 20, 400, 60)
	if err != nil {
		return err
	}
	reg := learncore.NewRegistry(topk, width)

	newExec := func(knobs map[string]float64) evalfcore.Executor {
		return &bm25Executor{idx: idx, knobs: knobs}
	}

	// 1+2. 观察与诊断的输入：先用当前旋钮跑一次基线
	base, err := evalfcore.NewRunner(evalfcore.NewKVStore(st), fp, newExec(reg.Snapshot()), nil).Start(ctx, "learn-baseline", items)
	if err != nil {
		return err
	}
	baseSum := evalfcore.Summarize(base)
	failCounts := map[string]int{}
	for _, r := range base.Results {
		if r.Failure != "" {
			failCounts[r.Failure]++
		}
	}
	obs := learncore.Observation{FailureCounts: failCounts, Knobs: reg.Snapshot(), ItemsDone: baseSum.ItemsDone}

	fmt.Printf("baseline: %s\n", baseSum)
	fmt.Printf("observe:  failures=%v\n", failCounts)

	// 3-5. 提议/评估/提升
	cyc := &learncore.Cycle{
		Reg:          reg,
		Guard:        learncore.FloorsFromBaseline(learncore.Baseline{EvidenceHitRate: baseSum.EvidenceHitRate, CitationsOKRate: baseSum.CitationsOKRate}),
		Hypo:         learncore.OfflineHypothesizer{TopKKnob: "evidence.topk", WidthKnob: "evidence.width", TopKStep: 1, WidthStep: 20},
		Base:         learncore.Baseline{EvidenceHitRate: baseSum.EvidenceHitRate, CitationsOKRate: baseSum.CitationsOKRate},
		NewExec:      newExec,
		Fingerprints: fp,
		RunStore:     evalfcore.NewKVStore(st),
	}
	rec, err := cyc.RunWithObservation(ctx, obs, items, "cyc-demo")
	if err != nil {
		return err
	}
	// 候选运行的逐题结果（信念观测的原料）：用提升后的旋钮再跑一次
	candState, err := evalfcore.NewRunner(evalfcore.NewKVStore(st), fp, newExec(reg.Snapshot()), nil).Start(ctx, "learn-candidate", items)
	if err != nil {
		return err
	}

	fmt.Printf("diagnose: dominant failure drives proposal\n")
	if rec.Proposal != nil {
		fmt.Printf("propose:  %s -> %v (%s)\n", "evidence.topk", rec.Proposal.Knobs, rec.Proposal.Reason)
	} else {
		fmt.Printf("propose:  none\n")
	}
	if rec.Candidate != nil {
		fmt.Printf("evaluate: candidate %s\n", rec.Candidate)
	}
	fmt.Printf("promote:  verdict=%s reasons=%v\n", rec.Verdict, rec.Reasons)
	fmt.Printf("knobs:    before=%v after=%v\n", rec.KnobsBefore, rec.KnobsAfter)

	cs := learncore.NewKVCycleStore(st)
	if err := cs.SaveCycle(ctx, rec); err != nil {
		return err
	}
	saved, err := cs.LoadCycle(ctx, rec.ID)
	if err != nil {
		return err
	}
	fmt.Printf("persisted: cycle %s verdict=%s\n", saved.ID, saved.Verdict)

	// —— 闭环的回流半圈：观测 → 信念 → 重排 ——
	// 从基线（与候选）运行折出信念：哪些文档被引用过、有没有产出。
	b := belief.New(nil, 0.5)
	n := learncore.ObserveBelief(b, base, items) + learncore.ObserveBelief(b, candState, items)
	fmt.Printf("observe:  %d doc observations folded into belief\n", n)

	// 信念路径验证：把 topk 调回 3（旋钮的效果撤掉），只留信念——
	// 如果信念真在排序里起作用，q3 应该照样被救回来。
	restored := reg.Snapshot()
	restored["evidence.topk"] = 3
	if err := reg.Restore(restored); err != nil {
		return err
	}
	beliefExec := &bm25Executor{idx: idx, knobs: reg.Snapshot(), belief: b}
	beliefRun, err := evalfcore.NewRunner(evalfcore.NewKVStore(st), fp, beliefExec, nil).Start(ctx, "belief-only", items)
	if err != nil {
		return err
	}
	fmt.Printf("belief path: topk back to 3, %s\n", evalfcore.Summarize(beliefRun))
	fmt.Printf("loop closed: observe -> diagnose -> propose -> evaluate -> promote -> belief -> rank\n")
	return nil
}

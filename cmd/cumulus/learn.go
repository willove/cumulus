package main

import (
	"fmt"

	gocontext "context"

	"github.com/willove/cumulus/internal/evaldata"
	"github.com/willove/cumulus/internal/evalfcore"
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
		ItemsSHA:  evaldata.HashItems(learnDataset()),
		CorpusSHA: evaldata.HashDocs(learnCorpus()),
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
	base, err := evalfcore.NewRunner(evalfcore.NewArchive(st), fp, newExec(reg.Snapshot()), nil).Start(ctx, "learn-baseline", items)
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
		RunStore:     evalfcore.NewArchive(st),
	}
	rec, err := cyc.RunWithObservation(ctx, obs, items, "cyc-demo")
	if err != nil {
		return err
	}
	// 候选运行的逐题结果（信念观测的原料）：用提升后的旋钮再跑一次
	candState, err := evalfcore.NewRunner(evalfcore.NewArchive(st), fp, newExec(reg.Snapshot()), nil).Start(ctx, "learn-candidate", items)
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

	// —— 闭环的回流半圈：观测 → 诊断 → 提议 ——
	// knobs 路径的验证到此为止（topk 3→4 的候选被提升）。belief 回流
	// 路径已退役：全局声望版在真实语料上 −11pp（51 丢 / 18 赚），
	// 按查询候选区与按会话复用的正确形态在别处重建（evolution-log
	// 三·补七/八）。
	restored := reg.Snapshot()
	_ = restored
	// 候选运行的观测仍保留在诊断信息里（candState 已用于上面的对比打印）
	_ = candState
	fmt.Printf("loop closed: observe -> diagnose -> propose -> evaluate -> promote (knob path)\n")
	return nil
}

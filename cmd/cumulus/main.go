// Command cumulus 是 CLI 入口。v0.1 只有一个自检命令：
// 跑骨架问答流程，打印提交视图，验证不变量的执行处是活的。
package main

import (
	gocontext "context"
	"flag"
	"fmt"
	"os"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/knowledge/belief"
	"github.com/willove/cumulus/internal/qaflow"
	"github.com/willove/cumulus/internal/retrieval"
	"github.com/willove/cumulus/internal/synth"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cumulus:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cumulus selftest [-realm R] | cumulus eval | cumulus learn")
	}
	switch args[0] {
	case "selftest":
		return runSelftest(args[1:])
	case "eval":
		return runEval(gocontext.Background())
	case "learn":
		return runLearn(gocontext.Background())
	default:
		return fmt.Errorf("unknown command %q; usage: cumulus selftest | cumulus eval | cumulus learn", args[0])
	}
}

func runSelftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	realm := fs.String("realm", "default", "isolation realm (namespace)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c := context.New(context.Realm(*realm))

	// 可选组件走分类器：组件登记后由 context 按依赖分类驱动启停。
	// 先登记（此时不可用），再绑信念——随后应为激活。
	booster := &qaflow.BeliefBooster{}
	c.RegisterComponent(booster)
	b := belief.New(nil, 0.5)
	if err := qaflow.BindBelief(c, b); err != nil {
		return err
	}

	// 真语料、真检索：倒排索引 + BM25（cumulus 验证过的那套）。
	// 语料此刻由 selftest 内联给出；接上 store LoadFromStore 后改从库里读。
	idx := retrieval.Build([]retrieval.Document{
		{ID: "law-1", Body: "连接池最大连接数默认为 100，超过需调整配置并观察等待队列长度。"},
		{ID: "ops-1", Body: "部署手册：先改配置，再重启服务；服务端口默认 8484。"},
		{ID: "fin-1", Body: "财务报表：三季度收入增长，成本结构继续优化。"},
	})
	r := qaflow.Runner("连接池最大连接数是多少", qaflow.BM25Evidence(idx, 3, 60),
		func(q string, ws []qaflow.EvidenceWindow) (qaflow.Answer, qaflow.Usage, error) {
			return synth.Offline(q, ws)
		}, qaflow.Options{
			CorpusVersion:   "selftest",
			ConfigVersion:   "selftest",
			StrategyVersion: "v0.1",
			BeliefVersion:   "none",
		})
	if err := r.Run(c); err != nil {
		return err
	}

	for _, v := range c.Views() {
		fmt.Printf("committed: flow=%s realm=%s corpus=%s strategy=%s at=%s\n",
			v.Flow, v.Realm, v.CorpusVersion, v.StrategyVersion, v.At.Format("15:04:05"))
	}
	if ws, ok := context.Get(c, qaflow.KeyWindows); ok {
		for _, w := range ws {
			d, _ := idx.Doc(w.SourceID)
			text, err := retrieval.ResolveSpan(d.Body, w.Span)
			if err != nil {
				return err
			}
			fmt.Printf("window: %s score=%.2f span=%s text=%q\n", w.SourceID, w.Score, w.Span, text)
		}
	}
	for _, s := range c.Components() {
		state := "inactive"
		if s.Active {
			state = "active"
		}
		line := fmt.Sprintf("component: %-16s %s", s.Name, state)
		if len(s.Missing) > 0 {
			line += fmt.Sprintf(" missing=%v", s.Missing)
		}
		if s.LastError != "" {
			line += fmt.Sprintf(" error=%s", s.LastError)
		}
		fmt.Println(line)
	}
	fmt.Println("selftest ok")
	return nil
}

package qaflow

import (
	"errors"
	"fmt"
	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
	"strings"
)

type RewriteStage struct {
	Query        string
	Hypothetical string           // 注入的改写（HyDE 式）；空 = 不改写
	Idx          *retrieval.Index // 漂移闸判语料内/外用；nil = 闸不启动
	Analyze      func(q string) query.Analysis
	Prior        bool // 开文档级多信号重排（开关在此落到 context）
}

func (RewriteStage) Name() string    { return "intent-clarify" }
func (RewriteStage) Reads() []string { return []string{"session"} }
func (RewriteStage) Writes() []string {
	// KeyPriorOn 必须申报：禁闭纪律下，写了不申报的键在 stage 结束就不可
	// 见（本轮真跑踩过——prior 开了却全程静默，就是漏申报）
	return []string{KeyRewrite.String(), KeyAnalysis.String(), KeyPriorOn.String(), KeyFacts.String()}
}
func (s RewriteStage) Run(c *context.Context) error {
	rw := Rewrite{Original: s.Query, Hypothetical: s.Hypothetical}
	_ = context.Set(c, KeyPriorOn, s.Prior)
	// 事实分解（规则版）：查询没声明清单就 K=1——宁漏勿切
	_ = context.Set(c, KeyFacts, facts.Decompose(s.Query))
	if s.Analyze != nil {
		if err := context.Set(c, KeyAnalysis, s.Analyze(s.Query)); err != nil {
			return err
		}
	}
	// 漂移闸（BioHarness：改写管召回，原问管接地）：注入的改写丢掉原问
	// 的语料内内容词 → 拦下，检索回退原问，丢词清单入账
	if s.Hypothetical != "" && s.Idx != nil {
		var dropped []string
		for _, term := range dedupe(retrieval.Fields(s.Query)) {
			if !s.Idx.HasTerm(term) {
				continue // 语料外词：改写不可能保留，不算漂移
			}
			if !strings.Contains(s.Hypothetical, term) {
				dropped = append(dropped, term)
			}
		}
		if len(dropped) > 0 {
			rw.DriftRejected = true
			rw.DroppedTerms = dropped
			rw.Hypothetical = ""
		}
	}
	return context.Set(c, KeyRewrite, rw)
}
func (s RewriteStage) Verify(c *context.Context) error {
	r, ok := context.Get(c, KeyRewrite)
	if !ok {
		return errors.New("rewrite missing")
	}
	if r.Original != s.Query {
		return fmt.Errorf("rewrite drifted from original question: %q != %q", r.Original, s.Query)
	}
	return nil
}

// ---------- stage 2: 证据供给 ----------

// EvidenceStage 从检索后端取证据窗口。Retrieve 收 context：候选区信念
// 这类按查询变化的输入从 context 读，不靠闭包捕获——命中过什么、
// 当前信什么，因此都进得了提交视图。

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/decide"
	"github.com/willove/cumulus/internal/evalfcore"
	"github.com/willove/cumulus/internal/judge"
	"github.com/willove/cumulus/internal/qaflow"
)

// verifyWithDecision 是**答案级验证**（post-answer verification）：合成之后、
// 交付之前问决策模型两句——"答案的事实点都在证据里有依据吗"（noul）与
// "每条窗口与问题是什么关系"（GaRAGe 四级）。
//
// 为什么它与已有信号不同：confidence/coverage/margin/support 全是**检索侧**的
// （问"窗够不够"），而这三问的是**答案侧**（"这句答案有没有证据"）。而且决策模型
// 与合成器**不同家族**——本项目最忌讳的循环（判官=合成器，拿判官输出预测判官
// 决定必然满分）在这里不成立，所以它是**唯一可作为独立信号评估的**候选。
//
// 口径与纪律：
//   - 失败**如实留痕**（Reason 记原因），不静默当成 0——0 与"没跑"必须可区分；
//   - 一次调用问两句（决策模型支持多问题同问），~70ms/题，比合成便宜两个数量级；
//   - 默认**关**（CUMULUS_DECIDE=1 开）：未在完整跑动上验过的机制不进默认路径。
type decideVerifier struct {
	client *decide.Client
}

func verifyWithDecision(c *decide.Client) *decideVerifier { return &decideVerifier{client: c} }

// Verify 返回填好三字段的 outcome 与一句原因（空 = 成功）。
func (v *decideVerifier) Verify(ctx gocontext.Context, question string, windows []qaflow.EvidenceWindow, answer string) (noul float64, choice string, conf float64, reason string) {
	if v == nil || v.client == nil {
		return 0, "", 0, "decide: not bound"
	}
	if strings.TrimSpace(answer) == "" {
		return 0, "", 0, "refused: no answer to verify"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "问题：%s\n", question)
	for i, w := range windows {
		text := w.Text
		if r := []rune(text); len(r) > 200 {
			text = string(r[:200]) + "…"
		}
		fmt.Fprintf(&b, "[%d] %s\n", i+1, strings.TrimSpace(text))
	}
	resp, err := v.client.Ask(ctx, decide.Request{
		Content: b.String() + "\n候选答案：" + truncateRunes(answer, 400),
		Questions: map[string]decide.Question{
			"grounded": {
				Type:         decide.TypeNoul,
				Instructions: "候选答案的事实点是否都能在上面这些证据原文里找到依据？措辞不同没关系，答案只覆盖证据的一部分也可以。",
			},
			"relation": {
				Type:         decide.TypeChoice,
				Instructions: "证据与问题的关系是？",
				Criteria: map[string]string{
					"ANSWER":   "这条证据原文直接给出了问题的答案",
					"RELATED":  "只谈相关话题，没有答案",
					"OUTDATED": "这条给出的是过时的答案",
					"UNKNOWN":  "无法判断",
				},
			},
		},
	})
	if err != nil {
		return 0, "", 0, "decide: " + err.Error()
	}
	g := resp.Answers["grounded"]
	rel := resp.Answers["relation"]
	// relation 用**分数型**问（选中最相关的那条）——choice 只回一个标签，
	// 我们要的是"这条最相关的证据是不是答案"，用 score 拿到连续量便于加权。
	conf = g.Confidence
	if conf == 0 {
		conf = rel.Confidence
	}
	return g.Noul, rel.Choice, conf, ""
}

var _ = evalfcore.Verdict{}

// decideEnabled 读 CUMULUS_DECIDE（默认关）。决策模型验证默认不进标准路径——
// 它是一次研究实验（用独立信号测 AUC），不是产品行为。
func decideEnabled() bool { return os.Getenv("CUMULUS_DECIDE") == "1" }

// judgeFromEnv 装配判官：
//
//	CUMULUS_JUDGE=llm    等义判官（默认口径：答案是否被金标支持）
//	CUMULUS_JUDGE=points 分点覆盖判官（金标拆要点，逐条命中比例过阈）
//
// 两者读数不可混用：points 口径下"判对"= 覆盖了大部分要点，llm 口径下=
// 整段被支持。多跳/长金标任务上 points 才是对的那个口径（真跑：DomainRAG
// multidoc 等义判官 6.8%，而答案与金标常几乎逐字一致）。
func judgeFromEnv(which string) (judge.Judge, error) {
	if which != "llm" && which != "points" {
		return nil, nil
	}
	c, err := llmFromEnvImpl()
	if err != nil {
		return nil, err
	}
	if which == "points" {
		thr := 0.0
		if v := os.Getenv("CUMULUS_JUDGE_TAU"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
				thr = f
			}
		}
		return &judge.Points{Client: c, Threshold: thr}, nil
	}
	return &judge.LLM{Client: c}, nil
}

func judgeLabel(which string) string {
	switch which {
	case "llm":
		return "llm(等义)"
	case "points":
		return "points(分点覆盖)"
	default:
		return "none (N/A)"
	}
}

// truncateRunes 截断到 n 个字符（eval 明细打印用）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// shortSHA 安全截断指纹（12 位）。指纹长度随来源不同（内容寻址 16 位、
// 演示用的短串），硬切 [:12] 会在短串上 panic——真跑踩过。
func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// widthFromKnobs 取证据窗口宽度（桥的加权重取与首程同一把尺，否则两路不可比）。
func widthFromKnobs(knobs map[string]float64) int {
	if knobs != nil && knobs["evidence.width"] >= 1 {
		return int(knobs["evidence.width"])
	}
	return 400
}

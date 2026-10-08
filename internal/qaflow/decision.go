package qaflow

import (
	gocontext "context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/willove/cumulus/internal/context"
	"github.com/willove/cumulus/internal/decide"
)

// KeyDecision 是决策层的挂点：它的值**永远有**（哪怕决策没发生），
// 所以"有没有在决策"这件事是**可查询的事实**，不是从日志里猜的。
var KeyDecision = context.NewKey[DecisionRecord]("decision")

// DecisionRecord 是可降级决策的**可观测契约**。
//
// 三个不变式（每条都有测试钉死）：
//  1. **缺席不改行为**：client == nil 时不决策、不报错，调用方拿到的输入
//     与输出逐字段相同（TestDecisionAbsentIsNoop 钉死）；
//  2. **失败不阻断**：决策模型挂了不是答案失败——原样放行 + 留痕
//     （TestDecisionFailureDegradesInsteadOfBlocking 钉死）；
//  3. **degraded 必须可见**：Reason 永远有值（not-bound / error:… / ok），
//     "没接上"和"接上了但没生效"在读数里分得开（TestDecisionRecordsEveryOutcome）。
//
// 为什么要有这一层而不是"加个信号"：本项目三·补三十七到四十已证伪三次
// "更聪明的打分"，决策模型的价值只可能在**架构层**（便宜、逐题一致、可做闸门），
// 而任何架构能力都必须**能安全缺席**——否则它就从"可选增强"变成"硬依赖"，
// 一次外部服务抖动就会把问答面带下线。
type DecisionRecord struct {
	Source     string  `json:"source"`           // 决策面来源（model id）
	Kind       string  `json:"kind"`             // 这一层在判什么（answerable / grounded / relation…）
	Applied    bool    `json:"applied"`          // 决策是否真的发生
	Reason     string  `json:"reason"`           // not-bound / error:… / ok
	Noul       float64 `json:"noul,omitempty"`   // 0..1
	Choice     string  `json:"choice,omitempty"` // 选项型答案
	Confidence float64 `json:"confidence,omitempty"`
	LatencyMS  int64   `json:"latency_ms"`
}

// DecisionDecider 是一个可空决策器（nil = 这一层没接决策面）。
type DecisionDecider struct {
	Client *decide.Client
	// GateThreshold：noul 低于它就算"证据不足以支撑答案"。**只在这一层
	// 有意义**：它把"能不能合成"这件事交给架构，而不是事后统计。
	GateThreshold float64
}

// Decide 问一句，返回判断与留痕。**永不返回 error**——决策失败不是问答失败。
//
// 问题三类（choice/noul/score 的形状由 decide 包负责，这里只问"是否答得出来"）：
//
//	KindAnswerable："这些窗口里能不能找到问题的答案？"
//
// 返回的 ok=false 表示"决策说不行"（或决策缺席/失败而没有依据说不行）——
// 调用方据此决定放不放行；**缺席与失败都算放行**（保持今天的行为）。
func (d *DecisionDecider) Decide(ctx gocontext.Context, kind, question string, windows []EvidenceWindow) (ok bool, noul float64, rec DecisionRecord) {
	rec = DecisionRecord{Kind: kind, Reason: "not-bound"}
	if d == nil || d.Client == nil {
		return true, 0, rec // 缺席 = 放行（行为不变）
	}
	rec.Source = d.Client.Model

	var sb strings.Builder
	fmt.Fprintf(&sb, "问题：%s\n证据：\n", question)
	for i, w := range windows {
		text := w.Text
		if r := []rune(text); len(r) > 200 {
			text = string(r[:200]) + "…"
		}
		fmt.Fprintf(&sb, "[%d] %s\n", i+1, strings.TrimSpace(text))
	}
	thr := d.GateThreshold
	if thr <= 0 || thr > 1 {
		thr = 0.5 // 默认闸门（真上线前要按数据定，别拍）
	}

	start := time.Now()
	resp, err := d.Client.Ask(ctx, decide.Request{
		Content: sb.String(),
		Questions: map[string]decide.Question{
			"answerable": {
				Type:         decide.TypeNoul,
				Instructions: "这些证据里能不能找到这个问题的答案？",
			},
		},
	})
	rec.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		rec.Reason = "error: " + err.Error()
		return true, 0, rec // 失败 = 放行，但留痕
	}
	rec.Applied = true
	rec.Reason = "ok"
	if a, ok := resp.Answers["answerable"]; ok {
		rec.Noul, rec.Confidence = a.Noul, a.Confidence
	}
	return rec.Noul >= thr, rec.Noul, rec
}

// DecideFromEnv 装配可降级决策器（CUMULUS_DECIDE=1 开）。没配 key 就返回
// nil ——**nil 就是"这一层没接上"，是合法状态，不是错误**。
func DecideFromEnv(kind string) *DecisionDecider {
	if os.Getenv("CUMULUS_DECIDE") != "1" {
		return nil
	}
	c, err := decide.FromEnv()
	if err != nil {
		return nil
	}
	return &DecisionDecider{Client: c}
}

// DecisionRecordOf 取决策留痕（没有就返回零值——"没决策过"与"决策结果是零"可区分：
// 前者 Applied=false 且 Reason 非空）。
func DecisionRecordOf(c *context.Context) (DecisionRecord, bool) {
	return context.Get(c, KeyDecision)
}

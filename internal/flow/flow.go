// Package flow 是流程框架：stage 契约 + runner。
//
// runner 做三件事（依据 docs/flow-grammar.md）：
//   - 禁闭：stage 运行期间只许写它声明的 key（context 守卫）；
//   - 反卷：任一步失败，回放本流程的全部注册（LIFO），错误带上撤销清单；
//   - 记视图：全流程成功才写提交视图。
package flow

import (
	"fmt"
	"time"

	"github.com/willove/cumulus/internal/context"
)

// Stage 是一次迁移中的一步。Reads/Writes 是契约：Reads 供查阅与静态核对，
// Writes 由 runner 强制执行（写声明之外的 key 会直接失败）。
// TracePhase 是阶段观测的相位。
type TracePhase string

// 相位取值：开始、正常结束、失败结束。
const (
	TraceStart TracePhase = "start"
	TraceDone  TracePhase = "done"
	TraceFail  TracePhase = "fail"
)

// TraceFunc 是**阶段观测钩子**：kernel 只声明形状，不认识上层的事件词表。
//
// 为什么在 kernel 里放这个类型：flow 是底座，它**不许依赖** harness（输出面在
// capabilities 层，反向依赖会被 boundary 门禁拦住）。所以这里用**纯函数类型**，
// 由上层（qaflow/api）负责把相位翻译成事件。底座给一个"你可以观测我"的洞，
// 而不是让它知道谁在观测。
type TraceFunc func(c *context.Context, name string, phase TracePhase, durMS int64)

// Stage 是一个步骤的契约：能读什么、能写什么、怎么跑、跑完怎么自检。
type Stage interface {
	Name() string
	Reads() []string
	Writes() []string
	// Run 执行本步。返回 error 即本步失败，触发反卷。
	Run(c *context.Context) error
	// Verify 是跑完后的便宜信号检查（接地、格式、可回溯），
	// 返回 error 同样触发反卷。没有便宜信号可查时返回 nil。
	Verify(c *context.Context) error
}

// Runner 按顺序跑一组 stage。
type Runner struct {
	// Trace 非 nil 时逐阶段回调（开始/结束/失败 + 耗时）。**可选**：不设时
	// Run 逐字段不变——观测面缺席不许改变流程行为（harness 契约 1）。
	Trace  TraceFunc
	Flow   string
	Stages []Stage
	// View 是本次迁移要记录的提交视图（四版本由调用方填）。
	View context.CommittedView
	// ViewHook 在 Commit 之前调用，让流程把运行时才知道的版本填进视图
	// （典型：路由实际生效的档位与校准程序——只有 stage 跑完才存在）。
	// 可空。填进来的字段是"发生额"，不是调用方的声明值。
	ViewHook func(c *context.Context, v *context.CommittedView)
}

// Run 执行全部 stage。任一 stage 的 Run 或 Verify 失败：
// 反卷本流程的全部注册，返回包装后的错误。全部成功才 Commit。
func (r *Runner) Run(c *context.Context) error {
	mark := c.Mark()
	for _, st := range r.Stages {
		name := st.Name()
		stStart := time.Now()
		if r.Trace != nil {
			r.Trace(c, name, TraceStart, 0)
		}
		c.BeginStage(name, st.Writes())
		err := st.Run(c)
		c.EndStage()
		if err != nil {
			notes := c.UnwindTo(mark)
			if r.Trace != nil {
				r.Trace(c, name, TraceFail, time.Since(stStart).Milliseconds())
			}
			return fmt.Errorf("flow %s: stage %s: %w (unwound %d)", r.Flow, name, err, len(notes))
		}
		c.BeginStage(name, st.Writes())
		verr := st.Verify(c)
		c.EndStage()
		if verr != nil {
			notes := c.UnwindTo(mark)
			if r.Trace != nil {
				r.Trace(c, name, TraceFail, time.Since(stStart).Milliseconds())
			}
			return fmt.Errorf("flow %s: stage %s verify: %w (unwound %d)", r.Flow, name, verr, len(notes))
		}
		if r.Trace != nil {
			r.Trace(c, name, TraceDone, time.Since(stStart).Milliseconds())
		}
	}
	r.View.Flow = r.Flow
	if r.ViewHook != nil {
		r.ViewHook(c, &r.View)
	}
	c.Commit(r.View)
	return nil
}

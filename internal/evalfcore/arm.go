package evalfcore

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Arm 是一次评测里的一个对照臂。
//
// 臂是**注册**（v0.2 §三.3）：加一条基线、换一个判官是装一个注册，不是
// 改运行器。注册化之后才谈得上合流测试（随机装卸重放、终态等价）。
type Arm struct {
	ID   string // 臂标签（bm25 / bm25-bare / deep …）
	Dumb bool   // 最笨基线标记（见 ValidateArms）
	Note string // 人读的说明（进输出，便于审计这一臂到底动了什么）
	Run  func(ctx context.Context) (RunState, error)
}

// ValidateArms 强制哑对照臂不变量（v0.2 §三.7）：
// **每次对照运行必须至少含一个最笨基线臂**（纯 BM25 top-k、零改写、
// 零管理）。精致臂不赢哑臂不许上线——纯 BM25 85% 对全管线 51.7%、
// Letta 文件系统 74% 胜专用记忆系统，两条都是这条不变量的证据。
//
// 为什么是硬校验而不是注释：没有哑臂时，任何"改进了 x%"都无法归因
// （是管线在增值，还是在补自己造的洞），对账也就无从谈起。
func ValidateArms(arms []Arm) error {
	if len(arms) == 0 {
		return errors.New("evalfcore: no arms registered")
	}
	ids := make([]string, 0, len(arms))
	dumb := ""
	for _, a := range arms {
		if a.ID == "" {
			return errors.New("evalfcore: arm with empty id")
		}
		if a.Run == nil {
			return fmt.Errorf("evalfcore: arm %q has no executor", a.ID)
		}
		ids = append(ids, a.ID)
		if a.Dumb {
			dumb = a.ID
		}
	}
	if dumb == "" {
		return fmt.Errorf("evalfcore: no dumb baseline arm among [%s] (grammar §三.7: a comparison run must include the dumbest baseline)",
			strings.Join(ids, " "))
	}
	return nil
}

// RunArms 按注册顺序跑全部臂，先校验不变量（缺哑臂直接拒绝开跑，
// 不烧钱跑完再说）。
func RunArms(ctx context.Context, arms []Arm) ([]RunState, error) {
	if err := ValidateArms(arms); err != nil {
		return nil, err
	}
	out := make([]RunState, 0, len(arms))
	for _, a := range arms {
		st, err := a.Run(ctx)
		if err != nil {
			return out, fmt.Errorf("evalfcore: arm %s: %w", a.ID, err)
		}
		out = append(out, st)
	}
	return out, nil
}

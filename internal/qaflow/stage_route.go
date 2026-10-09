package qaflow

import (
	"errors"
	"fmt"
	"github.com/willove/cumulus/internal/context"
)

type RouteStage struct {
	// Config 是阈值与校准来源。零值 = 手工基线 0.5 + 每事实 0.05。
	Config RouteConfig
	// hasEscalate 是**运行期**复核（EscalateStage.Available()）。配置里的
	// HasEscalate 是接线声明；两个都真才允许"零窗口先升级"——声明有而实际没装
	// 执行处时，先升级只是空转。
	hasEscalate bool
}

func (RouteStage) Verify(c *context.Context) error {
	d, ok := context.Get(c, KeyRoute)
	if !ok {
		return errors.New("route decision missing")
	}
	switch d.Action {
	case "fast", "escalate", "refuse":
		return nil
	default:
		return fmt.Errorf("unknown route action %q", d.Action)
	}
}

// ---------- stage 4: 合成 ----------

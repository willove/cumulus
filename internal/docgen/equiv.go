package docgen

import (
	gocontext "context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willove/cumulus/internal/llm"
)

// EquivalentJudgeLLM 是**模型版语义判据**：一次调用判断多对论断是否同一件事。
//
// 为什么只在词面判据兜不住时才用（`anySameTo` 的口径）：
//   - 每对都调模型 = N×M 次调用，太贵；
//   - 词面判据（精确 → 二元组覆盖 0.75 → 字符集合 0.9）已经能处理绝大多数
//     "标点/顺序/引用标记"的差异，那些不必惊动模型。
//
// **保守方向**：解析失败、模型缺席、判不出来 → 一律返回 false（视为不同）。
// 理由与整包的纪律一致：**宁可多报差异，不可谎报合并**——合并错了会掩盖真实的
// 信息变化（读者以为"没变"，其实内容已经变了）。
type EquivalentJudgeLLM struct {
	Client llm.Completer
	// MaxPairs 一次判多少对（0 = 默认 12）。超出的对数不判 → 视为不同。
	MaxPairs int
}

const equivSystem = `你是论断等价判定器。判断两条陈述是否在说**同一件事**（允许同义改写、
语序不同、标点不同、用词不同；不允许含义增加或减少）。

只输出 JSON，形如 {"same":[1,2],"diff":[3]}，编号是下面给出的对号。`

// Judge 返回一个判据函数（内部按需批量调用）。
func (j *EquivalentJudgeLLM) Judge() EquivalentJudge {
	return func(a, b string) (bool, error) {
		if j == nil || j.Client == nil {
			return false, fmt.Errorf("docgen: 无判据 LLM")
		}
		// 单对：一次调用只判一对（调用面保持极简；真正的批量优化留给有需要时）
		var sb strings.Builder
		sb.WriteString("第 1 对：\nA: " + trimClaim(a) + "\nB: " + trimClaim(b) + "\n")
		resp, err := j.Client.Complete(gocontext.Background(), llm.Request{
			System: equivSystem, Prompt: sb.String(), MaxTokens: 120,
		})
		if err != nil {
			return false, fmt.Errorf("docgen: 判据调用: %w", err)
		}
		raw := llm.LastJSONObject(resp.Text)
		if raw == "" {
			return false, fmt.Errorf("docgen: 判据回包无 JSON")
		}
		var v struct {
			Same []int `json:"same"`
			Diff []int `json:"diff"`
		}
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return false, fmt.Errorf("docgen: 判据回包不合约定: %w", err)
		}
		for _, n := range v.Same {
			if n == 1 {
				return true, nil
			}
		}
		return false, nil // 判"否"或没提到 → 不同（保守）
	}
}

func trimClaim(s string) string {
	if len([]rune(s)) > 120 {
		return string([]rune(s)[:120]) + "…"
	}
	return s
}

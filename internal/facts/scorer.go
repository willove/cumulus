package facts

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/willove/cumulus/internal/llm"
)

// Scorer 是事实覆盖的模型判官（词面判据的兜底升级）。
//
// 为什么需要：词面判据（内容词占比 + 3 字核心）认不出改写。真法条写
// "发明专利权的期限为二十年"，问句是"专利期限是多少年"——内容占比过
// 半，但 3 字核心"专利期"在条文里不存在（条文是"专利**权**的期限"），
// 词面判 uncovered，而答案明明就在窗里。判官看一眼就知道"这窗说了二
// 十年 = 支撑"。
//
// 这符合 Noesis（arXiv:2609.07663）的判定：这个尺寸的模型瓶颈是上
// 下文利用——"读一段话判它支不支持这条事实"正是它擅长的利用，不是
// 它不擅长的生成。cumulus 的 FactAware scorer 同思路（一次评分更新
// 全部事实的覆盖）。
//
// 纪律：
//   - 只**兜底升级**：词面判已盖的事实不劳模型（省钱，也少一条不确定
//     链路）；
//   - 批一次：所有未盖事实 × 全部窗一次调用；
//   - 判不出支撑 = 空列表（模型不许和稀泥——兜底不是放水）；
//   - 失败不阻塞：返回 err，调用方保持词面原判（判官缺席时流程照跑）。
type Scorer interface {
	// Rescue 判每条未盖事实被哪些窗支撑。返回 factID → 支撑窗
	// （"SourceID#Span"）列表。没支撑的事实不进 map。
	Rescue(ctx context.Context, missing []Fact, ws []Window) (map[string][]string, error)
}

// completer 判官需要的最小补全面（测试注入假件，生产传真 client）。
type completer interface {
	Complete(ctx context.Context, req llm.Request) (llm.Response, error)
}

// LLMScorer 是用 MiniMax 做判官的实现。
type LLMScorer struct {
	Client completer
}

const scorerSystemPrompt = `你是证据判官。任务：判断给定的证据窗口是否**支撑**给定的事实。

规则（严格执行）：
1. 只看窗口原文说了什么。窗口含有该事实的答案内容（数字、条件、结论）才算支撑；只提了同样的词、答非所问不算。
2. 一条事实可以被多个窗支撑，也可以一个都没有。没有就给出空列表——判不出就是判不出，不许和稀泥。
3. 你只judgment支撑关系，不回答问题本身。
4. 只输出 JSON：{"support":{"f1":["w2"],"f2":[]}}，键是事实 id，值是窗口号列表。`

// 判官提示词里的窗**不截断**。截断是把答案切掉：条文吸附的窗常跨两
// 三条文（如第四十~四十二共 538 字，"二十年"在第二百七十字起），截
// 到 200 字判官就看不见答案、判"没支撑"——真跑教训：锚点已经落在正确
// 条文，判官却因截断把 f1 判没支撑。12 窗 × 最多几百字 = 几 KB 提示
// 词，个人工具付得起。

// Rescue 批量判"未盖事实 × 全部窗"。
func (s *LLMScorer) Rescue(ctx context.Context, missing []Fact, ws []Window) (map[string][]string, error) {
	if s.Client == nil || len(missing) == 0 || len(ws) == 0 {
		return nil, fmt.Errorf("facts: scorer not wired or nothing to judge")
	}
	capped := ws
	if len(capped) > 12 {
		capped = capped[:12]
	}
	var b strings.Builder
	b.WriteString("待判事实：\n")
	for _, f := range missing {
		fmt.Fprintf(&b, "  %s「%s」\n", f.ID, f.Query)
	}
	b.WriteString("\n证据窗口：\n")
	for i, w := range capped {
		fmt.Fprintf(&b, "  [w%d] %s（%s）%s\n", i+1, w.SourceID, w.Span, w.Text)
	}
	resp, err := s.Client.Complete(ctx, llm.Request{
		System:    scorerSystemPrompt,
		Prompt:    b.String(),
		MaxTokens: 256,
	})
	if err != nil {
		return nil, fmt.Errorf("facts: scorer complete: %w", err)
	}
	var out struct {
		Support map[string][]string `json:"support"`
	}
	if err := json.Unmarshal([]byte(cleanJSON(resp.Text)), &out); err != nil || out.Support == nil {
		// 判官违约（不是严格 JSON）= 这次没有判官，不是错误答案——
		// 调用方保持词面原判
		return nil, fmt.Errorf("facts: scorer output not the agreed JSON: %w", err)
	}
	support := out.Support
	// 只接受认识的事实 id 与存在的窗口号（其余忽略——判官输出不可全信）
	known := map[string]bool{}
	for _, f := range missing {
		known[f.ID] = true
	}
	result := map[string][]string{}
	for id, list := range support {
		if !known[id] {
			continue
		}
		for _, label := range list {
			idx := windowLabelIndex(label)
			if idx < 0 || idx >= len(capped) {
				continue
			}
			w := capped[idx]
			result[id] = append(result[id], w.SourceID+"#"+w.Span)
		}
		if len(result[id]) == 0 {
			delete(result, id)
		}
	}
	return result, nil
}

// windowLabelIndex "w3" → 2（-1 = 不认识）。
func windowLabelIndex(label string) int {
	label = strings.TrimSpace(label)
	label = strings.TrimPrefix(label, "[")
	label = strings.TrimSuffix(label, "]")
	if !strings.HasPrefix(label, "w") {
		return -1
	}
	n := 0
	for _, c := range label[1:] {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return -1
	}
	return n - 1
}

// cleanJSON 剥掉模型输出常见的 ```json 围栏。
func cleanJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}

// Rescue 判官兜底：对 rep 里**词面判没盖**的事实（Missing 里的）问一次
// 模型，判有支撑的把 Covered/Supports 补上并重算 Complete/Missing。
//
// 三条纪律：
//   - 只升级不降级：词面已盖的事实不过问（省钱，也少一条不确定链路）；
//   - 判官失败/违约 = 这次没有判官：rep 保持词面原判返回 err，流程不
//     被拖死；
//   - 支撑窗必须真在窗集里（模型给的坐标核不上就丢——判官输出不可全
//     信）。
func Rescue(rep *Report, ws []Window, sc Scorer) error {
	if sc == nil || len(rep.Missing) == 0 || len(ws) == 0 {
		return nil
	}
	byID := map[string]*Fact{}
	var missing []Fact
	for i := range rep.Facts {
		byID[rep.Facts[i].ID] = &rep.Facts[i]
		if !rep.Facts[i].Covered {
			missing = append(missing, rep.Facts[i])
		}
	}
	got, err := sc.Rescue(context.Background(), missing, ws)
	if err != nil {
		for i := range rep.Facts {
			if !rep.Facts[i].Covered {
				rep.Facts[i].Judge = "error: " + err.Error()
			}
		}
		return err
	}
	if len(got) == 0 {
		for i := range rep.Facts {
			if !rep.Facts[i].Covered {
				rep.Facts[i].Judge = "no-support"
			}
		}
		return nil
	}
	byKey := map[string]Window{}
	for _, w := range ws {
		byKey[w.SourceID+"#"+w.Span] = w
	}
	for id, keys := range got {
		f, ok := byID[id]
		if !ok {
			continue
		}
		var sup []Window
		for _, k := range keys {
			if w, ok := byKey[k]; ok {
				sup = append(sup, w)
			}
		}
		if len(sup) == 0 {
			continue
		}
		for i := 1; i < len(sup); i++ {
			for j := i; j > 0 && sup[j].Score > sup[j-1].Score; j-- {
				sup[j], sup[j-1] = sup[j-1], sup[j]
			}
		}
		f.Covered = true
		f.Supports = sup
		f.Score = sup[0].Score
		f.Judge = "rescued"
	}
	// 重算汇总
	rep.Complete = true
	rep.Missing = nil
	for _, f := range rep.Facts {
		if !f.Covered {
			rep.Complete = false
			rep.Missing = append(rep.Missing, f.ID)
		}
	}
	return nil
}

// Package synth 是合成面的两种实现：离线确定性合成与 LLM 合成。
//
// 契约同源（qaflow.SynthFunc）：给问题与证据窗口，产出**带引用的答案**
// ——每个断言必须挂到窗口 id 上。挂不上的断言不许存在（流程文法 §2.2），
// 这条由合约解析处的校验兜底，Verify 再查一遍。
//
// 离线合成（Offline）不用模型：把窗口原文按序拼成断言。它存在的意义
// 是让全链路在没有 key 的环境里可跑可测；它不是“简化版 LLM”，是
// 另一条腿——评测的抽取式基线就是它。
package synth

import (
	"encoding/json"
	"fmt"
	"strings"

	gocontext "context"

	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/qaflow"
)

// Offline 是确定性合成：每个窗口一条断言，答案取第一条。
// 用法与 LLM 版完全同构——调用方换实现不换契约。
func Offline(question string, windows []qaflow.EvidenceWindow) (qaflow.Answer, qaflow.Usage, error) {
	if len(windows) == 0 {
		return qaflow.Answer{}, qaflow.Usage{}, fmt.Errorf("synth offline: no evidence windows to synthesize from")
	}
	ans := qaflow.Answer{Refused: false}
	var texts []string
	for _, w := range windows {
		cit := w.SourceID + "#" + w.Span
		ans.Citations = append(ans.Citations, cit)
		texts = append(texts, w.Text)
	}
	ans.Text = strings.Join(texts, " ") // 离线合成的答案就是证据原文本身
	return ans, qaflow.Usage{CostKnown: false}, nil
}

// LLM 用模型合成。提示词里带窗口契约，输出必须是严格 JSON：
//
//	{"answer": "...", "assertions": [{"text": "...", "window": "w1"}]}
//
// 其中 window 是提示词里分配的窗口编号。引用编号在解析时换成
// "docID#span" 坐标——模型只碰编号，碰不到原文坐标，减少幻觉面。
//
// **拒答是一等结局**（真实运行学到的）：契约第三字段
//
//	{"answer": "", "assertions": [], "refused": true}
//
// 证据确实答不了时模型必须走这个协议。第一版没有它，模型用自然语言
// （"现有证据未提供……无法作答"）+ 空断言表达拒答，被契约当成错误
// 把整题失败掉——拒答是合法结局，不是失败；失败的是把拒答混进答案。
// 拒答时 answer 必须为空、断言必须为空（Verify 也查）。
type LLM struct {
	Client llm.Completer
}

type llmAnswer struct {
	Answer     string         `json:"answer"`
	Assertions []llmAssertion `json:"assertions"`
	Refused    bool           `json:"refused"`
}

type llmAssertion struct {
	Text   string `json:"text"`
	Window string `json:"window"` // w1 / w2 ...，指向提示词里的窗口编号
}

// Synthesize 实现 qaflow.SynthFunc。
func (l *LLM) Synthesize(question string, windows []qaflow.EvidenceWindow) (qaflow.Answer, qaflow.Usage, error) {
	if l.Client == nil {
		return qaflow.Answer{}, qaflow.Usage{}, llm.ErrNotConfigured
	}
	if len(windows) == 0 {
		return qaflow.Answer{}, qaflow.Usage{}, fmt.Errorf("synth llm: no evidence windows")
	}

	// 窗口编号化：模型只认 wN
	labels := make([]string, len(windows))
	for i := range windows {
		labels[i] = fmt.Sprintf("w%d", i+1)
	}

	resp, err := l.Client.Complete(gocontext.Background(), llm.Request{
		System:    synthesisSystemPrompt,
		Prompt:    buildSynthesisPrompt(question, windows, labels),
		MaxTokens: 1024,
	})
	if err != nil {
		return qaflow.Answer{}, qaflow.Usage{}, fmt.Errorf("synth llm: complete: %w", err)
	}
	ans, err := parseSynthesis(resp.Text, windows, labels)
	if err != nil {
		// 结构化错误回给调用方：契约不符就重试，不许把半成品当答案
		return qaflow.Answer{}, qaflow.Usage{}, err
	}
	return ans, qaflow.Usage{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		CostKnown:        resp.Usage.CostKnown,
	}, nil
}

// parseSynthesis 严格解析并校验引用。任何一条断言挂不上窗口即整体失败——
// 要么全过，要么不过（qc 的“无证据断言不许存在”）。
func parseSynthesis(raw string, windows []qaflow.EvidenceWindow, labels []string) (qaflow.Answer, error) {
	raw = strings.TrimSpace(raw)
	// 容忍 ```json 围栏（模型常见输出）
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var parsed llmAnswer
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return qaflow.Answer{}, fmt.Errorf("synth llm: output is not the agreed JSON contract: %w", err)
	}
	// 拒答协议：answer 与 assertions 都必须空；带文本的"拒答"是混日子，
	// 按错误处理（拒答不许夹带答案）
	if parsed.Refused {
		if strings.TrimSpace(parsed.Answer) != "" || len(parsed.Assertions) > 0 {
			return qaflow.Answer{}, fmt.Errorf("synth llm: refused answer must not carry text or assertions; raw=%q", truncate(raw, 200))
		}
		return qaflow.Answer{Refused: true}, nil
	}
	if strings.TrimSpace(parsed.Answer) == "" && len(parsed.Assertions) == 0 {
		return qaflow.Answer{}, fmt.Errorf("synth llm: empty answer; raw=%q", truncate(raw, 200))
	}

	index := make(map[string]qaflow.EvidenceWindow, len(labels))
	for i, w := range windows {
		index[labels[i]] = w
	}

	ans := qaflow.Answer{Text: parsed.Answer}
	seen := make(map[string]bool)
	for _, a := range parsed.Assertions {
		w, ok := index[a.Window]
		if !ok {
			return qaflow.Answer{}, fmt.Errorf("synth llm: assertion %q cites unknown window %q", a.Text, a.Window)
		}
		cit := w.SourceID + "#" + w.Span
		if !seen[cit] {
			seen[cit] = true
			ans.Citations = append(ans.Citations, cit)
		}
	}
	if len(ans.Citations) == 0 {
		return qaflow.Answer{}, fmt.Errorf("synth llm: no window-backed assertion (assertions=%d, windows=%d); refusing to emit an uncited answer; raw=%q",
			len(parsed.Assertions), len(windows), truncate(raw, 300))
	}
	return ans, nil
}

const synthesisSystemPrompt = `你是证据合成器。规则只有一条：每个断言都必须引用给定的证据窗口，
不许凭记忆补充。输出严格 JSON：{"answer": "最终答案", "assertions": [{"text": "断言", "window": "wN"}]}。
没有窗口支持的要点，删掉，不要写。

证据不足以回答问题时，输出 {"answer": "", "assertions": [], "refused": true}——
宁可说不知道，不许用窗口外的话拼答案。`

func buildSynthesisPrompt(question string, windows []qaflow.EvidenceWindow, labels []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "问题：%s\n\n证据窗口：\n", question)
	for i, w := range windows {
		fmt.Fprintf(&b, "[%s] 文档 %s 位置 %s 得分 %.2f\n原文：%s\n", labels[i], w.SourceID, w.Span, w.Score, w.Text)
	}
	b.WriteString("\n按系统提示的 JSON 契约作答。")
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

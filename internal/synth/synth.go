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

	"github.com/willove/cumulus/internal/facts"
	"github.com/willove/cumulus/internal/llm"
	"github.com/willove/cumulus/internal/qaflow"
)

// Offline 是确定性合成：每个窗口一条断言，答案取第一条。
// 用法与 LLM 版完全同构——调用方换实现不换契约。
func Offline(question string, windows []qaflow.EvidenceWindow, _ facts.Report) (qaflow.Answer, qaflow.Usage, error) {
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
func (l *LLM) Synthesize(question string, windows []qaflow.EvidenceWindow, fx facts.Report) (qaflow.Answer, qaflow.Usage, error) {
	if l.Client == nil {
		return qaflow.Answer{}, qaflow.Usage{}, llm.ErrNotConfigured
	}
	if len(windows) == 0 {
		return qaflow.Answer{}, qaflow.Usage{}, fmt.Errorf("synth llm: no evidence windows")
	}

	prompt, rendered := buildSynthesisPrompt(question, windows, fx)
	// 标签按渲染序重新连续编号（分组视图 w1..wN）——跳号标签会把模型
	// 搞晕（实测：引用空标签/不存在的标签）
	renderLabels := make([]string, len(rendered))
	for i := range renderLabels {
		renderLabels[i] = fmt.Sprintf("w%d", i+1)
	}
	resp, err := l.Client.Complete(gocontext.Background(), llm.Request{
		System:    synthesisSystemPrompt,
		Prompt:    prompt,
		MaxTokens: 1024,
	})
	if err != nil {
		return qaflow.Answer{}, qaflow.Usage{}, fmt.Errorf("synth llm: complete: %w", err)
	}
	ans, err := parseSynthesis(resp.Text, rendered, renderLabels)
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
		// 拒答可以带理由（模型天然会解释为什么拒——"现有证据不足以…"），
		// 但理由进 RefusalReason 字段，不进 Text、不许带断言。断言跟着
		// 拒答出现 = 既说不知道又摆证据，自相矛盾，按错误处理。
		if len(parsed.Assertions) > 0 {
			return qaflow.Answer{}, fmt.Errorf("synth llm: refused answer must not carry assertions; raw=%q", truncate(raw, 200))
		}
		return qaflow.Answer{Refused: true, RefusalReason: strings.TrimSpace(parsed.Answer)}, nil
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
			// 引用了渲染集里不存在的标签 = 模型编了凭据（这条断言的话没有
			// 依据）。整份输出按拒答处理：契约是"要么全过要么不过"——一条
			// 编的标签意味着整套引用纪律失效，不能只挑好的信。
			return qaflow.Answer{Refused: true, RefusalReason: "合成引用了不存在的证据窗口（模型编造引用），按契约拒答"}, nil
		}
		cit := w.SourceID + "#" + w.Span
		if !seen[cit] {
			seen[cit] = true
			ans.Citations = append(ans.Citations, cit)
		}
	}
	if len(ans.Citations) == 0 {
		// 有答案但零断言 = 模型自己也不认为这话有据。两条路都不能走：
		// 当错误抛（500 给用户）或照发（无引用的话进答案）。正路是转成
		// **正式拒答**：它写的原话进 RefusalReason——用户看到的是诚实的
		// "不确定"，且知道模型当时怎么说的（多事实分组提示下小模型偶
		// 发这个形态：逐事实作答时对没把握的那条只写了散文没挂引用）。
		if strings.TrimSpace(ans.Text) != "" {
			return qaflow.Answer{Refused: true, RefusalReason: strings.TrimSpace(ans.Text)}, nil
		}
		return qaflow.Answer{}, fmt.Errorf("synth llm: no window-backed assertion (assertions=%d, windows=%d); raw=%q",
			len(parsed.Assertions), len(windows), truncate(raw, 300))
	}
	return ans, nil
}

const synthesisSystemPrompt = `你是证据合成器。规则只有一条：每个断言都必须引用给定的证据窗口，
不许凭记忆补充。输出严格 JSON：{"answer": "最终答案", "assertions": [{"text": "断言", "window": "wN"}]}。
没有窗口支持的要点，删掉，不要写。

证据与问题相关但不完整时，用已有证据回答能答的部分，并在答案里说明局限
（如"证据未涉及具体分值"）——知识工作里"库里最接近的"胜过干巴巴的不知道；
只有证据与问题完全无关时才输出 {"answer": "", "assertions": [], "refused": true}。
无论答不答，不许用窗口外的话拼答案。

**多事实问题**：证据按事实分组给出（事实 fN + 它的支撑窗口），是为了
你不丢事实——**输出仍是一段答案 + 断言数组**，每条断言挂它来自的窗
口。某条事实没有支撑时，在答案里明说这条没有依据，不许用别条事实的
窗口内容拼它的答案。`

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

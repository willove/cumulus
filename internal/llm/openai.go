package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// transient 判瞬时网络错（超时、连接重置、EOF）。只重试这些——
// 4xx/5xx 是 HTTP 层，重试无意义（限流另说）。
func transient(err error) bool {
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "broken pipe")
}

// OpenAICompleter 是 OpenAI 兼容的 /chat/completions 客户端
// （MiniMax、DeepSeek、Groq 等同一形状）。契约与 cumulus 的
// internal/llm 对齐：choices[].message.content + usage。
type OpenAICompleter struct {
	BaseURL   string // 形如 https://api.minimaxi.com/v1（不带 /chat/completions）
	APIKey    string
	Model     string
	MaxTokens int
	Caller    string // 可选：X-LLM-Caller 头，供服务侧归因
	Client    *http.Client
}

// FromEnv 从环境变量装配。缺 base_url 或 key 或 model 时任一项缺失
// 返回 ErrNotConfigured——不许静默降级到桩。
func FromEnv(baseURL, apiKey, model string) (*OpenAICompleter, error) {
	if baseURL == "" || apiKey == "" || model == "" {
		return nil, fmt.Errorf("%w: base_url/key/model all required", ErrNotConfigured)
	}
	return &OpenAICompleter{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		Model:     model,
		MaxTokens: 1024,
		Caller:    "cumulus",
		Client:    &http.Client{Timeout: 120 * time.Second},
	}, nil
}

type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"` // 轻思考信号；M3.1 认，旧端点无害忽略
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens      int64 `json:"total_tokens"`
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete 调一次补全。上游不报 usage 时 CostKnown=false——成本未知
// 不是 0，上层据此停止后续付费调用。
func (c *OpenAICompleter) Complete(ctx context.Context, req Request) (Response, error) {
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 120 * time.Second}
	}
	body := chatRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.Prompt},
		},
		MaxTokens:       c.MaxTokens,
		ReasoningEffort: "low",
	}
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("llm: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Response{}, fmt.Errorf("llm: request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Caller != "" {
		httpReq.Header.Set("X-LLM-Caller", c.Caller)
	}

	// 有界重试：只为瞬时网络错（超时/连接重置）重试，最多 2 次，间隔
	// 短。一次 TCP 抖动不该废掉一个跑了 12 分钟的评测（真跑教训：
	// 300 题跑到 247 题被一个 read timeout 全杀掉）。
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		// 重试前重置请求体：第一次尝试已把它读走
		httpReq.Body = io.NopCloser(bytes.NewReader(raw))
		httpReq.ContentLength = int64(len(raw))
		resp, err = c.Client.Do(httpReq)
		if err == nil || attempt >= 2 || !transient(err) {
			break
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	if err != nil {
		return Response{}, fmt.Errorf("llm: do: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Response{}, fmt.Errorf("llm: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("llm: status %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}

	var out chatResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return Response{}, fmt.Errorf("llm: decode: %w", err)
	}
	if out.Error.Message != "" {
		return Response{}, fmt.Errorf("llm: upstream error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return Response{}, fmt.Errorf("llm: empty choices")
	}
	msg := out.Choices[0].Message
	text := msg.Content
	if strings.TrimSpace(text) == "" && msg.ReasoningContent != "" {
		// 推理模型偶发把所有输出放进 reasoning_content。结构化契约调用
		// （严格 JSON）可以救：取推理链里最后一个 JSON 对象。提取不放松
		// 校验——调用方照旧全过或不过；救不回来就报错，不拿推理链当答案。
		if obj := lastJSONObject(msg.ReasoningContent); obj != "" {
			text = obj
		} else {
			return Response{}, fmt.Errorf("llm: only reasoning content returned, no answer")
		}
	}
	usage := Usage{
		PromptTokens:     int(out.Usage.PromptTokens),
		CompletionTokens: int(out.Usage.CompletionTokens),
		CostKnown:        out.Usage.TotalTokens > 0,
	}
	return Response{Text: text, Usage: usage}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// lastJSONObject 从任意文本里取最后一个配平且合法的 JSON 对象（推理链里
// 挑结论）。找不到返回空串。
func lastJSONObject(s string) string {
	depth, start, found := 0, -1, ""
	for i, r := range s {
		switch r {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					if candidate := s[start : i+1]; json.Valid([]byte(candidate)) {
						found = candidate
					}
					start = -1
				}
			}
		}
	}
	return found
}

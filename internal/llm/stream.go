package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Chunk 是一次流式增量。**两个通道分开**，因为它们在产品里是两件事：
//
//	Reasoning 合成器的思考过程（harness 的 reasoning 帧，要"全部显示"）
//	Content   答案正文（harness 的 content 帧）
//
// 混在一个流里，调用方只能靠猜（"这段是思考还是答案？"）——而这正是旧
// cumulus 那种"reasoning 混进正文"的毛病。这里从类型上就不给混的机会。
type Chunk struct {
	Reasoning string
	Content   string
}

// StreamFunc 收一次增量。返回 error 时上游立刻中止（调用方的问题就别继续烧 token）。
type StreamFunc func(Chunk) error

// Streamer 是**可选**的流式补全能力。Completer 不实现它就是"只能整段拿"——
// 调用方必须能问一句"你支持流式吗"，而不是靠猜。
//
// 为什么不把它并进 Completer 接口：**并进去就得让所有实现都实现它**，
// 桩、离线合成器、测试替身全得跟着改，而它们根本不需要流式。能力用接口
// 询问（`if s, ok := c.(Streamer)`），缺席是**合法状态**，不是错误
// （与 harness 契约 1 同一个道理）。
type Streamer interface {
	Stream(ctx context.Context, req Request, fn StreamFunc) (Response, error)
}

// SupportsStream 问一句"这个补全器支持流式吗"（读数要能看见，别靠猜）。
func SupportsStream(c Completer) bool {
	_, ok := c.(Streamer)
	return ok
}

type streamRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	MaxTokens       int           `json:"max_tokens,omitempty"`
	Stream          bool          `json:"stream"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

// streamFrame 是一帧 SSE。**只取增量**（delta），不是整条消息。
type streamFrame struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
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

// Stream 走 SSE 流式补全。
//
// 与 Complete 的一致性要求：**同一个请求，两条路给出的 Text 必须一致**
// （否则同一问题两次答案不同，却说不清是哪条路的锅）。做法是：流里把两个
// 通道**各自累积**，最后拼成与 Complete 相同的正文（推理链兜底的 JSON 提取
// 也复用同一份逻辑）。
//
// 计量：流式端点**常常不回 usage**（OpenAI 兼容端点里很常见）。那时
// Usage.CostKnown=false——**成本未知不是 0**，上层据此停止后续付费调用。
func (c *OpenAICompleter) Stream(ctx context.Context, req Request, fn StreamFunc) (Response, error) {
	if fn == nil {
		return Response{}, fmt.Errorf("llm: stream needs a consumer")
	}
	if c.Client == nil {
		c.Client = &http.Client{Timeout: 120 * time.Second}
	}
	body := streamRequest{
		Model: c.Model,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.Prompt},
		},
		Stream:          true,
		ReasoningEffort: "low",
		MaxTokens:       c.MaxTokens,
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
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Caller != "" {
		httpReq.Header.Set("X-LLM-Caller", c.Caller)
	}

	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("llm: stream do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return Response{}, fmt.Errorf("llm: stream status %d: %s", resp.StatusCode, truncate(string(payload), 300))
	}

	var reasoning, content strings.Builder
	var last streamFrame
	scanErr := scanSSE(resp.Body, func(line string) error {
		if line == "[DONE]" {
			return nil
		}
		var f streamFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			// 单帧解不开就跳过这一帧（上游偶尔夹心跳/注释行），**不要**
			// 因为一帧坏掉就丢掉整条答案——但仍要在返回值里报出来。
			return nil
		}
		if f.Error.Message != "" {
			return fmt.Errorf("llm: upstream error: %s", f.Error.Message)
		}
		last = f
		ch := Chunk{}
		for _, ch2 := range f.Choices {
			ch.Reasoning += ch2.Delta.ReasoningContent
			ch.Content += ch2.Delta.Content
		}
		if ch.Reasoning == "" && ch.Content == "" {
			return nil // 空帧（心跳）不惊动调用方
		}
		if ch.Reasoning != "" {
			reasoning.WriteString(ch.Reasoning)
		}
		if ch.Content != "" {
			content.WriteString(ch.Content)
		}
		return fn(ch)
	})
	if scanErr != nil {
		return Response{}, scanErr
	}

	text := content.String()
	if strings.TrimSpace(text) == "" && reasoning.Len() > 0 {
		// 与 Complete 同一条兜底：推理模型把输出全放进 reasoning_content 时，
		// 结构化契约可以救最后一个 JSON 对象。**救不回来就报错**，
		// 不拿推理链当答案（这条不许为了"流式好做"而放松）。
		if obj := lastJSONObject(reasoning.String()); obj != "" {
			text = obj
		} else {
			return Response{}, fmt.Errorf("llm: stream returned only reasoning content, no answer")
		}
	}
	usage := Usage{
		PromptTokens:     int(last.Usage.PromptTokens),
		CompletionTokens: int(last.Usage.CompletionTokens),
		CostKnown:        last.Usage.TotalTokens > 0,
	}
	return Response{Text: text, Usage: usage}, nil
}

// scanSSE 逐行扫 SSE 流：只把 `data:` 后的载荷交给 fn。
//
// **不依赖 bufio.Scanner 的默认上限**：长答案 + 长思考很容易超过 64KB，
// 超限会静默截断流（真跑过：一条 200KB 的思考链直接断在中间，且报错难懂）。
// 自己按字节读，行缓冲按需增长。
func scanSSE(r io.Reader, fn func(payload string) error) error {
	br := bufio.NewReaderSize(r, 32*1024)
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("llm: stream read: %w", err)
		}
		line = append(line[:0], chunk...)
		for isPrefix {
			more, pfx, err2 := br.ReadLine()
			if err2 != nil {
				return fmt.Errorf("llm: stream read: %w", err2)
			}
			line = append(line, more...)
			isPrefix = pfx
		}
		text := strings.TrimSpace(string(line))
		if text == "" || strings.HasPrefix(text, ":") {
			continue // 空行/注释（心跳）
		}
		payload, ok := strings.CutPrefix(text, "data:")
		if !ok {
			continue
		}
		if err := fn(strings.TrimSpace(payload)); err != nil {
			return err
		}
	}
	return nil
}

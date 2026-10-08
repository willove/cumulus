// Package decide 是**决策模型**的客户端：给一段内容 + 若干结构化问题，拿回结构化
// 答案（选项 / 是否 / 分数 + 置信度）。
//
// 为什么研究线要它（真跑逼出来的）：
//
//   - **它不是"再一个 LLM"**。它是另一家族的专用决策模型，回答的是**离散问题**
//     （该不该升级、有没有依据、属于哪一类、有多严重），不是生成自然语言答案。
//     所以它能提供**检索侧信号根本给不出的东西**——本项目三·补三十七到四十证伪过
//     三次"更聪明的打分"，而这里换的是**问题本身**（从"窗够不够"换成"这句答案
//     有没有依据"）。
//   - **它不与合成器同族**。本项目最忌讳的循环（判官=合成器 → 用判官输出预测
//     判官决定必然满分）在这里天然不成立：verify 信号来自决策模型，标签来自
//     证据命中（确定性），两者独立。
//   - **便宜且确定**：实测 62.8ms / 125 input tokens，比合成便宜两个数量级，
//     适合放进每次问答的固定环节。
//
// 纪律：
//   - 每次调用都留 Raw（原文）与耗时——决策模型也是会变的，读数必须可回溯；
//   - 三种问题类型（choice/noul/score）都是**带 criteria 的**（choice 的判据
//     是 map，score 是有序图例）——没有判据的决策等于让模型拍脑袋。
package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL 是决策模型的 compatible-mode 端点。
const DefaultBaseURL = "https://maas.qianwenaiapi.com/compatible-mode/v1/systemone"

// DefaultModel 是决策模型名（preview 阶段，**换版要换常量并记版本**——它会变）。
const DefaultModel = "decision-model-preview"

// Client 是决策模型客户端。零值不可用，用 New。
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// FromEnv 从 DASHSCOPE_API_KEY / DECIDE_BASE_URL / DECIDE_MODEL 装客户端。
// 密钥只从环境读（.env 已 gitignore），**不落任何配置文件**。
func FromEnv() (*Client, error) {
	key := os.Getenv("DASHSCOPE_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("decide: DASHSCOPE_API_KEY not set")
	}
	base := os.Getenv("DECIDE_BASE_URL")
	if base == "" {
		base = DefaultBaseURL
	}
	model := os.Getenv("DECIDE_MODEL")
	if model == "" {
		model = DefaultModel
	}
	return New(base, key, model), nil
}

// New 构造客户端。
func New(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// 问题类型。
const (
	TypeChoice = "choice" // 单选：criteria 是 {取值: 判据说明}
	TypeNoul   = "noul"   // 是否：0..1（0.99 = 强是）
	TypeScore  = "score"  // 打分：criteria 是**有序**档位（0 起）
)

// Question 是一个结构化问题。
//
// **Criteria 与 Scale 不能各自带 `json:"criteria"` 标签**——encoding/json 遇到
// 同名标签的两个字段会把**两个都丢掉**（真跑踩过：score 的有序档位从来没发出去，
// 决策模型只好自己猜档位，而请求看起来完全正常）。所以这里自己序列化。
type Question struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"-"` // choice：判据 map
	Scale        []string          `json:"-"` // score：有序档位
}

// MarshalJSON 按类型决定 criteria 的形状（choice = map，score = 有序数组）。
func (q Question) MarshalJSON() ([]byte, error) {
	out := map[string]any{
		"type":         q.Type,
		"instructions": q.Instructions,
	}
	switch q.Type {
	case TypeChoice:
		if len(q.Criteria) > 0 {
			out["criteria"] = q.Criteria
		}
	case TypeScore:
		if len(q.Scale) > 0 {
			out["criteria"] = q.Scale
		}
	}
	return json.Marshal(out)
}

// Request 是一次决策调用。Content 是被判断的材料（一段文本/一份档案）。
type Request struct {
	TicketID  string              `json:"ticket_id,omitempty"`
	Content   string              `json:"content"`
	Questions map[string]Question `json:"questions"`
}

// Answer 是一问的答案。三种类型共享一个结构（哪一列有值看 Type）。
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`        // choice
	Noul          float64            `json:"noul,omitempty"`          // noul（0..1）
	Score         float64            `json:"score,omitempty"`         // score
	Legend        map[string]string  `json:"legend,omitempty"`        // score 的档位回显
	Probabilities map[string]float64 `json:"probabilities,omitempty"` // 分布（choice/score）
	Confidence    float64            `json:"confidence,omitempty"`    // 0..1
}

// Response 是一次调用的完整回执（含 Raw 与耗时——读数必须可回溯）。
type Response struct {
	Answers   map[string]Answer `json:"answers"`
	Raw       string            `json:"-"`
	UsageIn   int               `json:"-"`
	LatencyMS float64           `json:"-"`
	RequestID string            `json:"-"`
}

// Ask 发一次决策调用。
func (c *Client) Ask(ctx context.Context, req Request) (Response, error) {
	if c == nil || c.APIKey == "" {
		return Response{}, fmt.Errorf("decide: client not configured")
	}
	body := map[string]any{
		"model":     c.Model,
		"state":     map[string]string{"ticket_id": req.TicketID, "content": req.Content},
		"questions": req.Questions,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return Response{}, fmt.Errorf("decide: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL, bytes.NewReader(buf))
	if err != nil {
		return Response{}, fmt.Errorf("decide: request: %w", err)
	}
	httpReq.Header.Set("Authorization", c.APIKey) // 该端点用裸 key（无 Bearer）
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("decide: do: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, fmt.Errorf("decide: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Response{Raw: string(raw)}, fmt.Errorf("decide: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		RequestID string            `json:"request_id"`
		Answers   map[string]Answer `json:"answers"`
		Usage     struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
		LatencyMS float64 `json:"latency_ms"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Response{Raw: string(raw)}, fmt.Errorf("decide: unmarshal: %w", err)
	}
	return Response{
		Answers: out.Answers, Raw: string(raw),
		UsageIn: out.Usage.InputTokens, LatencyMS: out.LatencyMS, RequestID: out.RequestID,
	}, nil
}

// YesNo 是最常用的一问：材料是否满足某条件（返回 0..1 的强度与置信度）。
func (c *Client) YesNo(ctx context.Context, content, instructions string) (score, confidence float64, resp Response, err error) {
	r, err := c.Ask(ctx, Request{Content: content, Questions: map[string]Question{
		"q": {Type: TypeNoul, Instructions: instructions},
	}})
	if err != nil {
		return 0, 0, r, err
	}
	a := r.Answers["q"]
	return a.Noul, a.Confidence, r, nil
}

// Choose 问一个带判据的单选题（返回选项与分布）。
func (c *Client) Choose(ctx context.Context, content, instructions string, criteria map[string]string) (choice string, probs map[string]float64, confidence float64, err error) {
	r, err := c.Ask(ctx, Request{Content: content, Questions: map[string]Question{
		"q": {Type: TypeChoice, Instructions: instructions, Criteria: criteria},
	}})
	if err != nil {
		return "", nil, 0, err
	}
	a := r.Answers["q"]
	return a.Choice, a.Probabilities, a.Confidence, nil
}

// Score 问一个有序档位的打分题（返回分值、档位回显与置信度）。
func (c *Client) Score(ctx context.Context, content, instructions string, scale []string) (score float64, legend map[string]string, confidence float64, err error) {
	r, err := c.Ask(ctx, Request{Content: content, Questions: map[string]Question{
		"q": {Type: TypeScore, Instructions: instructions, Scale: scale},
	}})
	if err != nil {
		return 0, nil, 0, err
	}
	a := r.Answers["q"]
	return a.Score, a.Legend, a.Confidence, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

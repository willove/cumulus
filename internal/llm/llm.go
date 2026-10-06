// Package llm 是模型调用面。接口先行：业务流程只认 Completer，
// 离线桩与真提供方都在它后面。
//
// 计费诚实落在 Usage：提供方不报 usage 时 CostKnown=false，
// 上层据此判“成本未知”并停止后续付费调用（流程文法 §二 stage 5）。
package llm

import "context"

// Usage 是一次调用的用量。CostKnown 为假表示上游没报——
// 成本未知不是 0（N/A 与 0 是两件事，评测与台账都守这条）。
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	CostKnown        bool
}

// Request 是一次补全请求。
type Request struct {
	System    string
	Prompt    string
	MaxTokens int
}

// Response 是补全结果。
type Response struct {
	Text  string
	Usage Usage
}

// Completer 补全器。实现方负责把 Usage 填实话；上游不报就留 CostKnown=false。
type Completer interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// ErrNotConfigured 没有配提供方。配了就填，没配就明说——不许静默换桩。
var ErrNotConfigured = errNotConfigured("llm: no completer configured")

type errNotConfigured string

func (e errNotConfigured) Error() string { return string(e) }

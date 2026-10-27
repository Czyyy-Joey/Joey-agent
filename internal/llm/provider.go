// Package llm 定义 Agent 循环所使用的模型 Provider 抽象。
package llm

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// Provider 为一次请求流式返回一个模型响应。
//
// 该序列在 EventCompleted 之后结束，或在产生非 nil error 之后结束。
// 取消 ctx 必须立即中止底层请求；随后 Provider 会
// 产生 ctx.Err()（可能被包装）。
type Provider interface {
	Stream(ctx context.Context, req Request) iter.Seq2[StreamEvent, error]
}

// Request 是与 Provider 无关的采样请求。
type Request struct {
	Instructions string
	Items        []protocol.Item
	Tools        []ToolSpec
}

// ToolSpec 描述向模型声明的函数工具。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// EventKind 标识一个 StreamEvent。
type EventKind int

const (
	EventTextDelta EventKind = iota + 1
	EventReasoningDelta
	// EventToolCall 携带一个已完整组装好的工具调用。
	EventToolCall
	// EventCompleted 是成功流的最后一个事件。
	EventCompleted
)

func (k EventKind) String() string {
	switch k {
	case EventTextDelta:
		return "text_delta"
	case EventReasoningDelta:
		return "reasoning_delta"
	case EventToolCall:
		return "tool_call"
	case EventCompleted:
		return "completed"
	}
	return "unknown"
}

// StreamEvent 是流式模型响应中经过规范化的一部分。
type StreamEvent struct {
	Kind  EventKind
	Delta string
	Call  *protocol.ToolCall
	// Usage 在 EventCompleted 上设置，前提是上游报告了它。
	Usage *protocol.Usage
	// FinishReason 在 EventCompleted 上设置，例如 "stop"、"tool_calls"、"length"。
	FinishReason string
}

// ToolMode 控制是否向模型发送工具。
type ToolMode string

const (
	ToolsNative ToolMode = "native"
	ToolsNone   ToolMode = "none"
)

// ModelInfo 描述网关背后某个模型所支持的能力。
// 不同模型的能力各不相同，因此这些能力需要配置，而不是假定。
type ModelInfo struct {
	Name              string
	ContextWindow     int
	Streaming         bool
	StreamUsage       bool
	ToolMode          ToolMode
	ParallelToolCalls bool
	// MaxTokensParam 是 "max_completion_tokens" 或 "max_tokens"。
	MaxTokensParam  string
	MaxOutputTokens int
}

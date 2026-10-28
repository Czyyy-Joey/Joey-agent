// Package protocol 保存在运行时、API 和 Web 前端之间共享的数据类型。
// 它不依赖其他 Joey 包。
package protocol

import (
	"crypto/rand"
	"encoding/hex"
)

// ItemKind 标识历史条目所代表的内容。
type ItemKind string

const (
	ItemUserMessage  ItemKind = "user_message"
	ItemAgentMessage ItemKind = "agent_message"
	ItemReasoning    ItemKind = "reasoning"
	ItemToolCall     ItemKind = "tool_call"
	ItemToolOutput   ItemKind = "tool_output"
	// ItemTurnAborted 标记上一轮被中断，Text 以用户消息的形式告诉模型。
	ItemTurnAborted ItemKind = "turn_aborted"
	// ItemCompaction 是上下文压缩检查点：Text 是摘要，Compaction.Replacement
	// 是压缩后的模型历史。它本身不发给模型；见 history.Replay。
	ItemCompaction ItemKind = "compaction"
)

// Item 是会话历史中一条模型可见的条目。
// 工具调用及其输出作为独立的条目存储，与模型的传输格式保持一致；
// UI 通过 CallID 把它们合并起来。
type Item struct {
	ID     string      `json:"id"`
	TurnID string      `json:"turn_id"`
	Kind   ItemKind    `json:"kind"`
	Text   string      `json:"text,omitempty"`
	Call   *ToolCall   `json:"call,omitempty"`
	Output *ToolOutput `json:"output,omitempty"`
	// Compaction 只出现在 ItemCompaction 中。
	Compaction *Compaction `json:"compaction,omitempty"`
}

// CompactionReason 说明为什么压缩上下文。
type CompactionReason string

const (
	// CompactPreTurn：轮次开始前，历史加上新输入已超过阈值。
	CompactPreTurn CompactionReason = "pre_turn"
	// CompactMidTurn：轮次中间，工具输出让上下文超过了阈值。
	CompactMidTurn CompactionReason = "mid_turn"
	// CompactOverflow：模型返回上下文超限错误，压缩后重试一次。
	CompactOverflow CompactionReason = "overflow"
	// CompactManual：用户执行 /compact。
	CompactManual CompactionReason = "manual"
	// CompactPostTurn：模型给出最终回答后，上下文达到轮末压缩的水位，
	// 或者有已经完成的后台摘要可以采用。
	CompactPostTurn CompactionReason = "post_turn"
)

// Compaction 描述一次上下文压缩。
type Compaction struct {
	Reason CompactionReason `json:"reason"`
	// Background 表示采用的是后台提前生成的摘要。
	Background   bool `json:"background,omitempty"`
	ItemsBefore  int  `json:"items_before"`
	ItemsAfter   int  `json:"items_after"`
	TokensBefore int  `json:"tokens_before"`
	TokensAfter  int  `json:"tokens_after"`
	// Replacement 是压缩后的模型历史：保留的用户消息加上摘要。事件里不带它。
	Replacement []Item `json:"replacement,omitempty"`
}

// ToolCall 是模型请求的函数调用。
// Arguments 是模型生成的原始 JSON 字符串，可能无效。
type ToolCall struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolOutput 是针对某个 ToolCall 回传给模型的结果。
type ToolOutput struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
	// Diff 是文件改动的 unified diff，随历史保存以便重新打开会话时展示；
	// 转换成模型消息时不包含它。
	Diff string `json:"diff,omitempty"`
}

// Usage 是一次或多次模型请求的 token 统计。
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// Add 把 other 累加到 u 上。
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens += other.TotalTokens
}

// NewID 返回一个随机标识符，例如 "item_3f9a1c0b7d2e"。
func NewID(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

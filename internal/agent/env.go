// Package agent 运行轮次循环：采样模型、执行请求的工具、回传结果，
// 并重复直到模型给出回答。
//
// 它是 Codex 的 run_turn / run_sampling_request
// （codex-rs/core/src/session/turn.rs）的 Go 对应实现。它不关心存储或
// HTTP；所需的一切都通过 TurnEnv 注入。
package agent

import (
	"fmt"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// DefaultMaxSteps 限制每个轮次的采样请求数。
const DefaultMaxSteps = 30

// TurnEnv 承载一个轮次的依赖项。
type TurnEnv struct {
	Provider     llm.Provider
	Router       *tools.Router
	Instructions string
	TurnID       string
	MaxSteps     int
	// ContextWindow 随 token 用量一起报告给客户端。
	ContextWindow int
	// Emit 接收运行时事件；调用方负责填充会话字段。
	Emit func(protocol.Event)
	// Record 在条目加入历史之前被调用，用来持久化它；返回错误会让轮次
	// 失败。因此客户端看到 item.completed 时，该条目一定已经保存。
	Record func(protocol.Item) error
}

// TurnResult 描述轮次如何结束。
type TurnResult struct {
	Status protocol.TurnStatus
	// Items 是按顺序排列的、该轮次产生的新历史项。Items 中的每个
	// 工具调用都有对应的工具输出。
	Items        []protocol.Item
	FinalMessage string
	Usage        protocol.Usage
	Err          error
}

// DefaultInstructions 是基础系统提示词。
func DefaultInstructions(workspace string) string {
	return fmt.Sprintf(`You are Joey, a coding agent running in a local harness.
The user's workspace root is %s. File paths are relative to it.

Use the available tools to inspect files instead of guessing their contents.
Call a tool whenever it helps; you will see its output and can continue.
When you have enough information, answer directly and concisely in the user's language.`, workspace)
}

func toolSpecs(r *tools.Router) []llm.ToolSpec {
	if r == nil {
		return nil
	}
	specs := r.Specs()
	out := make([]llm.ToolSpec, len(specs))
	for i, s := range specs {
		out[i] = llm.ToolSpec{Name: s.Name, Description: s.Description, Parameters: s.Parameters}
	}
	return out
}

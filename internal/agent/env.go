// Package agent 运行轮次循环：采样模型、执行请求的工具、回传结果，
// 并重复直到模型给出回答。
//
// 它是 Codex 的 run_turn / run_sampling_request
// （codex-rs/core/src/session/turn.rs）的 Go 对应实现。它不关心存储或
// HTTP；所需的一切都通过 TurnEnv 注入。
package agent

import (
	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/prompt"
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
	// AutoCompactLimit 是自动压缩的阈值（见 history.AutoCompactLimit）；
	// 0 表示只在模型报告上下文超限后才压缩。
	AutoCompactLimit int
	// PostTurnCompactPercent > 0 时，模型给出最终回答后，如果上下文达到
	// AutoCompactLimit 的这个百分比，就在轮次结束前压缩，让下一轮从较小的
	// 上下文开始。对应 Codex 的 model_post_turn_compact_threshold_percent。
	PostTurnCompactPercent int
	// BackgroundCompactPercent > 0 时，上下文达到 AutoCompactLimit 的这个
	// 百分比后，在 Background 中提前生成摘要。对应 Codex 的
	// async_compaction_threshold_percent。
	BackgroundCompactPercent int
	// Background 是会话的后台压缩器；为 nil 时不做后台压缩。
	Background *Background
	// ToolOutputTokens 是单个工具输出写入历史时保留的 token 数；0 表示
	// 默认值。实际上限还受 AutoCompactLimit 约束（见 history.ToolOutputLimit）。
	ToolOutputTokens int
	// Size 是轮次开始时上游报告的上下文大小，由上一轮的 TurnResult.Context
	// 给出；零值表示用估算。
	Size history.ContextSize
	// Emit 接收运行时事件；调用方负责填充会话字段。
	Emit func(protocol.Event)
	// Record 在条目加入历史之前被调用，用来持久化它；返回错误会让轮次
	// 失败。因此客户端看到 item.completed 时，该条目一定已经保存。
	Record func(protocol.Item) error
	// Approver 处理工具的审批请求；为 nil 时需要审批的操作一律被拒绝。
	Approver tools.Approver
}

// TurnResult 描述轮次如何结束。
type TurnResult struct {
	Status protocol.TurnStatus
	// Items 是按顺序排列的、该轮次产生的新历史项，包括压缩检查点。Items
	// 中的每个工具调用都有对应的工具输出。模型历史由 history.Replay 得到。
	Items        []protocol.Item
	FinalMessage string
	Usage        protocol.Usage
	Err          error
	// Context 是轮次结束时的上下文大小，传给下一轮的 TurnEnv.Size。
	Context history.ContextSize
}

// BaseInstructions 是基础系统提示词，环境上下文和项目指令由 prompt 包追加。
const BaseInstructions = `You are Joey, a coding agent running in a local harness.
File paths are relative to the workspace root shown in <environment_context>.

Use the available tools to inspect files instead of guessing their contents.
Call a tool whenever it helps; you will see its output and can continue.
Prefer search_files to find code, read_file to read it, apply_patch to change part of a file and
write_file only for new files or full rewrites. Read a file before patching it.
exec_command runs shell commands inside a sandbox; use it to build and test your changes.
If the user denies an action, do not repeat it unchanged: adjust the plan or ask.
Follow the project instructions (AGENTS.md / JOEY.md) below when they apply.
When you have enough information, answer directly and concisely in the user's language.`

// DefaultInstructions 返回只有工作目录、没有项目指令的系统提示词。
func DefaultInstructions(workspace string) string {
	return prompt.Build(BaseInstructions, prompt.Env{Workspace: workspace}, nil)
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

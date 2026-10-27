// Package history 维护模型历史的结构不变量。
package history

import "github.com/Czyyy-Joey/joey-agent/internal/protocol"

const (
	// RestartedMessage 是进程在工具调用完成前退出时补上的输出。
	RestartedMessage = "aborted: the process stopped before this tool call finished"
	// MissingMessage 是轮次结束时仍没有输出的调用（例如写库失败）补上的输出。
	MissingMessage = "aborted: no result was recorded for this tool call"
)

// AbortReason 说明轮次为什么被中断。
type AbortReason int

const (
	AbortedByUser AbortReason = iota
	AbortedByShutdown
	AbortedByRestart
)

// AbortMarker 返回追加在被中断轮次末尾的标记条目。它以用户消息的形式
// 发给模型，让模型知道上一轮没有正常结束，对应 Codex 的 <turn_aborted>。
func AbortMarker(turnID string, reason AbortReason) protocol.Item {
	why := "The user interrupted the previous turn on purpose."
	switch reason {
	case AbortedByShutdown:
		why = "The previous turn was stopped because the Joey server shut down."
	case AbortedByRestart:
		why = "The previous turn was cut off because the Joey process stopped unexpectedly."
	}
	return protocol.Item{
		ID:     protocol.NewID("item"),
		TurnID: turnID,
		Kind:   protocol.ItemTurnAborted,
		Text: "<turn_aborted>\n" + why +
			" Its final answer may be missing and tool calls marked \"aborted\" did not complete." +
			" If the user asks to continue, re-check state before acting.\n</turn_aborted>",
	}
}

// Normalize 保证 Chat Completions 要求的配对关系：每个 tool_call 后面
// （在下一条非工具条目之前）恰好有一个对应的 tool_output。
//
//   - 缺失的输出以 content 补上，插在这组工具条目的末尾；
//   - 找不到调用的孤儿输出和重复输出被丢弃。
//
// 它对应 Codex 的 ensure_call_outputs_present（context_manager/normalize.rs）。
// 返回规范化后的新切片和补上的条目；items 本身不会被修改。
func Normalize(items []protocol.Item, content string) (out, added []protocol.Item) {
	out = make([]protocol.Item, 0, len(items))
	var pending []protocol.ToolCall // 本组中尚未得到输出的调用，按出现顺序
	var pendingTurn []string
	flush := func() {
		for i, c := range pending {
			it := protocol.Item{
				ID:     protocol.NewID("item"),
				TurnID: pendingTurn[i],
				Kind:   protocol.ItemToolOutput,
				Output: &protocol.ToolOutput{CallID: c.CallID, Content: content, IsError: true},
			}
			out = append(out, it)
			added = append(added, it)
		}
		pending, pendingTurn = pending[:0], pendingTurn[:0]
	}
	for _, it := range items {
		switch it.Kind {
		case protocol.ItemToolCall:
			if it.Call == nil {
				continue
			}
			pending = append(pending, *it.Call)
			pendingTurn = append(pendingTurn, it.TurnID)
		case protocol.ItemToolOutput:
			if it.Output == nil {
				continue
			}
			i := indexOf(pending, it.Output.CallID)
			if i < 0 {
				continue
			}
			pending = append(pending[:i], pending[i+1:]...)
			pendingTurn = append(pendingTurn[:i], pendingTurn[i+1:]...)
		default:
			flush()
		}
		out = append(out, it)
	}
	flush()
	return out, added
}

func indexOf(calls []protocol.ToolCall, id string) int {
	for i, c := range calls {
		if c.CallID == id {
			return i
		}
	}
	return -1
}

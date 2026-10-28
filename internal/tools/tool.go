// Package tools 定义工具接口、注册表，以及执行模型工具调用的路由器。
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// Tool 是模型可以按名称调用的能力。
type Tool interface {
	Spec() Spec
	// Handle 执行一次调用。返回 *ModelError（或对其包装）表示向模型
	// 报告失败，模型可能会换一种方式重试；其他任何错误都是致命错误，
	// 会导致本轮失败。
	Handle(ctx context.Context, inv Invocation) (Result, error)
}

// Spec 向模型和路由器描述一个工具。
type Spec struct {
	Name        string
	Description string
	// Parameters 是一个 JSON Schema 对象。
	Parameters json.RawMessage
	// ParallelSafe 为 true 的工具可以与其他 ParallelSafe 工具并发运行；
	// 其余工具独占执行（对应 Codex 的 supports_parallel_tool_calls）。
	ParallelSafe bool
	// Timeout 限制单次调用的时长；零值表示使用路由器默认值，NoTimeout
	// 表示不由路由器限时。需要等待审批的工具用 NoTimeout，因为用户考虑
	// 的时间不应算进工具的时限，它们自己限制执行时长。
	Timeout time.Duration
}

// NoTimeout 让路由器不为工具调用设置时限。
const NoTimeout time.Duration = -1

// Invocation 是对工具的一次调用。
type Invocation struct {
	CallID string
	TurnID string
	// ItemID 是这次调用在历史中的条目 id，审批事件用它关联到工具卡片。
	ItemID string
	Tool   string
	Args   json.RawMessage
	// Output 发送一段实时输出（例如命令的 stdout），可以为 nil。可能被
	// 并发调用。
	Output func(chunk string)
	// Approver 向用户请求批准；为 nil 时需要批准的操作一律被拒绝。
	Approver Approver
}

// Stream 发送一段实时输出；Output 为 nil 时什么都不做。
func (inv Invocation) Stream(chunk string) {
	if inv.Output != nil && chunk != "" {
		inv.Output(chunk)
	}
}

// Result 是一次工具执行的结果。
type Result struct {
	Content string
	// IsError 表示操作执行了但没有成功（例如命令以非零状态退出）。和
	// *ModelError 不同，它带着完整的输出。
	IsError bool
	// Diff 是这次调用对文件的改动（unified diff），只用于展示，不发给模型。
	Diff string
}

// ModelError 是回传给模型的工具失败。
type ModelError struct {
	Msg string
}

func (e *ModelError) Error() string { return e.Msg }

// Errorf 返回一个带格式化消息的 *ModelError。
func Errorf(format string, args ...any) error {
	return &ModelError{Msg: fmt.Sprintf(format, args...)}
}

// DecodeArgs 严格地把 JSON 参数解码到 v，拒绝未知字段，
// 从而让拼写错误的参数报告给模型，而不是被忽略。
func DecodeArgs(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return Errorf("invalid arguments: %v", err)
	}
	if dec.More() {
		return Errorf("invalid arguments: trailing data after JSON object")
	}
	return nil
}

// IsModelError 报告 err 是否应当展示给模型。
func IsModelError(err error) bool {
	var me *ModelError
	return errors.As(err, &me)
}

// ApprovalRequest 是工具向用户请求的批准。
type ApprovalRequest struct {
	// Data 描述要做的事。ApprovalID、CallID、Tool、Decision 由审批方填写。
	Data protocol.ApprovalData
	// Keys 标识"同样的操作"：用户选择"本会话都允许"后，Keys 全部已被
	// 允许过的请求不再询问。例如 "exec:go test ./..."、"patch:/abs/main.go"。
	Keys []string
}

// Approver 等待用户对一次请求做出决定，对应 Codex 的 request_command_approval。
// 实现必须在 ctx 结束时返回 DecisionCancelled。
type Approver interface {
	Approve(ctx context.Context, inv Invocation, req ApprovalRequest) protocol.ReviewDecision
}

// RequireApproval 请求批准并把结果转换成错误：批准时返回 nil；拒绝、超时
// 返回 *ModelError，告诉模型操作没有执行；轮次被中断或用户选择中止时返回
// ctx 的错误，路由器据此把调用标记为 aborted。
func (inv Invocation) RequireApproval(ctx context.Context, req ApprovalRequest) error {
	if inv.Approver == nil {
		return Errorf("this action needs the user's approval, but no one can approve it here; it was not performed")
	}
	d := inv.Approver.Approve(ctx, inv, req)
	switch {
	case d.Approves():
		return nil
	case ctx.Err() != nil || d == protocol.DecisionCancelled || d == protocol.DecisionAbort:
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	case d == protocol.DecisionTimedOut:
		return Errorf("no one approved this action in time; it was not performed")
	}
	return Errorf("the user denied this action; it was not performed. Do not retry it unchanged: choose a different approach or ask the user")
}

// Outcome 是路由一次工具调用的结果。
type Outcome struct {
	Output   protocol.ToolOutput
	Status   protocol.ItemStatus
	Duration time.Duration
	// 当处理器以必须结束本轮的方式失败时，会设置 Fatal。
	Fatal error
}

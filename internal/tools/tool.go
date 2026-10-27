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
	// ParallelSafe 为 true 的工具可以与其他 ParallelSafe 工具并发运行。
	ParallelSafe bool
	// Timeout 限制单次调用的时长；零值表示使用路由器默认值。
	Timeout time.Duration
}

// Invocation 是对工具的一次调用。
type Invocation struct {
	CallID string
	TurnID string
	Args   json.RawMessage
}

// Result 是一次成功的工具执行结果。
type Result struct {
	Content string
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

// Outcome 是路由一次工具调用的结果。
type Outcome struct {
	Output   protocol.ToolOutput
	Status   protocol.ItemStatus
	Duration time.Duration
	// 当处理器以必须结束本轮的方式失败时，会设置 Fatal。
	Fatal error
}

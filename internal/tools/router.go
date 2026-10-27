package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// DefaultTimeout 为未在 Spec 中设置超时的工具调用提供时限。
const DefaultTimeout = 60 * time.Second

// AbortedMessage 是因取消而中断的调用所记录的输出。
// 历史中的每次调用都需要一个输出，否则下一个请求会被拒绝。
const AbortedMessage = "aborted: the turn was interrupted before this tool call finished"

// Router 把工具调用分发给已注册的工具。
type Router struct {
	tools map[string]Tool
}

// NewRouter 注册工具，拒绝重复或空的名称。
func NewRouter(ts ...Tool) (*Router, error) {
	r := &Router{tools: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		name := t.Spec().Name
		if name == "" {
			return nil, errors.New("tools: tool with empty name")
		}
		if _, dup := r.tools[name]; dup {
			return nil, fmt.Errorf("tools: duplicate tool %q", name)
		}
		r.tools[name] = t
	}
	return r, nil
}

// Specs 返回所有已注册工具的 Spec，按名称排序。
func (r *Router) Specs() []Spec {
	specs := make([]Spec, 0, len(r.tools))
	for _, t := range r.tools {
		specs = append(specs, t.Spec())
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

// Execute 执行一次调用。它总会为该调用返回一个输出，包括
// 未知工具、无效参数、panic、超时和取消的情况。
func (r *Router) Execute(ctx context.Context, turnID string, call protocol.ToolCall) Outcome {
	start := time.Now()
	out := func(content string, status protocol.ItemStatus, fatal error) Outcome {
		return Outcome{
			Output:   protocol.ToolOutput{CallID: call.CallID, Content: content, IsError: status != protocol.ItemCompleted},
			Status:   status,
			Duration: time.Since(start),
			Fatal:    fatal,
		}
	}

	if ctx.Err() != nil {
		return out(AbortedMessage, protocol.ItemAborted, nil)
	}
	t, ok := r.tools[call.Name]
	if !ok {
		return out(fmt.Sprintf("unknown tool %q; available tools: %s", call.Name, r.names()), protocol.ItemFailed, nil)
	}
	if !json.Valid([]byte(call.Arguments)) {
		return out("invalid arguments: not valid JSON (the output may have been truncated); retry with a complete JSON object", protocol.ItemFailed, nil)
	}

	spec := t.Spec()
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := safeHandle(callCtx, t, Invocation{CallID: call.CallID, TurnID: turnID, Args: json.RawMessage(call.Arguments)})
	switch {
	case ctx.Err() != nil:
		return out(AbortedMessage, protocol.ItemAborted, nil)
	case err == nil:
		return out(res.Content, protocol.ItemCompleted, nil)
	case IsModelError(err):
		return out(err.Error(), protocol.ItemFailed, nil)
	case errors.Is(err, context.DeadlineExceeded) || callCtx.Err() != nil:
		return out(fmt.Sprintf("tool %q timed out after %s", call.Name, timeout), protocol.ItemFailed, nil)
	default:
		return out("internal tool error", protocol.ItemFailed, fmt.Errorf("tool %q: %w", call.Name, err))
	}
}

func (r *Router) names() string {
	s := ""
	for i, spec := range r.Specs() {
		if i > 0 {
			s += ", "
		}
		s += spec.Name
	}
	return s
}

// safeHandle 把处理器的 panic 转换成模型可见的错误，
// 这样一个有缺陷的工具就不会让进程崩溃。
func safeHandle(ctx context.Context, t Tool, inv Invocation) (res Result, err error) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("tool panicked", "tool", t.Spec().Name, "panic", p, "stack", string(debug.Stack()))
			err = &ModelError{Msg: fmt.Sprintf("tool %q crashed: %v", t.Spec().Name, p)}
		}
	}()
	return t.Handle(ctx, inv)
}

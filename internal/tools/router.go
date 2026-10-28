package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
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

// Call 是一次要执行的工具调用及其运行环境。
type Call struct {
	protocol.ToolCall
	TurnID string
	ItemID string
	// Output 接收实时输出，可以为 nil。
	Output func(chunk string)
	// Approver 处理审批请求，可以为 nil（需要审批的操作会被拒绝）。
	Approver Approver
}

// Execute 执行一次调用，不带实时输出和审批方。
func (r *Router) Execute(ctx context.Context, turnID string, call protocol.ToolCall) Outcome {
	return r.Run(ctx, Call{ToolCall: call, TurnID: turnID})
}

// ParallelSafe 报告名为 name 的工具能否与其他并行安全的工具同时执行。
// 未知工具视为并行安全：它们立即失败，不会访问任何资源。
func (r *Router) ParallelSafe(name string) bool {
	t, ok := r.tools[name]
	return !ok || t.Spec().ParallelSafe
}

// RunAll 按模型给出的顺序执行 calls。每个调用开始前调用 started（可以为
// nil），完成后按调用顺序调用 done；两者都不会并发调用。连续的并行安全调用同时执行；其他调用独占执行，并且要等它
// 之前的调用全部结束，它之后的调用也要等它结束。这与 Codex 用读写锁
// （parallel.rs 的 parallel_execution）的效果相同，但保留了顺序：模型先
// 写文件再读文件时，读一定看到写之后的内容。
//
// done 返回非 nil 错误（通常是保存失败）或某个调用返回致命错误时，尚未
// 开始的调用不再执行，以 skipped 结果交给 done；已经开始的调用被取消。
func (r *Router) RunAll(ctx context.Context, calls []Call, started func(i int), done func(i int, o Outcome) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		mu      sync.Mutex
		stopErr error
	)
	finish := func(i int, o Outcome) {
		mu.Lock()
		defer mu.Unlock()
		if stopErr == nil && o.Fatal != nil {
			stopErr = o.Fatal
			cancel(o.Fatal)
		}
		if err := done(i, o); err != nil && stopErr == nil {
			stopErr = err
			cancel(err)
		}
	}
	stopped := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return stopErr != nil
	}
	for i := 0; i < len(calls); {
		j := i + 1
		if r.ParallelSafe(calls[i].Name) {
			for j < len(calls) && r.ParallelSafe(calls[j].Name) {
				j++
			}
		}
		if stopped() {
			for k := i; k < len(calls); k++ {
				finish(k, skipped(calls[k]))
			}
			break
		}
		results := make([]chan Outcome, j-i)
		for k := i; k < j; k++ {
			if started != nil {
				mu.Lock()
				started(k)
				mu.Unlock()
			}
			ch := make(chan Outcome, 1)
			results[k-i] = ch
			go func() { ch <- r.Run(ctx, calls[k]) }()
		}
		// 按调用顺序交给 done，历史里的输出顺序因此是确定的。
		for k := i; k < j; k++ {
			finish(k, <-results[k-i])
		}
		i = j
	}
	mu.Lock()
	defer mu.Unlock()
	return stopErr
}

// SkippedMessage 是因前面的调用致命失败而没有执行的调用的输出。
const SkippedMessage = "skipped: an earlier tool call failed fatally"

func skipped(c Call) Outcome {
	return Outcome{
		Output: protocol.ToolOutput{CallID: c.CallID, Content: SkippedMessage, IsError: true},
		Status: protocol.ItemAborted,
	}
}

// Run 执行一次调用。它总会为该调用返回一个输出，包括未知工具、无效参数、
// panic、超时和取消的情况。
func (r *Router) Run(ctx context.Context, call Call) Outcome {
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
	if err := Validate(spec.Parameters, json.RawMessage(call.Arguments)); err != nil {
		return out(err.Error(), protocol.ItemFailed, nil)
	}
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	callCtx, cancel := ctx, context.CancelFunc(func() {})
	if timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	inv := Invocation{
		CallID: call.CallID, TurnID: call.TurnID, ItemID: call.ItemID, Tool: call.Name,
		Args: json.RawMessage(call.Arguments), Output: call.Output, Approver: call.Approver,
	}
	res, err := safeHandle(callCtx, t, inv)
	switch {
	case ctx.Err() != nil:
		return out(AbortedMessage, protocol.ItemAborted, nil)
	case err == nil && res.IsError:
		return out(res.Content, protocol.ItemFailed, nil)
	case err == nil:
		o := out(res.Content, protocol.ItemCompleted, nil)
		o.Output.Diff = res.Diff
		return o
	case IsModelError(err):
		return out(err.Error(), protocol.ItemFailed, nil)
	case errors.Is(err, context.Canceled):
		// 工具自己报告了取消（例如用户在审批时选择了中止），而轮次的 ctx
		// 还没来得及结束。
		return out(AbortedMessage, protocol.ItemAborted, nil)
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

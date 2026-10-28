package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

type fakeTool struct {
	spec Spec
	fn   func(ctx context.Context, inv Invocation) (Result, error)
}

func (f *fakeTool) Spec() Spec { return f.spec }
func (f *fakeTool) Handle(ctx context.Context, inv Invocation) (Result, error) {
	return f.fn(ctx, inv)
}

func newFake(name string, fn func(context.Context, Invocation) (Result, error)) *fakeTool {
	return &fakeTool{spec: Spec{Name: name}, fn: fn}
}

func TestNewRouterRejectsDuplicates(t *testing.T) {
	ok := func(context.Context, Invocation) (Result, error) { return Result{}, nil }
	if _, err := NewRouter(newFake("a", ok), newFake("a", ok)); err == nil {
		t.Error("expected duplicate error")
	}
	if _, err := NewRouter(newFake("", ok)); err == nil {
		t.Error("expected empty-name error")
	}
}

func TestRouterExecute(t *testing.T) {
	r, err := NewRouter(
		newFake("echo", func(_ context.Context, inv Invocation) (Result, error) {
			var a struct{ Msg string }
			if err := DecodeArgs(inv.Args, &a); err != nil {
				return Result{}, err
			}
			return Result{Content: a.Msg}, nil
		}),
		newFake("model_err", func(context.Context, Invocation) (Result, error) {
			return Result{}, Errorf("file %s missing", "x")
		}),
		newFake("panics", func(context.Context, Invocation) (Result, error) { panic("kaboom") }),
		newFake("fatal", func(context.Context, Invocation) (Result, error) {
			return Result{}, errors.New("disk on fire")
		}),
		&fakeTool{spec: Spec{Name: "slow", Timeout: 20 * time.Millisecond}, fn: func(ctx context.Context, _ Invocation) (Result, error) {
			<-ctx.Done()
			return Result{}, ctx.Err()
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, tool, args string
		status           protocol.ItemStatus
		contains         string
		fatal            bool
	}{
		{"success", "echo", `{"Msg":"hi"}`, protocol.ItemCompleted, "hi", false},
		{"unknown tool", "nope", `{}`, protocol.ItemFailed, `unknown tool "nope"; available tools: echo, fatal`, false},
		{"invalid json", "echo", `{"Msg":`, protocol.ItemFailed, "not valid JSON", false},
		{"unknown field", "echo", `{"msg2":1}`, protocol.ItemFailed, "unknown field", false},
		{"model error", "model_err", `{}`, protocol.ItemFailed, "file x missing", false},
		{"panic", "panics", `{}`, protocol.ItemFailed, `tool "panics" crashed: kaboom`, false},
		{"fatal", "fatal", `{}`, protocol.ItemFailed, "internal tool error", true},
		{"timeout", "slow", `{}`, protocol.ItemFailed, "timed out after 20ms", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := r.Execute(context.Background(), "turn", protocol.ToolCall{CallID: "c1", Name: tt.tool, Arguments: tt.args})
			if o.Status != tt.status {
				t.Errorf("status = %s, want %s", o.Status, tt.status)
			}
			if o.Output.CallID != "c1" {
				t.Errorf("call id = %q", o.Output.CallID)
			}
			if !strings.Contains(o.Output.Content, tt.contains) {
				t.Errorf("content = %q, want %q", o.Output.Content, tt.contains)
			}
			if o.Output.IsError != (tt.status != protocol.ItemCompleted) {
				t.Errorf("IsError = %v", o.Output.IsError)
			}
			if (o.Fatal != nil) != tt.fatal {
				t.Errorf("fatal = %v", o.Fatal)
			}
		})
	}
}

func TestRouterCancellation(t *testing.T) {
	started := make(chan struct{})
	r, _ := NewRouter(newFake("block", func(ctx context.Context, _ Invocation) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	o := r.Execute(ctx, "t", protocol.ToolCall{CallID: "c", Name: "block", Arguments: "{}"})
	if o.Status != protocol.ItemAborted || o.Output.Content != AbortedMessage {
		t.Errorf("outcome = %+v", o)
	}

	// 已取消的 context：工具永远不会被调用。
	o = r.Execute(ctx, "t", protocol.ToolCall{CallID: "d", Name: "block", Arguments: "{}"})
	if o.Status != protocol.ItemAborted {
		t.Errorf("status = %s", o.Status)
	}
}

// TestRunAllParallelAndExclusive：连续的并行安全调用同时执行；独占调用
// 等前面的全部结束、后面的等它结束；done 按调用顺序调用。
func TestRunAllParallelAndExclusive(t *testing.T) {
	var mu sync.Mutex
	var log []string
	note := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	running := make(chan struct{}, 8)
	read := &fakeTool{spec: Spec{Name: "read", ParallelSafe: true}, fn: func(_ context.Context, inv Invocation) (Result, error) {
		running <- struct{}{}
		// 两个 read 都开始之后才结束，证明它们是并行的。
		deadline := time.After(2 * time.Second)
		for len(running) < 2 {
			select {
			case <-deadline:
				return Result{}, errors.New("reads did not overlap")
			case <-time.After(time.Millisecond):
			}
		}
		note("read " + inv.CallID)
		return Result{Content: inv.CallID}, nil
	}}
	write := &fakeTool{spec: Spec{Name: "write"}, fn: func(_ context.Context, inv Invocation) (Result, error) {
		for len(running) > 0 {
			<-running
		}
		note("write " + inv.CallID)
		return Result{Content: inv.CallID}, nil
	}}
	r, _ := NewRouter(read, write)
	calls := []Call{
		{ToolCall: protocol.ToolCall{CallID: "r1", Name: "read", Arguments: "{}"}},
		{ToolCall: protocol.ToolCall{CallID: "r2", Name: "read", Arguments: "{}"}},
		{ToolCall: protocol.ToolCall{CallID: "w1", Name: "write", Arguments: "{}"}},
		{ToolCall: protocol.ToolCall{CallID: "r3", Name: "read", Arguments: "{}"}},
		{ToolCall: protocol.ToolCall{CallID: "r4", Name: "read", Arguments: "{}"}},
	}
	var started, done []string
	err := r.RunAll(context.Background(), calls, func(i int) { started = append(started, calls[i].CallID) }, func(i int, o Outcome) error {
		if o.Status != protocol.ItemCompleted {
			t.Errorf("%s: %+v", calls[i].CallID, o)
		}
		done = append(done, o.Output.CallID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "r1 r2 w1 r3 r4"; strings.Join(done, " ") != want || strings.Join(started, " ") != want {
		t.Errorf("started=%v done=%v", started, done)
	}
	// write 一定在 r1、r2 之后、r3、r4 之前。
	idx := map[string]int{}
	for i, s := range log {
		idx[s] = i
	}
	if !(idx["write w1"] > idx["read r1"] && idx["write w1"] > idx["read r2"] && idx["write w1"] < idx["read r3"] && idx["write w1"] < idx["read r4"]) {
		t.Errorf("order = %v", log)
	}
}

// TestRunAllStopsAfterFatal：致命错误或 done 返回错误后，后面的调用被跳过，
// 但每个调用仍然得到一个输出。
func TestRunAllStopsAfterFatal(t *testing.T) {
	ran := 0
	ok := &fakeTool{spec: Spec{Name: "ok"}, fn: func(context.Context, Invocation) (Result, error) { ran++; return Result{}, nil }}
	fatal := &fakeTool{spec: Spec{Name: "fatal"}, fn: func(context.Context, Invocation) (Result, error) { return Result{}, errors.New("disk on fire") }}
	r, _ := NewRouter(ok, fatal)
	calls := []Call{
		{ToolCall: protocol.ToolCall{CallID: "a", Name: "ok", Arguments: "{}"}},
		{ToolCall: protocol.ToolCall{CallID: "b", Name: "fatal", Arguments: "{}"}},
		{ToolCall: protocol.ToolCall{CallID: "c", Name: "ok", Arguments: "{}"}},
	}
	var outs []Outcome
	err := r.RunAll(context.Background(), calls, nil, func(_ int, o Outcome) error { outs = append(outs, o); return nil })
	if err == nil || !strings.Contains(err.Error(), "disk on fire") || len(outs) != 3 || ran != 1 {
		t.Fatalf("err=%v outs=%+v ran=%d", err, outs, ran)
	}
	if outs[2].Output.Content != SkippedMessage || outs[2].Status != protocol.ItemAborted {
		t.Errorf("skipped = %+v", outs[2])
	}

	saveErr := errors.New("save failed")
	n := 0
	err = r.RunAll(context.Background(), []Call{calls[0], calls[2]}, nil, func(int, Outcome) error { n++; return saveErr })
	if !errors.Is(err, saveErr) || n != 2 || ran != 2 {
		t.Errorf("err=%v n=%d ran=%d", err, n, ran)
	}
}

func TestRouterValidatesAgainstSchema(t *testing.T) {
	called := false
	tool := &fakeTool{spec: Spec{Name: "t", Parameters: []byte(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)},
		fn: func(context.Context, Invocation) (Result, error) { called = true; return Result{}, nil }}
	r, _ := NewRouter(tool)
	o := r.Execute(context.Background(), "t", protocol.ToolCall{CallID: "c", Name: "t", Arguments: `{"pth":"x"}`})
	if called || o.Status != protocol.ItemFailed || !strings.Contains(o.Output.Content, `missing required field "path"`) || !strings.Contains(o.Output.Content, `unknown field "pth"`) {
		t.Errorf("outcome = %+v, called=%v", o, called)
	}
}

func TestResultIsErrorAndNoTimeout(t *testing.T) {
	tool := &fakeTool{spec: Spec{Name: "t", Timeout: NoTimeout}, fn: func(ctx context.Context, _ Invocation) (Result, error) {
		if _, ok := ctx.Deadline(); ok {
			return Result{}, errors.New("NoTimeout tool got a deadline")
		}
		return Result{Content: "exit 1", IsError: true}, nil
	}}
	r, _ := NewRouter(tool)
	o := r.Execute(context.Background(), "t", protocol.ToolCall{CallID: "c", Name: "t", Arguments: `{}`})
	if o.Status != protocol.ItemFailed || !o.Output.IsError || o.Output.Content != "exit 1" || o.Fatal != nil {
		t.Errorf("outcome = %+v", o)
	}
}

type fixedApprover struct {
	d    protocol.ReviewDecision
	seen []ApprovalRequest
}

func (f *fixedApprover) Approve(_ context.Context, _ Invocation, req ApprovalRequest) protocol.ReviewDecision {
	f.seen = append(f.seen, req)
	return f.d
}

func TestRequireApproval(t *testing.T) {
	req := ApprovalRequest{Data: protocol.ApprovalData{Kind: protocol.ApprovalExec, Command: "x"}}
	if err := (Invocation{}).RequireApproval(context.Background(), req); !IsModelError(err) {
		t.Errorf("no approver = %v", err)
	}
	for d, check := range map[protocol.ReviewDecision]func(error) bool{
		protocol.DecisionApproved:           func(err error) bool { return err == nil },
		protocol.DecisionApprovedForSession: func(err error) bool { return err == nil },
		protocol.DecisionDenied:             func(err error) bool { return IsModelError(err) && strings.Contains(err.Error(), "denied") },
		protocol.DecisionTimedOut:           func(err error) bool { return IsModelError(err) && strings.Contains(err.Error(), "in time") },
		protocol.DecisionAbort:              func(err error) bool { return errors.Is(err, context.Canceled) },
		protocol.DecisionCancelled:          func(err error) bool { return errors.Is(err, context.Canceled) },
	} {
		a := &fixedApprover{d: d}
		if err := (Invocation{Approver: a}).RequireApproval(context.Background(), req); !check(err) || len(a.seen) != 1 {
			t.Errorf("%s: err = %v", d, err)
		}
	}
	// 工具因审批被中止时，路由器把调用标为 aborted 而不是致命错误。
	tool := &fakeTool{spec: Spec{Name: "t"}, fn: func(ctx context.Context, inv Invocation) (Result, error) {
		return Result{}, inv.RequireApproval(ctx, req)
	}}
	r, _ := NewRouter(tool)
	o := r.Run(context.Background(), Call{ToolCall: protocol.ToolCall{CallID: "c", Name: "t", Arguments: "{}"}, Approver: &fixedApprover{d: protocol.DecisionAbort}})
	if o.Status != protocol.ItemAborted || o.Fatal != nil {
		t.Errorf("abort outcome = %+v", o)
	}
}

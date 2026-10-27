package tools

import (
	"context"
	"errors"
	"strings"
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

package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

type echoTool struct{}

func (echoTool) Spec() tools.Spec { return tools.Spec{Name: "echo", Description: "echo"} }
func (echoTool) Handle(_ context.Context, inv tools.Invocation) (tools.Result, error) {
	return tools.Result{Content: "echo:" + string(inv.Args)}, nil
}

// blockTool 阻塞直到被取消，并报告它何时启动。
type blockTool struct{ started chan struct{} }

func (b blockTool) Spec() tools.Spec { return tools.Spec{Name: "block"} }
func (b blockTool) Handle(ctx context.Context, _ tools.Invocation) (tools.Result, error) {
	close(b.started)
	<-ctx.Done()
	return tools.Result{}, ctx.Err()
}

type fatalTool struct{}

func (fatalTool) Spec() tools.Spec { return tools.Spec{Name: "fatal"} }
func (fatalTool) Handle(context.Context, tools.Invocation) (tools.Result, error) {
	return tools.Result{}, errors.New("broken")
}

type recorder struct {
	mu     sync.Mutex
	events []protocol.Event
}

func (r *recorder) emit(e protocol.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// types 将事件表示为 "type:kind:status" 以便紧凑断言。
func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		s := string(e.Type)
		if d, ok := e.Data.(protocol.ItemData); ok {
			s += ":" + string(d.Kind)
			if e.Type != protocol.EventItemDelta {
				s += ":" + string(d.Status)
			}
		}
		out = append(out, s)
	}
	return out
}

func newEnv(t *testing.T, p *llmtest.Scripted, rec *recorder, extra ...tools.Tool) TurnEnv {
	t.Helper()
	r, err := tools.NewRouter(append([]tools.Tool{echoTool{}}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}
	return TurnEnv{Provider: p, Router: r, Instructions: "sys", TurnID: "turn_1", MaxSteps: 5, Emit: rec.emit}
}

func kinds(items []protocol.Item) []protocol.ItemKind {
	out := make([]protocol.ItemKind, len(items))
	for i, it := range items {
		out[i] = it.Kind
	}
	return out
}

func TestTurnAnswersWithoutTools(t *testing.T) {
	p := llmtest.NewScripted(llmtest.Step{Text: "Hello there", Chunk: 4, Usage: &protocol.Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}})
	rec := &recorder{}
	res := RunTurn(context.Background(), newEnv(t, p, rec), nil, "hi")

	if res.Status != protocol.TurnCompleted || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	if res.FinalMessage != "Hello there" {
		t.Errorf("final = %q", res.FinalMessage)
	}
	if got := kinds(res.Items); !slices.Equal(got, []protocol.ItemKind{protocol.ItemUserMessage, protocol.ItemAgentMessage}) {
		t.Errorf("items = %v", got)
	}
	want := []string{
		"item.completed:user_message:completed",
		"item.started:agent_message:in_progress",
		"item.delta:agent_message", "item.delta:agent_message", "item.delta:agent_message",
		"item.completed:agent_message:completed",
		"token.usage",
	}
	if got := rec.types(); !slices.Equal(got, want) {
		t.Errorf("events =\n%v\nwant\n%v", got, want)
	}
	if res.Usage.TotalTokens != 13 {
		t.Errorf("usage = %+v", res.Usage)
	}
	req := p.Requests()[0]
	if req.Instructions != "sys" || len(req.Tools) != 1 || req.Tools[0].Name != "echo" {
		t.Errorf("request = %+v", req)
	}
}

func TestTurnToolLoop(t *testing.T) {
	p := llmtest.NewScripted(
		llmtest.Step{Text: "Checking.", Calls: []llmtest.StepCall{{ID: "c1", Name: "echo", Arguments: []byte(`{"n":1}`)}}},
		llmtest.Step{Calls: []llmtest.StepCall{
			{ID: "c2", Name: "echo", Arguments: []byte(`{"n":2}`)},
			{ID: "c3", Name: "missing_tool"},
		}},
		llmtest.Step{Text: "All done."},
	)
	rec := &recorder{}
	history := []protocol.Item{{ID: "old", Kind: protocol.ItemUserMessage, Text: "earlier"}}
	res := RunTurn(context.Background(), newEnv(t, p, rec), history, "go")

	if res.Status != protocol.TurnCompleted || res.FinalMessage != "All done." {
		t.Fatalf("result = %+v", res)
	}
	wantKinds := []protocol.ItemKind{
		protocol.ItemUserMessage,
		protocol.ItemAgentMessage, protocol.ItemToolCall, protocol.ItemToolOutput,
		protocol.ItemToolCall, protocol.ItemToolCall, protocol.ItemToolOutput, protocol.ItemToolOutput,
		protocol.ItemAgentMessage,
	}
	if got := kinds(res.Items); !slices.Equal(got, wantKinds) {
		t.Errorf("items = %v", got)
	}
	if len(history) != 1 {
		t.Error("input history was modified")
	}

	reqs := p.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d", len(reqs))
	}
	// 第二个请求包含旧历史、用户输入和第一轮工具调用。
	second := reqs[1].Items
	if second[0].ID != "old" || second[1].Text != "go" {
		t.Errorf("second request starts with %+v", second[:2])
	}
	last := second[len(second)-1]
	if last.Kind != protocol.ItemToolOutput || last.Output.Content != `echo:{"n":1}` {
		t.Errorf("second request ends with %+v", last)
	}
	// 未知工具会报告给模型，而不是致命错误。
	third := reqs[2].Items
	out := third[len(third)-1].Output
	if !out.IsError || !strings.Contains(out.Content, "unknown tool") || out.CallID != "c3" {
		t.Errorf("unknown tool output = %+v", out)
	}
}

func TestTurnMaxSteps(t *testing.T) {
	p := llmtest.NewScripted(llmtest.Step{Calls: []llmtest.StepCall{{Name: "echo"}}})
	p.Loop = true
	env := newEnv(t, p, &recorder{})
	env.MaxSteps = 3
	res := RunTurn(context.Background(), env, nil, "loop forever")
	if res.Status != protocol.TurnMaxSteps || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}
	if n := len(p.Requests()); n != 3 {
		t.Errorf("requests = %d", n)
	}
	assertPaired(t, res.Items)
}

func TestTurnProviderError(t *testing.T) {
	p := llmtest.NewScripted(llmtest.Step{Text: "partial", Error: "upstream exploded"})
	rec := &recorder{}
	res := RunTurn(context.Background(), newEnv(t, p, rec), nil, "x")
	if res.Status != protocol.TurnFailed || res.Err == nil || !strings.Contains(res.Err.Error(), "upstream exploded") {
		t.Fatalf("result = %+v", res)
	}
	// 部分消息会显示为失败，但不会进入历史。
	if got := kinds(res.Items); !slices.Equal(got, []protocol.ItemKind{protocol.ItemUserMessage}) {
		t.Errorf("items = %v", got)
	}
	types := rec.types()
	if !slices.Contains(types, "item.completed:agent_message:failed") || !slices.Contains(types, "error") {
		t.Errorf("events = %v", types)
	}
}

func TestTurnScriptExhausted(t *testing.T) {
	res := RunTurn(context.Background(), newEnv(t, llmtest.NewScripted(), &recorder{}), nil, "x")
	if res.Status != protocol.TurnFailed || !errors.Is(res.Err, llmtest.ErrScriptExhausted) {
		t.Fatalf("result = %+v", res)
	}
}

func TestTurnInterruptedWhileStreaming(t *testing.T) {
	p := llmtest.NewScripted(llmtest.Step{Text: "thinking...", Hang: true})
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	res := RunTurn(ctx, newEnv(t, p, rec), nil, "x")
	if res.Status != protocol.TurnInterrupted || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	if time.Since(start) > time.Second {
		t.Error("interrupt was slow")
	}
	if !slices.Contains(rec.types(), "item.completed:agent_message:interrupted") {
		t.Errorf("events = %v", rec.types())
	}
	if got := kinds(res.Items); !slices.Equal(got, []protocol.ItemKind{protocol.ItemUserMessage}) {
		t.Errorf("items = %v", got)
	}
}

func TestTurnInterruptedDuringTool(t *testing.T) {
	bt := blockTool{started: make(chan struct{})}
	p := llmtest.NewScripted(llmtest.Step{Calls: []llmtest.StepCall{{ID: "c1", Name: "block"}, {ID: "c2", Name: "echo"}}})
	rec := &recorder{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-bt.started; cancel() }()

	res := RunTurn(ctx, newEnv(t, p, rec, bt), nil, "x")
	if res.Status != protocol.TurnInterrupted {
		t.Fatalf("result = %+v", res)
	}
	assertPaired(t, res.Items)
	for _, it := range res.Items {
		if it.Kind == protocol.ItemToolOutput && it.Output.Content != tools.AbortedMessage {
			t.Errorf("output %s = %q, want aborted", it.Output.CallID, it.Output.Content)
		}
	}
	if n := len(p.Requests()); n != 1 {
		t.Errorf("model was called %d times after interrupt", n)
	}
}

func TestTurnFatalToolError(t *testing.T) {
	p := llmtest.NewScripted(llmtest.Step{Calls: []llmtest.StepCall{{ID: "c1", Name: "fatal"}, {ID: "c2", Name: "echo"}}})
	res := RunTurn(context.Background(), newEnv(t, p, &recorder{}, fatalTool{}), nil, "x")
	if res.Status != protocol.TurnFailed || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}
	assertPaired(t, res.Items)
}

func TestTurnLengthWarning(t *testing.T) {
	p := llmtest.NewScripted(llmtest.Step{Text: "cut"})
	// 包装 scripted provider 以改写 finish reason。
	rec := &recorder{}
	env := newEnv(t, p, rec)
	env.Provider = lengthProvider{p}
	res := RunTurn(context.Background(), env, nil, "x")
	if res.Status != protocol.TurnCompleted {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Contains(rec.types(), "warning") {
		t.Errorf("events = %v", rec.types())
	}
}

// assertPaired 检查每个工具调用之后恰好有一个输出。
func assertPaired(t *testing.T, items []protocol.Item) {
	t.Helper()
	open := map[string]bool{}
	for _, it := range items {
		switch it.Kind {
		case protocol.ItemToolCall:
			open[it.Call.CallID] = true
		case protocol.ItemToolOutput:
			if !open[it.Output.CallID] {
				t.Errorf("output %s has no preceding call", it.Output.CallID)
			}
			delete(open, it.Output.CallID)
		}
	}
	for id := range open {
		t.Errorf("call %s has no output", id)
	}
}

func TestTurnRecordsBeforeCompletedEvent(t *testing.T) {
	p := llmtest.NewScripted(
		llmtest.Step{Text: "Checking.", Calls: []llmtest.StepCall{{ID: "c1", Name: "echo"}}},
		llmtest.Step{Text: "Done."},
	)
	var mu sync.Mutex
	var log []string
	env := newEnv(t, p, &recorder{})
	env.Emit = func(e protocol.Event) {
		if e.Type == protocol.EventItemCompleted {
			mu.Lock()
			log = append(log, "event:"+string(e.Data.(protocol.ItemData).Kind))
			mu.Unlock()
		}
	}
	env.Record = func(it protocol.Item) error {
		mu.Lock()
		log = append(log, "save:"+string(it.Kind))
		mu.Unlock()
		return nil
	}
	res := RunTurn(context.Background(), env, nil, "hi")
	if res.Status != protocol.TurnCompleted {
		t.Fatalf("result = %+v", res)
	}
	want := []string{
		"save:user_message", "event:user_message",
		"save:agent_message", "event:agent_message",
		"save:tool_call", "save:tool_output", "event:tool_call",
		"save:agent_message", "event:agent_message",
	}
	if !slices.Equal(log, want) {
		t.Errorf("order =\n%v\nwant\n%v", log, want)
	}
}

func TestTurnRecordFailure(t *testing.T) {
	for _, failOn := range []protocol.ItemKind{protocol.ItemUserMessage, protocol.ItemAgentMessage, protocol.ItemToolCall, protocol.ItemToolOutput} {
		t.Run(string(failOn), func(t *testing.T) {
			p := llmtest.NewScripted(
				llmtest.Step{Calls: []llmtest.StepCall{{ID: "c1", Name: "echo"}}},
				llmtest.Step{Text: "Done."},
			)
			rec := &recorder{}
			env := newEnv(t, p, rec)
			env.Record = func(it protocol.Item) error {
				if it.Kind == failOn {
					return errors.New("disk full")
				}
				return nil
			}
			res := RunTurn(context.Background(), env, nil, "hi")
			if res.Status != protocol.TurnFailed || res.Err == nil || !strings.Contains(res.Err.Error(), "disk full") {
				t.Fatalf("result = %+v", res)
			}
			for _, it := range res.Items {
				if it.Kind == failOn {
					t.Errorf("unsaved %s item is in the result", failOn)
				}
			}
			for _, e := range rec.events {
				if d, ok := e.Data.(protocol.ItemData); ok && e.Type == protocol.EventItemCompleted && d.Kind == failOn && d.Status == protocol.ItemCompleted {
					t.Errorf("completed event for unsaved %s item", failOn)
				}
			}
		})
	}
}

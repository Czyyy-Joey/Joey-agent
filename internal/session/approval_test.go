package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// gatedTool 每次执行都请求批准，批准后返回 "ran"。
type gatedTool struct{ runs *int }

func (g gatedTool) Spec() tools.Spec {
	return tools.Spec{Name: "gated", Timeout: tools.NoTimeout}
}
func (g gatedTool) Handle(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	var a struct{ Cmd string }
	_ = json.Unmarshal(inv.Args, &a)
	req := tools.ApprovalRequest{Data: protocol.ApprovalData{Kind: protocol.ApprovalExec, Command: a.Cmd}, Keys: []string{"exec:" + a.Cmd}}
	if err := inv.RequireApproval(ctx, req); err != nil {
		return tools.Result{}, err
	}
	*g.runs++
	return tools.Result{Content: "ran " + a.Cmd}, nil
}

func gatedCall(id, cmd string) llmtest.Step {
	return llmtest.Step{Calls: []llmtest.StepCall{{ID: id, Name: "gated", Arguments: json.RawMessage(`{"Cmd":"` + cmd + `"}`)}}}
}

func newApprovalManager(t *testing.T, timeout time.Duration, steps ...llmtest.Step) (*Manager, *llmtest.Scripted, *int) {
	t.Helper()
	runs := new(int)
	p := llmtest.NewScripted(steps...)
	m, err := NewManager(Runtime{
		Provider: p, ApprovalTimeout: timeout,
		Tools: func(ToolEnv) (*tools.Router, error) { return tools.NewRouter(gatedTool{runs}) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(2 * time.Second) })
	return m, p, runs
}

// answer 等待下一个 approval.requested，然后用 d 回答它。
func answer(t *testing.T, s *Session, sub *Subscription, d protocol.ReviewDecision) protocol.ApprovalData {
	t.Helper()
	for {
		select {
		case e := <-sub.C:
			if e.Type != protocol.EventApprovalRequested {
				continue
			}
			data := e.Data.(protocol.ApprovalData)
			if e.ItemID == "" || data.CallID == "" || data.Tool != "gated" {
				t.Errorf("approval event = %+v", e)
			}
			if d != "" {
				if err := s.Resolve(data.ApprovalID, d); err != nil {
					t.Fatal(err)
				}
			}
			return data
		case <-time.After(5 * time.Second):
			t.Fatal("no approval request")
		}
	}
}

func outputs(p *llmtest.Scripted) []string {
	var out []string
	for _, it := range p.Requests()[len(p.Requests())-1].Items {
		if it.Kind == protocol.ItemToolOutput {
			out = append(out, it.Output.Content)
		}
	}
	return out
}

func TestApprovalFlow(t *testing.T) {
	m, p, runs := newApprovalManager(t, 0,
		gatedCall("c1", "a"), llmtest.Step{Text: "ok"}, // 批准一次
		gatedCall("c2", "b"), llmtest.Step{Text: "ok"}, // 拒绝
		gatedCall("c3", "c"), llmtest.Step{Text: "ok"}, // 本会话都允许
		gatedCall("c4", "c"), llmtest.Step{Text: "ok"}, // 不再询问
	)
	s, _ := m.Create("")
	_, sub, _ := s.Bus().Subscribe(s.Bus().LastSeq())
	defer sub.Cancel()

	for i, tc := range []struct {
		input    string
		decision protocol.ReviewDecision
		asks     bool
		want     string
	}{
		{"one", protocol.DecisionApproved, true, "ran a"},
		{"two", protocol.DecisionDenied, true, "the user denied this action"},
		{"three", protocol.DecisionApprovedForSession, true, "ran c"},
		{"four", "", false, "ran c"},
	} {
		turnID, err := s.Submit(tc.input)
		if err != nil {
			t.Fatal(err)
		}
		if tc.asks {
			answer(t, s, sub, tc.decision)
		}
		evs := waitTurn(t, sub, turnID)
		if st := turnStatus(evs); st != protocol.TurnCompleted {
			t.Fatalf("turn %d status = %s", i, st)
		}
		if got := outputs(p); len(got) == 0 || !strings.Contains(got[len(got)-1], tc.want) {
			t.Errorf("turn %d output = %v, want %q", i, got, tc.want)
		}
		resolved := 0
		for _, e := range evs {
			if e.Type == protocol.EventApprovalResolved {
				resolved++
			}
		}
		if (resolved == 1) != tc.asks {
			t.Errorf("turn %d: %d approval.resolved events", i, resolved)
		}
	}
	if *runs != 3 {
		t.Errorf("tool ran %d times, want 3", *runs)
	}
	if err := s.Resolve("appr_nope", protocol.DecisionApproved); !errors.Is(err, ErrApprovalNotFound) {
		t.Errorf("unknown approval = %v", err)
	}
	if err := s.Resolve("x", "maybe"); !errors.Is(err, ErrInvalidDecision) {
		t.Errorf("bad decision = %v", err)
	}
}

func TestApprovalTimeoutAndInterrupt(t *testing.T) {
	m, p, runs := newApprovalManager(t, 100*time.Millisecond,
		gatedCall("c1", "a"), llmtest.Step{Text: "ok"},
		// 被中断的轮次不会再请求模型，所以后面没有回答步骤。
		gatedCall("c2", "b"),
		gatedCall("c3", "c"),
	)
	s, _ := m.Create("")
	_, sub, _ := s.Bus().Subscribe(s.Bus().LastSeq())
	defer sub.Cancel()

	// 超时：按拒绝处理，轮次继续完成。
	turnID, _ := s.Submit("timeout")
	evs := waitTurn(t, sub, turnID)
	if turnStatus(evs) != protocol.TurnCompleted || !strings.Contains(outputs(p)[0], "in time") {
		t.Errorf("timeout: %s %v", turnStatus(evs), outputs(p))
	}

	// 等待审批时用户中断：审批以 cancelled 结束，调用被标记为 aborted。
	m.rt.ApprovalTimeout = time.Minute
	turnID, _ = s.Submit("interrupt")
	answer(t, s, sub, "")
	if len(s.PendingApprovals()) != 1 {
		t.Errorf("pending = %v", s.PendingApprovals())
	}
	s.Interrupt(2 * time.Second)
	evs = waitTurn(t, sub, turnID)
	if turnStatus(evs) != protocol.TurnInterrupted || len(s.PendingApprovals()) != 0 {
		t.Errorf("interrupt: %s pending=%v", turnStatus(evs), s.PendingApprovals())
	}
	assertResolved(t, evs, protocol.DecisionCancelled)

	// 用户选择中止：轮次被中断。
	turnID, _ = s.Submit("abort")
	answer(t, s, sub, protocol.DecisionAbort)
	evs = waitTurn(t, sub, turnID)
	if turnStatus(evs) != protocol.TurnInterrupted {
		t.Errorf("abort: %s", turnStatus(evs))
	}
	assertResolved(t, evs, protocol.DecisionAbort)
	info := s.Info()
	for _, it := range info.Items {
		if it.Kind == protocol.ItemToolOutput && it.Output.CallID != "c1" && it.Output.Content != tools.AbortedMessage {
			t.Errorf("output %s = %q", it.Output.CallID, it.Output.Content)
		}
	}
	if *runs != 0 {
		t.Errorf("tool ran %d times", *runs)
	}
}

func assertResolved(t *testing.T, evs []protocol.Event, want protocol.ReviewDecision) {
	t.Helper()
	for _, e := range evs {
		if e.Type == protocol.EventApprovalResolved {
			if d := e.Data.(protocol.ApprovalData).Decision; d != want {
				t.Errorf("decision = %s, want %s", d, want)
			}
			return
		}
	}
	t.Errorf("no approval.resolved event")
}

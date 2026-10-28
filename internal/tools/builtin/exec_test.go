package builtin

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

func execEnv(t *testing.T, approval sandbox.ApprovalMode) (Env, string) {
	t.Helper()
	if !sandbox.Available() {
		t.Skip("exec_command needs macOS Seatbelt")
	}
	return editEnv(t, approval)
}

func TestExecCommand(t *testing.T) {
	env, base := execEnv(t, sandbox.ApproveOnRequest)
	ec := &ExecCommand{Env: env}
	var mu sync.Mutex
	var streamed strings.Builder
	inv := tools.Invocation{CallID: "c", Tool: "exec_command", Output: func(s string) { mu.Lock(); streamed.WriteString(s); mu.Unlock() }}
	run := func(args string) (tools.Result, error) {
		t.Helper()
		if err := tools.Validate(ec.Spec().Parameters, json.RawMessage(args)); err != nil {
			return tools.Result{}, err
		}
		inv.Args = json.RawMessage(args)
		return ec.Handle(context.Background(), inv)
	}

	res, err := run(`{"command":"echo hello; echo oops >&2; exit 3"}`)
	if err != nil || !res.IsError || !strings.Contains(res.Content, "Exit code: 3") || !strings.Contains(res.Content, "hello\noops") {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if !strings.Contains(streamed.String(), "hello") {
		t.Errorf("streamed = %q", streamed.String())
	}
	if res, err := run(`{"command":"pwd","workdir":"src"}`); err != nil || res.IsError || !strings.Contains(res.Content, base+"/ws/src") {
		t.Errorf("workdir = %+v, %v", res, err)
	}
	if res, _ := run(`{"command":"sleep 5","timeout_ms":200}`); !res.IsError || !strings.Contains(res.Content, "timed out") {
		t.Errorf("timeout = %+v", res)
	}
	// 越权写被 Seatbelt 拒绝，输出提示可以申请沙箱外执行。
	res, _ = run(`{"command":"echo x > ` + base + `/outside/evil"}`)
	if !res.IsError || !strings.Contains(res.Content, "Operation not permitted") || !strings.Contains(res.Content, "require_escalated") {
		t.Errorf("denied = %+v", res)
	}
	if _, err := os.Stat(base + "/outside/evil"); err == nil {
		t.Error("sandboxed command wrote outside the workspace")
	}
	for args, want := range map[string]string{
		`{"command":"ls","workdir":"../outside"}`:       "outside the allowed directories",
		`{"command":"ls","sandbox_permissions":"root"}`: `must be one of "use_default", "require_escalated"`,
		`{"cmd":"ls"}`:          `missing required field "command"`,
		`{"command":"   "}`:     "command is required",
		`{"command":"sudo ls"}`: "no one can approve it here",
		`{"command":"ls","sandbox_permissions":"require_escalated"}`: "no one can approve it here",
	} {
		if _, err := run(args); err == nil || !tools.IsModelError(err) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}

func TestExecCommandApprovals(t *testing.T) {
	env, base := execEnv(t, sandbox.ApproveOnRequest)
	ec := &ExecCommand{Env: env}
	run := func(args string, ap tools.Approver) (tools.Result, error) {
		return ec.Handle(context.Background(), tools.Invocation{CallID: "c", Tool: "exec_command", Args: json.RawMessage(args), Approver: ap})
	}
	escalated := `{"command":"echo x > ` + base + `/outside/esc && echo done","sandbox_permissions":"require_escalated","justification":"write the report"}`

	// 拒绝：命令没有执行。
	ap := &recordingApprover{decisions: []protocol.ReviewDecision{protocol.DecisionDenied}}
	if _, err := run(escalated, ap); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied = %v", err)
	}
	if _, err := os.Stat(base + "/outside/esc"); err == nil {
		t.Fatal("denied command ran")
	}
	d := ap.requests[0].Data
	if !d.Unsandboxed || d.Kind != protocol.ApprovalExec || !strings.Contains(d.Reason, "write the report") || ap.requests[0].Keys[0] != "exec-unsandboxed:echo x > "+base+"/outside/esc && echo done" {
		t.Errorf("request = %+v", ap.requests[0])
	}
	// 批准：在沙箱外执行。
	ap = &recordingApprover{decisions: []protocol.ReviewDecision{protocol.DecisionApproved}}
	if res, err := run(escalated, ap); err != nil || res.IsError || !strings.Contains(res.Content, "done") {
		t.Fatalf("approved = %+v, %v", res, err)
	}
	if readFile(t, base+"/outside/esc") != "x\n" {
		t.Error("escalated command did not run outside the sandbox")
	}
	// 危险命令需要批准，即使在沙箱里；用户中止时返回取消。
	ap = &recordingApprover{decisions: []protocol.ReviewDecision{protocol.DecisionAbort}}
	if _, err := run(`{"command":"rm -rf ~"}`, ap); err == nil || tools.IsModelError(err) {
		t.Errorf("abort = %v", err)
	}
	if len(ap.requests) != 1 || ap.requests[0].Data.Unsandboxed {
		t.Errorf("dangerous request = %+v", ap.requests)
	}
	// 普通命令在 on-request 下不需要批准。
	ap = &recordingApprover{}
	if res, err := run(`{"command":"echo ok > ok.txt && cat ok.txt"}`, ap); err != nil || res.IsError || len(ap.requests) != 0 {
		t.Errorf("plain command = %+v, %v, %d requests", res, err, len(ap.requests))
	}
	// deny 规则和 never 模式。
	env.Rules = sandbox.Rules{Deny: []string{"git push"}}
	env.Approval = sandbox.ApproveNever
	ec = &ExecCommand{Env: env}
	for args, want := range map[string]string{
		`{"command":"git push origin main"}`:                         "deny rule",
		`{"command":"sudo ls"}`:                                      "approval=never",
		`{"command":"ls","sandbox_permissions":"require_escalated"}`: "approval is disabled",
	} {
		if _, err := run(args, &recordingApprover{decisions: []protocol.ReviewDecision{protocol.DecisionApproved}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}

func TestExecCommandUntrusted(t *testing.T) {
	env, _ := execEnv(t, sandbox.ApproveUntrusted)
	ec := &ExecCommand{Env: env}
	ap := &recordingApprover{decisions: []protocol.ReviewDecision{protocol.DecisionApproved}}
	if res, err := ec.Handle(context.Background(), tools.Invocation{Args: json.RawMessage(`{"command":"ls"}`), Approver: ap}); err != nil || res.IsError || len(ap.requests) != 0 {
		t.Errorf("known-safe = %+v %v %d", res, err, len(ap.requests))
	}
	if _, err := ec.Handle(context.Background(), tools.Invocation{Args: json.RawMessage(`{"command":"go version"}`), Approver: ap}); err != nil || len(ap.requests) != 1 {
		t.Errorf("untrusted = %v, %d requests", err, len(ap.requests))
	}
}

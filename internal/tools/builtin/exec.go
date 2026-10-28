package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// ExecCommand 在会话沙箱中执行一条 shell 命令，对应 Codex 的 shell /
// exec_command 工具（core/src/tools/handlers/unified_exec.rs）。不支持
// 交互式终端和 write_stdin。
type ExecCommand struct {
	Env Env
}

type execArgs struct {
	Command   string `json:"command"`
	Workdir   string `json:"workdir"`
	TimeoutMS int    `json:"timeout_ms"`
	// SandboxPermissions 为 "require_escalated" 时在沙箱外执行，必须经用户批准。
	SandboxPermissions string `json:"sandbox_permissions"`
	Justification      string `json:"justification"`
}

const (
	sandboxDefault   = "use_default"
	sandboxEscalated = "require_escalated"
)

func (t *ExecCommand) Spec() tools.Spec {
	p := t.Env.Sandbox.Policy()
	net := "Network access is disabled."
	if p.Network {
		net = "Network access is enabled."
	}
	return tools.Spec{
		Name: "exec_command",
		Description: fmt.Sprintf("Run a shell command with /bin/sh -c in the workspace and return its exit code and output. "+
			"Commands run inside a macOS sandbox (%s): they can read most files and write only inside the workspace and temp dirs; "+
			".git is read-only. %s Interactive programs are not supported (stdin is empty). "+
			"If a command fails because the sandbox denied it and you really need it, retry with "+
			`sandbox_permissions="require_escalated" and a short justification; the user must approve.`, p.Mode, net),
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command":             {"type": "string", "description": "Shell command, e.g. \"go test ./...\""},
    "workdir":             {"type": "string", "description": "Working directory relative to the workspace root (default: the root)"},
    "timeout_ms":          {"type": "integer", "minimum": 1, "description": "Kill the command after this many milliseconds (default 30000, max 600000)"},
    "sandbox_permissions": {"type": "string", "enum": ["use_default", "require_escalated"], "description": "use_default (sandboxed) or require_escalated (run outside the sandbox after user approval)"},
    "justification":       {"type": "string", "description": "Why the command needs to run outside the sandbox; shown to the user. Only with require_escalated."}
  },
  "required": ["command"],
  "additionalProperties": false
}`),
		// 命令可能修改任何文件，不能和其他工具同时执行。
		ParallelSafe: false,
		// 等待审批的时间不计入超时；命令自己的超时由 timeout_ms 控制。
		Timeout: tools.NoTimeout,
	}
}

func (t *ExecCommand) Handle(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	var args execArgs
	if err := tools.DecodeArgs(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return tools.Result{}, tools.Errorf("command is required")
	}
	escalate := false
	switch args.SandboxPermissions {
	case "", sandboxDefault:
	case sandboxEscalated:
		escalate = true
	default:
		return tools.Result{}, tools.Errorf("sandbox_permissions must be %q or %q", sandboxDefault, sandboxEscalated)
	}
	sb := t.Env.Sandbox
	p := sb.Policy()
	if escalate && !sb.Enforced() {
		// danger-full-access 下本来就没有沙箱，不需要额外批准。
		escalate = false
	}

	cwd, err := t.Env.WS.Resolve(args.Workdir)
	if err != nil {
		return tools.Result{}, err
	}
	a := sandbox.Assess(command, p, t.Env.Approval, t.Env.Rules)
	data := protocol.ApprovalData{Kind: protocol.ApprovalExec, Command: command, Cwd: t.Env.WS.Rel(cwd), Reason: a.Reason}
	switch {
	case a.Decision == sandbox.Forbidden:
		return tools.Result{}, tools.Errorf("command refused: %s", a.Reason)
	case escalate && t.Env.Approval == sandbox.ApproveNever:
		return tools.Result{}, tools.Errorf("running outside the sandbox needs the user's approval, but approval is disabled (approval=never)")
	case escalate:
		data.Unsandboxed = true
		reason := "requested to run outside the sandbox"
		if args.Justification != "" {
			reason += ": " + args.Justification
		}
		if a.Decision == sandbox.NeedsApproval {
			reason += "; " + a.Reason
		}
		data.Reason = reason
		// 沙箱外执行的会话级批准按命令记，"本会话都允许"只对完全相同的命令生效。
		if err := inv.RequireApproval(ctx, tools.ApprovalRequest{Data: data, Keys: []string{"exec-unsandboxed:" + command}}); err != nil {
			return tools.Result{}, err
		}
	case a.Decision == sandbox.NeedsApproval:
		if err := inv.RequireApproval(ctx, tools.ApprovalRequest{Data: data, Keys: []string{"exec:" + command}}); err != nil {
			return tools.Result{}, err
		}
	}

	timeout := sandbox.DefaultTimeout
	if args.TimeoutMS > 0 {
		timeout = time.Duration(args.TimeoutMS) * time.Millisecond
	}
	var mu sync.Mutex
	res, err := sb.Exec(ctx, sandbox.ExecRequest{
		Command: command, Cwd: cwd, Timeout: timeout, MaxOutput: t.Env.MaxOutput, Unsandboxed: escalate,
	}, func(_ sandbox.Stream, b []byte) {
		mu.Lock()
		defer mu.Unlock()
		inv.Stream(string(b))
	})
	if err != nil {
		if ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		return tools.Result{}, tools.Errorf("cannot run the command: %v", err)
	}
	if res.Cancelled {
		return tools.Result{}, ctx.Err()
	}
	return tools.Result{Content: formatExec(res), IsError: res.ExitCode != 0 || res.TimedOut}, nil
}

// formatExec 把结果写成模型容易读的文本，类似 Codex 的 format_exec_output。
func formatExec(r sandbox.ExecResult) string {
	var b strings.Builder
	switch {
	case r.TimedOut:
		fmt.Fprintf(&b, "Command timed out after %s and was killed.\n", r.Duration.Round(time.Millisecond))
	default:
		fmt.Fprintf(&b, "Exit code: %d\n", r.ExitCode)
	}
	fmt.Fprintf(&b, "Wall time: %s\n", r.Duration.Round(time.Millisecond))
	if r.LikelyDenied() {
		b.WriteString("Note: the command probably failed because the sandbox denied it. " +
			"If it is necessary, retry with sandbox_permissions=\"require_escalated\" and a justification.\n")
	}
	if r.Truncated {
		b.WriteString("Note: output was truncated in the middle.\n")
	}
	out := r.Stdout
	if r.Stderr != "" {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += r.Stderr
	}
	if strings.TrimSpace(out) == "" {
		b.WriteString("Output: (none)\n")
	} else {
		b.WriteString("Output:\n" + out)
		if !strings.HasSuffix(out, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

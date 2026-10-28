package agent_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/agent"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
	"github.com/Czyyy-Joey/joey-agent/internal/tools/builtin"
)

type allowAll struct{ n int }

func (a *allowAll) Approve(context.Context, tools.Invocation, tools.ApprovalRequest) protocol.ReviewDecision {
	a.n++
	return protocol.DecisionApproved
}

func args(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// TestAllSixTools 用 scripted 模型在临时 Go 项目里依次调用 6 个内置工具：
// 搜索 TODO、读文件、打补丁改端口、新建文件、列目录、go build 并运行。
func TestAllSixTools(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("exec_command needs macOS Seatbelt")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", dir)
	ws := filepath.Join(dir, "proj")
	files := map[string]string{
		"go.mod":  "module demo\n\ngo 1.22\n",
		"main.go": "package main\n\nimport \"fmt\"\n\n// TODO: read the port from flags\nfunc main() {\n\tport := 8080\n\tfmt.Println(\"port\", port, greeting())\n}\n",
	}
	for name, body := range files {
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := sandbox.NewPolicy(sandbox.WorkspaceWrite, ws, sandbox.PolicyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := sandbox.New(p)
	if err != nil {
		t.Fatal(err)
	}
	router, err := tools.NewRouter(builtin.Tools(builtin.Env{
		WS: builtin.WorkspaceFromGuard(sandbox.NewGuard(p)), Sandbox: sb, Approval: sandbox.ApproveOnRequest,
	})...)
	if err != nil {
		t.Fatal(err)
	}
	patchText := "*** Begin Patch\n*** Update File: main.go\n@@ func main() {\n-\tport := 8080\n+\tport := 9090\n*** End Patch"
	provider := llmtest.NewScripted(
		llmtest.Step{Calls: []llmtest.StepCall{
			{ID: "s", Name: "search_files", Arguments: args(map[string]any{"pattern": "TODO"})},
			{ID: "r", Name: "read_file", Arguments: args(map[string]any{"path": "main.go"})},
		}},
		llmtest.Step{Calls: []llmtest.StepCall{
			{ID: "p", Name: "apply_patch", Arguments: args(map[string]any{"patch": patchText})},
			{ID: "w", Name: "write_file", Arguments: args(map[string]any{"path": "greet.go", "content": "package main\n\nfunc greeting() string { return \"hi\" }\n"})},
			{ID: "l", Name: "list_directory", Arguments: args(map[string]any{})},
			{ID: "e", Name: "exec_command", Arguments: args(map[string]any{"command": "go build -o app . && ./app", "timeout_ms": 120000})},
		}},
		llmtest.Step{Text: "Changed the port to 9090 and the build passes."},
	)
	rec := map[string]protocol.ItemData{}
	var streamed strings.Builder
	ap := &allowAll{}
	env := agent.TurnEnv{
		Provider: provider, Router: router, TurnID: "t1", Instructions: agent.DefaultInstructions(ws), Approver: ap,
		Emit: func(e protocol.Event) {
			d, ok := e.Data.(protocol.ItemData)
			if !ok || d.Kind != protocol.ItemToolCall {
				return
			}
			switch e.Type {
			case protocol.EventItemCompleted:
				rec[d.CallID] = d
			case protocol.EventItemDelta:
				streamed.WriteString(d.Output)
			}
		},
	}
	res := agent.RunTurn(context.Background(), env, nil, "Find TODOs, change the port to 9090, add greet.go and build")
	if res.Status != protocol.TurnCompleted {
		t.Fatalf("result = %+v", res)
	}
	for id, want := range map[string]string{
		"s": "main.go:5:// TODO: read the port from flags",
		"r": "port := 8080",
		"p": "M main.go",
		"w": "Created greet.go",
		"l": "greet.go",
		"e": "port 9090 hi",
	} {
		d := rec[id]
		if d.Status != protocol.ItemCompleted || !strings.Contains(d.Output, want) {
			t.Errorf("%s: status=%s output=%q, want %q", id, d.Status, d.Output, want)
		}
	}
	if !strings.Contains(streamed.String(), "port 9090 hi") {
		t.Errorf("exec output was not streamed: %q", streamed.String())
	}
	if ap.n != 0 {
		t.Errorf("on-request asked for approval %d times", ap.n)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "main.go")); !strings.Contains(string(b), "port := 9090") {
		t.Errorf("main.go = %s", b)
	}
}

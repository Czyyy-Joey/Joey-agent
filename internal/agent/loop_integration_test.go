package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/agent"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
	"github.com/Czyyy-Joey/joey-agent/internal/tools/builtin"
)

// TestLoopWithRealTools 使用 scripted 模型输出和真实的 read_file /
// list_directory 工具在临时工作区上驱动循环。
func TestLoopWithRealTools(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Joey-agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := builtin.NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	router, err := tools.NewRouter(builtin.Tools(ws)...)
	if err != nil {
		t.Fatal(err)
	}
	p := llmtest.NewScripted(
		llmtest.Step{Calls: []llmtest.StepCall{
			{ID: "c1", Name: "list_directory", Arguments: []byte(`{}`)},
			{ID: "c2", Name: "read_file", Arguments: []byte(`{"path":"README.md"}`)},
			{ID: "c3", Name: "read_file", Arguments: []byte(`{"path":"../../etc/passwd"}`)},
		}},
		llmtest.Step{Text: "The project is Joey-agent."},
	)
	env := agent.TurnEnv{Provider: p, Router: router, TurnID: "t1", Instructions: agent.DefaultInstructions(ws.Root())}
	res := agent.RunTurn(context.Background(), env, nil, "What is this project?")
	if res.Status != protocol.TurnCompleted {
		t.Fatalf("result = %+v", res)
	}

	second := p.Requests()[1].Items
	outputs := map[string]*protocol.ToolOutput{}
	for _, it := range second {
		if it.Kind == protocol.ItemToolOutput {
			outputs[it.Output.CallID] = it.Output
		}
	}
	if o := outputs["c1"]; o == nil || !strings.Contains(o.Content, "README.md") {
		t.Errorf("list_directory output = %+v", o)
	}
	if o := outputs["c2"]; o == nil || !strings.Contains(o.Content, "# Joey-agent") || o.IsError {
		t.Errorf("read_file output = %+v", o)
	}
	if o := outputs["c3"]; o == nil || !o.IsError || !strings.Contains(o.Content, "outside the workspace") {
		t.Errorf("escape attempt output = %+v", o)
	}
}

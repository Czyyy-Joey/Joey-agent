package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// copyDir 把 src 下的普通文件复制到 dst。
func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCodingTask 在临时 Go 项目里跑 coding_task.json 剧本：搜索 TODO → 读文件
// → apply_patch（approval=untrusted，经 HTTP 批准）→ exec_command go build。
// 断言文件内容、命令的退出码和流式输出。
func TestCodingTask(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is macOS only")
	}
	bin := buildJoey(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(base, "goapp")
	copyDir(t, "../testdata/demo/goapp", ws)
	if err := os.Mkdir(filepath.Join(base, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}

	env := testEnv("JOEY_APPROVAL=untrusted", "TMPDIR="+filepath.Join(base, "tmp"))
	url, _ := launch(t, bin, env, "-script", "../testdata/scenarios/coding_task.json", "-workspace", ws)
	id := createSession(t, url, ws)
	sc := stream(t, url, id, 0)
	turnID := submit(t, url, id, "找出 TODO，把端口改成 9090，然后 go build")

	var (
		approvals []string
		results   = map[string]map[string]any{}
		streamed  strings.Builder
	)
loop:
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e struct {
			Type   string         `json:"type"`
			TurnID string         `json:"turn_id"`
			Data   map[string]any `json:"data"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e.TurnID != turnID {
			continue
		}
		switch e.Type {
		case "approval.requested":
			// untrusted：apply_patch 和 go build 都需要批准；search/read 是只读的不需要。
			approvals = append(approvals, fmt.Sprint(e.Data["tool"]))
			code, body := postJSON(t, url+"/api/sessions/"+id+"/approvals/"+e.Data["approval_id"].(string), `{"decision":"approved"}`)
			if code != http.StatusOK {
				t.Fatalf("approve: %d %v", code, body)
			}
		case "item.delta":
			if e.Data["kind"] == "tool_call" {
				streamed.WriteString(fmt.Sprint(e.Data["output"]))
			}
		case "item.completed":
			if e.Data["kind"] == "tool_call" {
				results[fmt.Sprint(e.Data["call_id"])] = e.Data
			}
		}
		if e.Type == "turn.completed" {
			if e.Data["status"] != "completed" {
				t.Fatalf("turn = %v", e.Data)
			}
			break loop
		}
	}

	if strings.Join(approvals, ",") != "apply_patch,exec_command" {
		t.Errorf("approvals = %v", approvals)
	}
	for id, want := range map[string]string{
		"call_todo":  "util.go:3:// TODO: add tests",
		"call_read":  "port := 8080",
		"call_patch": "M main.go",
		"call_build": "Exit code: 0",
	} {
		r := results[id]
		if r == nil || r["status"] != "completed" || !strings.Contains(fmt.Sprint(r["output"]), want) {
			t.Errorf("%s = %v, want %q", id, r, want)
		}
	}
	if !strings.Contains(streamed.String(), "BUILD OK") {
		t.Errorf("build output was not streamed: %q", streamed.String())
	}
	b, err := os.ReadFile(filepath.Join(ws, "main.go"))
	if err != nil || !strings.Contains(string(b), "port := 9090") || strings.Contains(string(b), "8080") {
		t.Errorf("main.go = %s, %v", b, err)
	}
}

// TestDeniedApproval：默认 on-request 下危险命令需要批准；用户拒绝后模型收到
// "用户拒绝"并换成安全的命令，后者在沙箱里直接执行。
func TestDeniedApproval(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is macOS only")
	}
	bin := buildJoey(t)
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	url, _ := launch(t, bin, testEnv(), "-script", "../testdata/scenarios/denied_command.json", "-workspace", ws)
	id := createSession(t, url, ws)
	sc := stream(t, url, id, 0)
	turnID := submit(t, url, id, "清理构建目录")
	outputs := map[string]string{}
	asked := 0
loop:
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e struct {
			Type   string         `json:"type"`
			TurnID string         `json:"turn_id"`
			Data   map[string]any `json:"data"`
		}
		_ = json.Unmarshal([]byte(line), &e)
		if e.TurnID != turnID {
			continue
		}
		switch e.Type {
		case "approval.requested":
			asked++
			if e.Data["command"] != "rm -rf ~" {
				t.Errorf("approval for %v", e.Data["command"])
			}
			postJSON(t, url+"/api/sessions/"+id+"/approvals/"+e.Data["approval_id"].(string), `{"decision":"denied"}`)
		case "item.completed":
			if e.Data["kind"] == "tool_call" {
				outputs[fmt.Sprint(e.Data["call_id"])] = fmt.Sprint(e.Data["output"])
			}
		case "turn.completed":
			if e.Data["status"] != "completed" {
				t.Fatalf("turn = %v", e.Data)
			}
			break loop
		}
	}
	if asked != 1 || !strings.Contains(outputs["call_rm"], "the user denied this action") || !strings.Contains(outputs["call_rm2"], "cleaned") {
		t.Errorf("asked=%d outputs=%v", asked, outputs)
	}
	if _, err := os.Stat(filepath.Join(ws, "build")); err == nil {
		t.Error("./build was not removed")
	}
}

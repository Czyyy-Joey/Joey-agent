// Package e2e 将真实的 joey 二进制作为黑盒运行。
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildJoey 在每次测试运行时编译一次二进制。
func buildJoey(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "joey")
	cmd := exec.Command("go", "build", "-o", bin, "../cmd/joey")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

// testEnv 让子进程默认使用内存数据库，避免写入用户的 ~/.joey/joey.db；
// 需要持久化的测试用 -db 覆盖。
func testEnv(extra ...string) []string {
	return append(os.Environ(), append([]string{"ONEAPI_API_KEY=", "JOEY_DB=:memory:"}, extra...)...)
}

// startServe 在随机端口上启动 "joey serve" 并返回其基础 URL。
func startServe(t *testing.T, bin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, append([]string{"serve", "-addr", "127.0.0.1:0", "-web", ""}, args...)...)
	cmd.Env = testEnv()
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancel()
			t.Error("joey serve did not exit after SIGINT")
		}
		cancel()
	})

	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if url, ok := strings.CutPrefix(sc.Text(), "listening on "); ok {
				lines <- url
			}
		}
	}()
	select {
	case url := <-lines:
		return url
	case <-time.After(10 * time.Second):
		t.Fatal("joey serve did not start")
	}
	return ""
}

type event struct {
	Seq    int64           `json:"seq"`
	Type   string          `json:"type"`
	TurnID string          `json:"turn_id"`
	Data   json.RawMessage `json:"data"`
}

func postJSON(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// readTurn 读取 SSE 事件直到 turnID 的 turn.completed。
func readTurn(t *testing.T, sc *bufio.Scanner, turnID string) []event {
	t.Helper()
	var evs []event
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatal(err)
		}
		if e.TurnID != turnID {
			continue
		}
		evs = append(evs, e)
		if e.Type == "turn.completed" {
			return evs
		}
	}
	t.Fatalf("stream ended before turn.completed: %v", sc.Err())
	return nil
}

func TestPhase1ReadReadme(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# Joey-agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := startServe(t, buildJoey(t),
		"-provider", "scripted", "-script", "../testdata/scenarios/read_readme.json", "-workspace", ws)

	code, sess := postJSON(t, base+"/api/sessions", "")
	if code != http.StatusCreated {
		t.Fatalf("create session: %d %v", code, sess)
	}
	id := sess["id"].(string)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/sessions/"+id+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)

	code, turn := postJSON(t, base+"/api/sessions/"+id+"/turns", `{"text":"读取 README.md，告诉我项目名"}`)
	if code != http.StatusAccepted {
		t.Fatalf("submit: %d %v", code, turn)
	}
	evs := readTurn(t, sc, turn["turn_id"].(string))

	var sequence []string
	var answer strings.Builder
	var readOutput string
	for _, e := range evs {
		var d struct {
			Kind, Status, Name, Text, Output string
		}
		_ = json.Unmarshal(e.Data, &d)
		switch {
		case e.Type == "item.delta" && d.Kind == "agent_message":
			answer.WriteString(d.Text)
			continue
		case e.Type == "item.completed" && d.Kind == "tool_call" && d.Name == "read_file":
			readOutput = d.Output
		}
		if e.Type != "item.delta" && e.Type != "token.usage" {
			sequence = append(sequence, fmt.Sprintf("%s/%s/%s/%s", e.Type, d.Kind, d.Name, d.Status))
		}
	}
	want := []string{
		"turn.started///running",
		"item.completed/user_message//completed",
		"item.started/agent_message//in_progress",
		"item.completed/agent_message//completed",
		"item.started/tool_call/list_directory/in_progress",
		"item.completed/tool_call/list_directory/completed",
		"item.started/tool_call/read_file/in_progress",
		"item.completed/tool_call/read_file/completed",
		"item.started/agent_message//in_progress",
		"item.completed/agent_message//completed",
		"turn.completed///completed",
	}
	if strings.Join(sequence, "\n") != strings.Join(want, "\n") {
		t.Errorf("event sequence:\n%s\nwant:\n%s", strings.Join(sequence, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(readOutput, "# Joey-agent") {
		t.Errorf("read_file output = %q", readOutput)
	}
	if !strings.Contains(answer.String(), "Joey-agent") {
		t.Errorf("answer = %q", answer.String())
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq <= evs[i-1].Seq {
			t.Fatalf("seq not increasing at %d", i)
		}
	}
}

func TestPhase1Interrupt(t *testing.T) {
	base := startServe(t, buildJoey(t),
		"-provider", "scripted", "-script", "../testdata/scenarios/slow_stream.json", "-workspace", t.TempDir())
	_, sess := postJSON(t, base+"/api/sessions", "")
	id := sess["id"].(string)

	_, turn := postJSON(t, base+"/api/sessions/"+id+"/turns", `{"text":"go slowly"}`)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if _, body := postJSON(t, base+"/api/sessions/"+id+"/interrupt", ""); body["interrupted"] != true {
		t.Fatalf("interrupt = %v", body)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("interrupt took %s", d)
	}

	// 重放整个轮次并检查其最终状态。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/sessions/"+id+"/events?after=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	evs := readTurn(t, bufio.NewScanner(resp.Body), turn["turn_id"].(string))
	if !strings.Contains(string(evs[len(evs)-1].Data), `"status":"interrupted"`) {
		t.Errorf("final event = %s", evs[len(evs)-1].Data)
	}

	// 之后会话可以接受新的轮次。
	if code, _ := postJSON(t, base+"/api/sessions/"+id+"/turns", `{"text":"again"}`); code != http.StatusAccepted {
		t.Errorf("submit after interrupt: %d", code)
	}
}

func TestPhase1ChatCLI(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# Joey-agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(buildJoey(t), "chat", "-provider", "scripted",
		"-script", "../testdata/scenarios/read_readme.json", "-workspace", ws, "读取 README.md")
	cmd.Env = testEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("chat: %v\n%s", err, out)
	}
	for _, want := range []string{"→ read_file(", "✓ completed", "Joey-agent", "── completed"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestServeRefusesPublicAddrWithoutToken(t *testing.T) {
	cmd := exec.Command(buildJoey(t), "serve", "-provider", "scripted",
		"-script", "../testdata/scenarios/read_readme.json", "-addr", "0.0.0.0:0", "-web", "")
	cmd.Env = testEnv("JOEY_AUTH_TOKEN=")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "refusing to listen") {
		t.Errorf("err = %v, output = %s", err, out)
	}
}

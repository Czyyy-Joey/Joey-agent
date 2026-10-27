package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// launch 启动 joey serve 并返回基础 URL 和用于模拟崩溃的 kill 函数。
// 测试结束时若进程仍在运行，会被强制结束。
func launch(t *testing.T, bin string, env []string, args ...string) (string, func()) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"serve", "-addr", "127.0.0.1:0", "-web", "", "-provider", "scripted"}, args...)...)
	cmd.Env = env
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	kill := func() {
		_ = cmd.Process.Kill()
		<-done
	}
	t.Cleanup(kill)

	urls := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if u, ok := strings.CutPrefix(sc.Text(), "listening on "); ok {
				urls <- u
			}
		}
	}()
	select {
	case u := <-urls:
		return u, kill
	case <-done:
		t.Fatal("joey serve exited during startup")
	case <-time.After(10 * time.Second):
		t.Fatal("joey serve did not start")
	}
	return "", nil
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %v", url, resp.StatusCode, out)
	}
	return out
}

// stream 打开会话的 SSE 流，从 after 之后开始。
func stream(t *testing.T, base, id string, after int) *bufio.Scanner {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/sessions/"+id+"/events?after="+strconv.Itoa(after), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return bufio.NewScanner(resp.Body)
}

// submit 提交一轮并返回其 turn_id。
func submit(t *testing.T, base, id, text string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text})
	code, out := postJSON(t, base+"/api/sessions/"+id+"/turns", string(body))
	if code != http.StatusAccepted {
		t.Fatalf("submit %q: %d %v", text, code, out)
	}
	return out["turn_id"].(string)
}

type turnSummary struct {
	status  string
	answer  string
	outputs map[string]string // 工具名 → 输出
}

func summarize(evs []event) turnSummary {
	s := turnSummary{outputs: map[string]string{}}
	for _, e := range evs {
		var d struct{ Kind, Status, Name, Text, Output string }
		_ = json.Unmarshal(e.Data, &d)
		switch {
		case e.Type == "item.delta" && d.Kind == "agent_message":
			s.answer += d.Text
		case e.Type == "item.completed" && d.Kind == "tool_call":
			s.outputs[d.Name] = d.Output
		case e.Type == "turn.completed":
			s.status = d.Status
		}
	}
	return s
}

func createSession(t *testing.T, base, cwd string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"cwd": cwd})
	code, out := postJSON(t, base+"/api/sessions", string(body))
	if code != http.StatusCreated || out["cwd"] != cwd {
		t.Fatalf("create session in %s: %d %v", cwd, code, out)
	}
	return out["id"].(string)
}

// TestPhase2Lifecycle 覆盖 Phase 2 的用户场景：在不同目录创建会话 → 多工具
// → 中途取消 → 继续 → 进程崩溃 → 重启 → 恢复会话并继续提问。
func TestPhase2Lifecycle(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	alpha, beta := filepath.Join(base, "projects", "alpha"), filepath.Join(base, "projects", "beta")
	for dir, title := range map[string]string{alpha: "# Alpha\n", beta: "# Beta\n"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(title), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db := filepath.Join(base, "state", "joey.db")
	env := testEnv("JOEY_ALLOWED_ROOTS=" + filepath.Join(base, "projects"))
	bin := buildJoey(t)

	url, kill := launch(t, bin, env, "-db", db, "-workspace", alpha, "-script", "../testdata/scenarios/lifecycle.json")

	// 1. 默认目录（alpha）中的会话：多工具调用，读到的是 alpha 的 README。
	a := createSession(t, url, alpha)
	sa := stream(t, url, a, 0)
	got := summarize(readTurn(t, sa, submit(t, url, a, "这个项目叫什么？")))
	if got.status != "completed" || !strings.Contains(got.outputs["read_file"], "# Alpha") || !strings.Contains(got.answer, "Alpha") {
		t.Fatalf("turn 1 = %+v", got)
	}

	// 2. 中途取消，再继续。
	slow := submit(t, url, a, "做一个很慢的任务")
	time.Sleep(150 * time.Millisecond)
	if _, body := postJSON(t, url+"/api/sessions/"+a+"/interrupt", ""); body["interrupted"] != true {
		t.Fatalf("interrupt = %v", body)
	}
	if got := summarize(readTurn(t, sa, slow)); got.status != "interrupted" {
		t.Fatalf("slow turn = %+v", got)
	}
	if got := summarize(readTurn(t, sa, submit(t, url, a, "继续"))); got.status != "completed" {
		t.Fatalf("continue turn = %+v", got)
	}

	// 3. 另一个目录（beta）中的会话，工具只能看到 beta。
	b := createSession(t, url, beta)
	sb := stream(t, url, b, 0)
	got = summarize(readTurn(t, sb, submit(t, url, b, "这个呢？")))
	if !strings.Contains(got.outputs["read_file"], "# Beta") {
		t.Fatalf("beta turn = %+v", got)
	}
	if code, _ := postJSON(t, url+"/api/sessions", `{"cwd":"`+base+`"}`); code != http.StatusForbidden {
		t.Errorf("cwd outside allowed roots: %d, want 403", code)
	}

	// 4. beta 中的轮次进行到一半时进程被杀。
	crashTurn := submit(t, url, b, "最后一个任务")
	for sb.Scan() {
		if strings.Contains(sb.Text(), `"item.delta"`) {
			break
		}
	}
	snapA := getJSON(t, url+"/api/sessions/"+a)
	kill()

	// 5. 用同一个数据库重启。
	url, _ = launch(t, bin, env, "-db", db, "-workspace", alpha, "-script", "../testdata/scenarios/resume.json")

	list := getJSON(t, url+"/api/sessions")["sessions"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["id"] != b || list[0].(map[string]any)["cwd"] != beta {
		t.Fatalf("session list after restart = %v", list)
	}
	infoB := getJSON(t, url+"/api/sessions/"+b)
	if infoB["status"] != "idle" || infoB["last_turn_status"] != "interrupted" {
		t.Errorf("crashed session = %v", infoB)
	}
	itemsB := infoB["items"].([]any)
	if last := itemsB[len(itemsB)-1].(map[string]any); last["kind"] != "turn_aborted" {
		t.Errorf("last item of crashed session = %v", last)
	}

	// 崩溃的轮次在事件流中也有结尾，且全部事件的 seq 连续。
	evs := readTurn(t, stream(t, url, b, 0), crashTurn)
	if summarize(evs).status != "interrupted" {
		t.Errorf("crashed turn replay = %+v", summarize(evs))
	}

	// 6. 会话 A 的历史与崩溃前完全一致，继续提问时模型拿到完整上文。
	infoA := getJSON(t, url+"/api/sessions/"+a)
	wantItems, _ := json.Marshal(snapA["items"])
	gotItems, _ := json.Marshal(infoA["items"])
	if string(wantItems) != string(gotItems) {
		t.Errorf("history of session A changed across restart")
	}
	lastSeq := int(infoA["last_seq"].(float64))
	sa = stream(t, url, a, lastSeq)
	follow := submit(t, url, a, "还记得项目名吗？")
	evs = readTurn(t, sa, follow)
	if evs[0].Seq != int64(lastSeq+1) {
		t.Errorf("first seq after restart = %d, want %d", evs[0].Seq, lastSeq+1)
	}
	if got := summarize(evs); got.status != "completed" || !strings.Contains(got.answer, "Alpha") {
		t.Errorf("follow-up = %+v", got)
	}

	all := readAll(t, stream(t, url, a, 0), follow)
	for i := 1; i < len(all); i++ {
		if all[i].Seq != all[i-1].Seq+1 {
			t.Fatalf("replay seq gap: %d then %d", all[i-1].Seq, all[i].Seq)
		}
	}
	if all[0].Seq != 1 {
		t.Errorf("replay starts at %d", all[0].Seq)
	}
}

// readAll 读取所有事件（不按轮次过滤），直到 turnID 的 turn.completed。
func readAll(t *testing.T, sc *bufio.Scanner, turnID string) []event {
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
		evs = append(evs, e)
		if e.TurnID == turnID && e.Type == "turn.completed" {
			return evs
		}
	}
	t.Fatalf("stream ended before turn.completed: %v", sc.Err())
	return nil
}

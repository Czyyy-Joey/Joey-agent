package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/dialog"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/session"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
	"github.com/Czyyy-Joey/joey-agent/internal/workspace"
)

type fixture struct {
	srv *httptest.Server
	mgr *session.Manager
}

func newFixture(t *testing.T, opts Options, steps ...llmtest.Step) *fixture {
	t.Helper()
	return newFixtureWithTools(t, opts, nil, steps...)
}

// newFixtureWithTools 和 newFixture 相同，但会话使用 ts 中的工具。
func newFixtureWithTools(t *testing.T, opts Options, ts []tools.Tool, steps ...llmtest.Step) *fixture {
	t.Helper()
	ws, err := workspace.NewPolicy(t.TempDir(), []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := session.NewManager(session.Runtime{
		Provider:   llmtest.NewScripted(steps...),
		Workspaces: ws,
		Tools:      func(session.ToolEnv) (*tools.Router, error) { return tools.NewRouter(ts...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	opts.Manager = mgr
	srv := httptest.NewServer(New(opts))
	t.Cleanup(func() {
		mgr.Close(time.Second)
		srv.Close()
	})
	return &fixture{srv: srv, mgr: mgr}
}

func (f *fixture) do(t *testing.T, method, path, body string, hdr ...string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (f *fixture) createSession(t *testing.T) string {
	t.Helper()
	resp, body := f.do(t, "POST", "/api/sessions", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	return body["id"].(string)
}

// sseReader 解析 SSE 响应体。
type sseReader struct {
	sc *bufio.Scanner
}

type sseEvent struct {
	id, event string
	data      string
}

func (r *sseReader) next() (sseEvent, error) {
	var ev sseEvent
	for r.sc.Scan() {
		line := r.sc.Text()
		switch {
		case line == "":
			if ev.data != "" || ev.event != "" {
				return ev, nil
			}
		case strings.HasPrefix(line, ":"):
			ev.event = "comment"
			return ev, nil
		case strings.HasPrefix(line, "id: "):
			ev.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			ev.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			ev.data = line[6:]
		}
	}
	if err := r.sc.Err(); err != nil {
		return ev, err
	}
	return ev, io.EOF
}

func openSSE(t *testing.T, ctx context.Context, url string, hdr ...string) (*http.Response, *sseReader) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp, &sseReader{sc: bufio.NewScanner(resp.Body)}
}

// readUntilTurnDone 读取事件直到 turn.completed 并返回它们。
func readUntilTurnDone(t *testing.T, r *sseReader) []protocol.Event {
	t.Helper()
	var out []protocol.Event
	for {
		ev, err := r.next()
		if err != nil {
			t.Fatalf("sse read: %v (got %d events)", err, len(out))
		}
		if ev.event == "comment" || ev.data == "" {
			continue
		}
		var e protocol.Event
		if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
			t.Fatal(err)
		}
		if ev.id != fmt.Sprint(e.Seq) {
			t.Errorf("SSE id %s != seq %d", ev.id, e.Seq)
		}
		out = append(out, e)
		if e.Type == protocol.EventTurnCompleted {
			return out
		}
	}
}

func TestTurnOverSSE(t *testing.T) {
	f := newFixture(t, Options{}, llmtest.Step{Text: "Hello!", Chunk: 2})
	id := f.createSession(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, r := openSSE(t, ctx, f.srv.URL+"/api/sessions/"+id+"/events")
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %s", ct)
	}

	resp2, body := f.do(t, "POST", "/api/sessions/"+id+"/turns", `{"text":"hi"}`)
	if resp2.StatusCode != http.StatusAccepted || body["turn_id"] == "" {
		t.Fatalf("submit: %d %v", resp2.StatusCode, body)
	}
	evs := readUntilTurnDone(t, r)
	var text string
	for _, e := range evs {
		if e.Type == protocol.EventItemDelta {
			text += e.Data.(map[string]any)["text"].(string)
		}
	}
	if text != "Hello!" {
		t.Errorf("streamed text = %q", text)
	}
	if evs[0].Type != protocol.EventTurnStarted || evs[0].Seq != 1 {
		t.Errorf("first event = %+v", evs[0])
	}

	// 轮次结束后的快照。
	_, info := f.do(t, "GET", "/api/sessions/"+id, "")
	if info["status"] != "idle" || len(info["items"].([]any)) != 2 {
		t.Errorf("info = %v", info)
	}
}

// TestCompactCommand：发送 "/compact" 时启动压缩轮次，而不是把它当作输入发给模型。
func TestCompactCommand(t *testing.T) {
	f := newFixture(t, Options{}, llmtest.Step{Text: "first answer"}, llmtest.Step{Text: "the summary"})
	id := f.createSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, r := openSSE(t, ctx, f.srv.URL+"/api/sessions/"+id+"/events")
	defer resp.Body.Close()
	f.do(t, "POST", "/api/sessions/"+id+"/turns", `{"text":"goal"}`)
	readUntilTurnDone(t, r)
	if resp, _ := f.do(t, "POST", "/api/sessions/"+id+"/turns", `{"text":" /compact "}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("compact: %d", resp.StatusCode)
	}
	var compaction map[string]any
	for _, e := range readUntilTurnDone(t, r) {
		if d, ok := e.Data.(map[string]any); ok && e.Type == protocol.EventItemCompleted && d["kind"] == "compaction" {
			compaction = d
		}
	}
	if compaction == nil || compaction["status"] != "completed" || compaction["text"] != "the summary" {
		t.Fatalf("compaction = %v", compaction)
	}
	_, info := f.do(t, "GET", "/api/sessions/"+id, "")
	items := info["items"].([]any)
	if last := items[len(items)-1].(map[string]any); last["kind"] != "compaction" || len(items) != 3 {
		t.Errorf("items = %v", items)
	}
}

func TestSSEReplay(t *testing.T) {
	f := newFixture(t, Options{}, llmtest.Step{Text: "abc", Chunk: 1})
	id := f.createSession(t)
	s, _ := f.mgr.Get(id)
	turnID, _ := s.Submit("x")
	_ = s.Wait(context.Background())
	_ = turnID
	total := s.Bus().LastSeq()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name  string
		url   string
		hdr   []string
		first int64
	}{
		{"after query", "/events?after=3", nil, 4},
		{"last-event-id header", "/events", []string{"Last-Event-ID", "5"}, 6},
		{"larger of query and header wins", "/events?after=2", []string{"Last-Event-ID", "5"}, 6},
		{"stale header does not rewind", "/events?after=5", []string{"Last-Event-ID", "2"}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, r := openSSE(t, ctx, f.srv.URL+"/api/sessions/"+id+tc.url, tc.hdr...)
			defer resp.Body.Close()
			evs := readUntilTurnDone(t, r)
			if evs[0].Seq != tc.first || evs[len(evs)-1].Seq != total {
				t.Errorf("replayed seq %d..%d, want %d..%d", evs[0].Seq, evs[len(evs)-1].Seq, tc.first, total)
			}
		})
	}
}

func TestSSEHeartbeatAndDisconnect(t *testing.T) {
	f := newFixture(t, Options{Heartbeat: 20 * time.Millisecond})
	id := f.createSession(t)
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	resp, r := openSSE(t, ctx, f.srv.URL+"/api/sessions/"+id+"/events")
	ev, err := r.next()
	if err != nil || ev.event != "comment" {
		t.Fatalf("expected heartbeat, got %+v %v", ev, err)
	}
	cancel()
	resp.Body.Close()

	// 客户端断开后处理器 goroutine 必须退出。
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+1 {
		t.Errorf("goroutines: before %d, after %d", before, n)
	}
}

func TestInterruptEndpoint(t *testing.T) {
	f := newFixture(t, Options{}, llmtest.Step{Hang: true})
	id := f.createSession(t)
	if resp, _ := f.do(t, "POST", "/api/sessions/"+id+"/turns", `{"text":"x"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatal(resp.Status)
	}
	if resp, _ := f.do(t, "POST", "/api/sessions/"+id+"/turns", `{"text":"y"}`); resp.StatusCode != http.StatusConflict {
		t.Errorf("second submit status = %d, want 409", resp.StatusCode)
	}
	_, body := f.do(t, "POST", "/api/sessions/"+id+"/interrupt", "")
	if body["interrupted"] != true {
		t.Errorf("interrupt = %v", body)
	}
	_, info := f.do(t, "GET", "/api/sessions/"+id, "")
	if info["status"] != "idle" || info["last_turn_status"] != "interrupted" {
		t.Errorf("info = %v", info)
	}
	_, body = f.do(t, "POST", "/api/sessions/"+id+"/interrupt", "")
	if body["interrupted"] != false {
		t.Errorf("idle interrupt = %v", body)
	}
}

func TestBadRequests(t *testing.T) {
	f := newFixture(t, Options{})
	id := f.createSession(t)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/sessions/nope", "", 404},
		{"POST", "/api/sessions/nope/turns", `{"text":"x"}`, 404},
		{"GET", "/api/sessions/nope/events", "", 404},
		{"POST", "/api/sessions/" + id + "/turns", `{"text":"  "}`, 400},
		{"POST", "/api/sessions/" + id + "/turns", `not json`, 400},
		{"POST", "/api/sessions/" + id + "/turns", `{"text":"x","extra":1}`, 400},
		{"GET", "/api/sessions/" + id + "/turns", "", 405},
		{"POST", "/api/sessions", `{"cwd":"relative"}`, 400},
		{"POST", "/api/sessions", `{"cwd":"/definitely/missing/dir"}`, 400},
		{"POST", "/api/sessions", `{"cwd":"/"}`, 403},
		{"POST", "/api/sessions", `{"workspace":"/"}`, 400},
		{"GET", "/api/sessions?limit=0", "", 400},
		{"GET", "/api/workspace/dirs?path=/", "", 403},
	} {
		resp, body := f.do(t, tc.method, tc.path, tc.body)
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s: %d %v, want %d", tc.method, tc.path, resp.StatusCode, body, tc.want)
		}
	}
}

func TestOriginCheck(t *testing.T) {
	f := newFixture(t, Options{AllowedOrigins: []string{"https://trusted.example"}})
	for _, tc := range []struct {
		origin string
		want   int
	}{
		{"", 201},
		{"http://localhost:5173", 201},
		{"http://127.0.0.1:3000", 201},
		{"https://trusted.example", 201},
		{"https://evil.example", 403},
		{"null", 403},
	} {
		var hdr []string
		if tc.origin != "" {
			hdr = []string{"Origin", tc.origin}
		}
		if resp, _ := f.do(t, "POST", "/api/sessions", "", hdr...); resp.StatusCode != tc.want {
			t.Errorf("origin %q: %d, want %d", tc.origin, resp.StatusCode, tc.want)
		}
	}
}

func TestAuthToken(t *testing.T) {
	f := newFixture(t, Options{AuthToken: "s3cret"})
	if resp, _ := f.do(t, "POST", "/api/sessions", ""); resp.StatusCode != 401 {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/api/sessions", "", "Authorization", "Bearer wrong"); resp.StatusCode != 401 {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/api/sessions", "", "Authorization", "Bearer s3cret"); resp.StatusCode != 201 {
		t.Errorf("header token: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/api/sessions?token=s3cret", ""); resp.StatusCode != 201 {
		t.Errorf("query token: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "GET", "/api/health", ""); resp.StatusCode != 401 {
		t.Errorf("health without token: %d", resp.StatusCode)
	}
}

func TestStaticSPA(t *testing.T) {
	static := fstest.MapFS{
		"index.html":    {Data: []byte("<html>joey</html>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	f := newFixture(t, Options{Static: static})
	for path, want := range map[string]string{"/": "joey", "/assets/app.js": "console.log", "/sessions/abc": "joey"} {
		resp, err := http.Get(f.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(b), want) {
			t.Errorf("%s = %q", path, b)
		}
	}
}

func TestSessionListAndCwd(t *testing.T) {
	f := newFixture(t, Options{}, llmtest.Step{Text: "ok"})
	ws := f.mgr.Workspaces()
	other := ws.Roots()[1]
	if err := os.Mkdir(filepath.Join(other, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(other, "proj")

	resp, body := f.do(t, "POST", "/api/sessions", `{"cwd":"`+proj+`"}`)
	if resp.StatusCode != http.StatusCreated || body["cwd"] != proj {
		t.Fatalf("create with cwd: %d %v", resp.StatusCode, body)
	}
	_, def := f.do(t, "POST", "/api/sessions", "")
	if def["cwd"] != ws.Default() {
		t.Errorf("default cwd = %v", def["cwd"])
	}

	_, list := f.do(t, "GET", "/api/sessions", "")
	sessions := list["sessions"].([]any)
	if len(sessions) != 2 {
		t.Fatalf("list = %v", list)
	}
	if _, list = f.do(t, "GET", "/api/sessions?limit=1", ""); len(list["sessions"].([]any)) != 1 {
		t.Errorf("limit ignored: %v", list)
	}

	_, dirs := f.do(t, "GET", "/api/workspace/dirs", "")
	if dirs["default"] != ws.Default() || len(dirs["roots"].([]any)) != 2 {
		t.Errorf("roots listing = %v", dirs)
	}
	_, dirs = f.do(t, "GET", "/api/workspace/dirs?path="+url.QueryEscape(other), "")
	entries := dirs["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["path"] != proj {
		t.Errorf("dir listing = %v", dirs)
	}
}

func TestPickFolder(t *testing.T) {
	var gotStart string
	var picked string
	var pickErr error
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	opts := Options{PickFolder: func(ctx context.Context, _, start string) (string, error) {
		gotStart = start
		if pickErr == errBlock {
			entered <- struct{}{}
			select {
			case <-block:
			case <-ctx.Done():
			}
			return "", ctx.Err()
		}
		return picked, pickErr
	}}
	f := newFixture(t, opts)
	ws := f.mgr.Workspaces()
	proj := filepath.Join(ws.Roots()[1], "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	picked = proj + "/"
	resp, body := f.do(t, "POST", "/api/workspace/pick", "")
	if resp.StatusCode != 200 || body["path"] != proj || gotStart != ws.Default() {
		t.Errorf("pick: %d %v start=%s", resp.StatusCode, body, gotStart)
	}
	_, _ = f.do(t, "POST", "/api/workspace/pick", `{"start":"`+proj+`"}`)
	if gotStart != proj {
		t.Errorf("start = %s, want %s", gotStart, proj)
	}

	pickErr = dialog.ErrCancelled
	if _, body := f.do(t, "POST", "/api/workspace/pick", ""); body["cancelled"] != true {
		t.Errorf("cancel: %v", body)
	}
	pickErr = nil
	picked = "/"
	if resp, _ := f.do(t, "POST", "/api/workspace/pick", ""); resp.StatusCode != http.StatusForbidden {
		t.Errorf("outside roots: %d", resp.StatusCode)
	}
	pickErr = dialog.ErrUnsupported
	if resp, _ := f.do(t, "POST", "/api/workspace/pick", ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("unsupported: %d", resp.StatusCode)
	}

	// 同时只能有一个对话框。
	pickErr = errBlock
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.do(t, "POST", "/api/workspace/pick", "")
	}()
	<-entered
	if resp, _ := f.do(t, "POST", "/api/workspace/pick", ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("second dialog: %d, want 409", resp.StatusCode)
	}
	close(block)
	<-done

	// 远程客户端不能弹出服务器桌面上的对话框。
	srv := New(Options{Manager: f.mgr, PickFolder: opts.PickFolder})
	req := httptest.NewRequest("POST", "/api/workspace/pick", nil)
	req.RemoteAddr = "10.0.0.8:5555"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("remote client: %d", rec.Code)
	}

	disabled := newFixture(t, Options{})
	if resp, _ := disabled.do(t, "POST", "/api/workspace/pick", ""); resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("disabled: %d", resp.StatusCode)
	}
}

var errBlock = errors.New("block")

// approvalTool 请求批准后返回 "approved"。
type approvalTool struct{}

func (approvalTool) Spec() tools.Spec { return tools.Spec{Name: "needs_ok", Timeout: tools.NoTimeout} }
func (approvalTool) Handle(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	req := tools.ApprovalRequest{Data: protocol.ApprovalData{Kind: protocol.ApprovalExec, Command: "make deploy", Reason: "test"}}
	if err := inv.RequireApproval(ctx, req); err != nil {
		return tools.Result{}, err
	}
	return tools.Result{Content: "approved"}, nil
}

func TestApprovalEndpoint(t *testing.T) {
	f := newFixtureWithTools(t, Options{}, []tools.Tool{approvalTool{}},
		llmtest.Step{Calls: []llmtest.StepCall{{ID: "c1", Name: "needs_ok"}}},
		llmtest.Step{Text: "done"},
	)
	id := f.createSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, r := openSSE(t, ctx, f.srv.URL+"/api/sessions/"+id+"/events?after=0")
	if resp, _ := f.do(t, "POST", "/api/sessions/"+id+"/turns", `{"text":"x"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatal(resp.Status)
	}
	var req protocol.ApprovalData
	for req.ApprovalID == "" {
		ev, err := r.next()
		if err != nil {
			t.Fatal(err)
		}
		if ev.data == "" {
			continue
		}
		var e struct {
			Type protocol.EventType
			Data protocol.ApprovalData
		}
		if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
			t.Fatal(err)
		}
		if e.Type == protocol.EventApprovalRequested {
			req = e.Data
		}
	}
	if req.Command != "make deploy" || req.CallID != "c1" || req.Tool != "needs_ok" {
		t.Errorf("request = %+v", req)
	}
	path := "/api/sessions/" + id + "/approvals/" + req.ApprovalID
	if resp, _ := f.do(t, "POST", path, `{"decision":"maybe"}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad decision = %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "POST", "/api/sessions/"+id+"/approvals/appr_nope", `{"decision":"approved"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown approval = %d", resp.StatusCode)
	}
	if resp, body := f.do(t, "POST", path, `{"decision":"approved"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve = %d %v", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, "POST", path, `{"decision":"approved"}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second answer = %d", resp.StatusCode)
	}
	evs := readUntilTurnDone(t, r)
	var resolved, output string
	for _, e := range evs {
		b, _ := json.Marshal(e.Data)
		switch {
		case e.Type == protocol.EventApprovalResolved:
			resolved = string(b)
		case e.Type == protocol.EventItemCompleted && strings.Contains(string(b), `"tool_call"`):
			output = string(b)
		}
	}
	if !strings.Contains(resolved, `"decision":"approved"`) || !strings.Contains(output, `"output":"approved"`) {
		t.Errorf("resolved=%s output=%s", resolved, output)
	}
}

func TestDeleteSessionEndpoint(t *testing.T) {
	f := newFixture(t, Options{}, llmtest.Step{Hang: true})
	id := f.createSession(t)
	if resp, _ := f.do(t, "POST", "/api/sessions/"+id+"/turns", `{"text":"x"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatal(resp.Status)
	}
	if resp, body := f.do(t, "DELETE", "/api/sessions/"+id, ""); resp.StatusCode != http.StatusConflict {
		t.Errorf("delete running = %d %v", resp.StatusCode, body)
	}
	f.do(t, "POST", "/api/sessions/"+id+"/interrupt", "")
	if resp, _ := f.do(t, "DELETE", "/api/sessions/"+id, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "GET", "/api/sessions/"+id, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("get after delete = %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, "DELETE", "/api/sessions/"+id, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete = %d", resp.StatusCode)
	}
	if _, body := f.do(t, "GET", "/api/sessions", ""); len(body["sessions"].([]any)) != 0 {
		t.Errorf("list = %v", body)
	}
}

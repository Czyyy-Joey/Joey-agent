package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
	"github.com/Czyyy-Joey/joey-agent/internal/store"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
	"github.com/Czyyy-Joey/joey-agent/internal/workspace"
)

// rootTool 回显它被绑定的工作目录，用于确认每个会话拿到自己的工具。
type rootTool struct{ root string }

func (r rootTool) Spec() tools.Spec { return tools.Spec{Name: "where"} }
func (r rootTool) Handle(context.Context, tools.Invocation) (tools.Result, error) {
	return tools.Result{Content: "root=" + r.root}, nil
}

type env struct {
	dbPath string
	base   string
	ws     *workspace.Policy
}

func newEnv(t *testing.T) env {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"a", "b", "outside"} {
		if err := os.MkdirAll(filepath.Join(base, "roots", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.NewPolicy(filepath.Join(base, "roots", "a"), []string{filepath.Join(base, "roots", "b")})
	if err != nil {
		t.Fatal(err)
	}
	return env{dbPath: filepath.Join(base, "joey.db"), base: base, ws: ws}
}

// open 用 e 的数据库启动一个管理器；调用方负责 Close。
func (e env) open(t *testing.T, p *llmtest.Scripted) *Manager {
	t.Helper()
	db, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Runtime{
		Provider:   p,
		Store:      db,
		Workspaces: e.ws,
		Tools:      func(e ToolEnv) (*tools.Router, error) { return tools.NewRouter(rootTool{e.Root}) },
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return m
}

func (e env) close(m *Manager) {
	m.Close(2 * time.Second)
	m.db.Close()
}

func runTurn(t *testing.T, s *Session, input string) []protocol.Event {
	t.Helper()
	_, sub, _ := s.Bus().Subscribe(s.Bus().LastSeq())
	defer sub.Cancel()
	id, err := s.Submit(input)
	if err != nil {
		t.Fatal(err)
	}
	return waitTurn(t, sub, id)
}

func TestResumeAfterRestart(t *testing.T) {
	e := newEnv(t)
	m := e.open(t, llmtest.NewScripted(
		llmtest.Step{Text: "Checking.", Calls: []llmtest.StepCall{{ID: "c1", Name: "where"}}},
		llmtest.Step{Text: "The answer is 42."},
	))
	s, err := m.Create("")
	if err != nil {
		t.Fatal(err)
	}
	runTurn(t, s, "what is the answer?\nsecond line")
	before := s.Info()
	e.close(m)

	p2 := llmtest.NewScripted(llmtest.Step{Text: "Still 42."})
	m2 := e.open(t, p2)
	defer e.close(m2)
	s2, err := m2.Get(s.ID())
	if err != nil {
		t.Fatal(err)
	}
	after := s2.Info()
	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("snapshot changed across restart (-before +after):\n%s", diff)
	}
	if after.Title != "what is the answer?" || after.Cwd != e.ws.Default() {
		t.Errorf("summary = %+v", after.Summary)
	}

	evs := runTurn(t, s2, "and again?")
	if evs[0].Seq != before.LastSeq+1 {
		t.Errorf("seq after restart = %d, want %d", evs[0].Seq, before.LastSeq+1)
	}
	req := p2.Requests()[0]
	if len(req.Items) != len(before.Items)+1 || req.Items[3].Output.Content != "root="+e.ws.Default() {
		t.Errorf("history sent after restart = %+v", req.Items)
	}

	// 全部事件都能从库中重放，seq 连续。
	backlog, sub, gap := NewBus(s.ID(), s2.Bus().LastSeq(), m2.db, nil).Subscribe(0)
	sub.Cancel()
	if gap || int64(len(backlog)) != s2.Bus().LastSeq() {
		t.Fatalf("replay gap=%v len=%d last=%d", gap, len(backlog), s2.Bus().LastSeq())
	}
	for i, ev := range backlog {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq %d at %d", ev.Seq, i)
		}
	}
}

func TestRecoverRunningTurnAfterCrash(t *testing.T) {
	e := newEnv(t)
	db, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC()
	// 模拟进程在执行工具调用 c2 时被杀：c1 有输出，c2 没有，轮次仍是 running。
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.CreateSession(ctx, store.SessionRecord{ID: "sess_x", Cwd: e.ws.Default(), CreatedAt: now, UpdatedAt: now}))
	must(db.StartTurn(ctx, store.TurnRecord{ID: "turn_x", SessionID: "sess_x", Status: protocol.TurnRunning, Input: "go", StartSeq: 1, StartedAt: now}))
	must(db.AppendEvent(ctx, protocol.Event{Seq: 1, SessionID: "sess_x", TurnID: "turn_x", Type: protocol.EventTurnStarted}))
	for _, it := range []protocol.Item{
		{ID: "i1", TurnID: "turn_x", Kind: protocol.ItemUserMessage, Text: "go"},
		{ID: "i2", TurnID: "turn_x", Kind: protocol.ItemToolCall, Call: &protocol.ToolCall{CallID: "c1", Name: "where"}},
		{ID: "i3", TurnID: "turn_x", Kind: protocol.ItemToolCall, Call: &protocol.ToolCall{CallID: "c2", Name: "where"}},
		{ID: "i4", TurnID: "turn_x", Kind: protocol.ItemToolOutput, Output: &protocol.ToolOutput{CallID: "c1", Content: "ok"}},
	} {
		must(db.AppendItem(ctx, "sess_x", it))
	}
	must(db.Close())

	p := llmtest.NewScripted(llmtest.Step{Text: "resumed"})
	m := e.open(t, p)
	defer e.close(m)
	if running, _ := m.db.RunningTurns(ctx); len(running) != 0 {
		t.Errorf("running turns left: %+v", running)
	}
	s, err := m.Get("sess_x")
	if err != nil {
		t.Fatal(err)
	}
	info := s.Info()
	if info.LastTurn != protocol.TurnInterrupted || info.Turns[0].Error != restartError || info.LastSeq != 2 {
		t.Errorf("info = %+v", info)
	}
	n := len(info.Items)
	if n != 6 || info.Items[4].Output.CallID != "c2" || info.Items[4].Output.Content != history.RestartedMessage ||
		info.Items[5].Kind != protocol.ItemTurnAborted || !strings.Contains(info.Items[5].Text, "stopped unexpectedly") {
		t.Fatalf("items = %+v", info.Items)
	}
	evs, _ := m.db.EventsAfter(ctx, "sess_x", 1, 2)
	var d protocol.TurnData
	if len(evs) == 1 {
		_ = json.Unmarshal(evs[0].Data.(json.RawMessage), &d)
	}
	if len(evs) != 1 || evs[0].Type != protocol.EventTurnCompleted || d.Status != protocol.TurnInterrupted {
		t.Errorf("recovery event = %+v", evs)
	}

	// 修复后的历史可以直接用于下一轮。
	runTurn(t, s, "continue")
	if got := len(p.Requests()[0].Items); got != 7 {
		t.Errorf("request items = %d", got)
	}
	e.close(m)

	// 再次启动不会重复修复。
	m3 := e.open(t, llmtest.NewScripted())
	defer e.close(m3)
	s3, _ := m3.Get("sess_x")
	if got := len(s3.Info().Items); got != 8 {
		t.Errorf("items after second restart = %d", got)
	}
}

func TestInterruptPersistsMarker(t *testing.T) {
	e := newEnv(t)
	m := e.open(t, llmtest.NewScripted(llmtest.Step{Text: "partial", Hang: true}, llmtest.Step{Text: "ok"}))
	defer e.close(m)
	s, _ := m.Create("")
	_, sub, _ := s.Bus().Subscribe(0)
	defer sub.Cancel()
	id, _ := s.Submit("long task")
	for ev := range sub.C {
		if ev.Type == protocol.EventItemDelta {
			break
		}
	}
	s.Interrupt(2 * time.Second)
	waitTurn(t, sub, id)

	items, _ := m.db.LoadItems(context.Background(), s.ID())
	if len(items) != 2 || items[0].Kind != protocol.ItemUserMessage || items[1].Kind != protocol.ItemTurnAborted ||
		!strings.Contains(items[1].Text, "interrupted the previous turn on purpose") {
		t.Errorf("stored items = %+v", items)
	}
	turns, _ := m.db.ListTurns(context.Background(), s.ID())
	if len(turns) != 1 || turns[0].Status != protocol.TurnInterrupted || turns[0].EndedAt.IsZero() {
		t.Errorf("stored turns = %+v", turns)
	}
}

func TestPerSessionWorkspace(t *testing.T) {
	e := newEnv(t)
	step := llmtest.Step{Calls: []llmtest.StepCall{{Name: "where"}}}
	p := llmtest.NewScripted(step, llmtest.Step{Text: "a"}, step, llmtest.Step{Text: "b"})
	m := e.open(t, p)
	defer e.close(m)

	dirB := filepath.Join(e.base, "roots", "b")
	sa, err := m.Create("")
	if err != nil {
		t.Fatal(err)
	}
	sb, err := m.Create(dirB)
	if err != nil {
		t.Fatal(err)
	}
	runTurn(t, sa, "where?")
	runTurn(t, sb, "where?")
	reqs := p.Requests()
	if got := reqs[1].Items[2].Output.Content; got != "root="+e.ws.Default() {
		t.Errorf("session a tool root = %s", got)
	}
	if got := reqs[3].Items[2].Output.Content; got != "root="+dirB {
		t.Errorf("session b tool root = %s", got)
	}
	if !strings.Contains(reqs[3].Instructions, dirB) {
		t.Errorf("instructions do not mention cwd: %s", reqs[3].Instructions)
	}

	for dir, want := range map[string]error{
		filepath.Join(e.base, "roots", "outside"): workspace.ErrOutside,
		filepath.Join(e.base, "roots", "missing"): workspace.ErrNotFound,
		"relative/dir": workspace.ErrInvalid,
	} {
		if _, err := m.Create(dir); !errors.Is(err, want) {
			t.Errorf("Create(%s) = %v, want %v", dir, err, want)
		}
	}

	// 目录被删除后，会话仍可查看，但不能再开始新轮次。
	if err := os.RemoveAll(dirB); err != nil {
		t.Fatal(err)
	}
	if _, err := sb.Submit("again"); !errors.Is(err, ErrWorkspaceUnavailable) {
		t.Errorf("submit in deleted workspace: %v", err)
	}

	list, err := m.List(10)
	if err != nil || len(list) != 2 || list[0].ID != sb.ID() || list[0].Cwd != dirB {
		t.Errorf("list = %+v, %v", list, err)
	}
}

func TestMakeTitle(t *testing.T) {
	for in, want := range map[string]string{
		"\n\n  读取   README.md \n其余": "读取 README.md",
		strings.Repeat("长", 50):     strings.Repeat("长", 40) + "…",
		"   ":                       "",
	} {
		if got := makeTitle(in); got != want {
			t.Errorf("makeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSandboxLifecycle(t *testing.T) {
	e := newEnv(t)
	db, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	created := 0
	var seenRoot string
	var seenSandbox *sandbox.Sandbox
	m, err := NewManager(Runtime{
		Provider:   llmtest.NewScripted(llmtest.Step{Text: "a"}, llmtest.Step{Text: "b"}),
		Store:      db,
		Workspaces: e.ws,
		Sandbox: func(root string) (*sandbox.Sandbox, error) {
			created++
			p, err := sandbox.NewPolicy(sandbox.FullAccess, root, sandbox.PolicyOptions{})
			if err != nil {
				return nil, err
			}
			return sandbox.New(p)
		},
		Tools: func(env ToolEnv) (*tools.Router, error) {
			seenRoot, seenSandbox = env.Root, env.Sandbox
			return tools.NewRouter()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := m.Create("")
	if s.Sandbox() != nil {
		t.Error("sandbox created before the first turn")
	}
	runTurn(t, s, "one")
	runTurn(t, s, "two")
	if created != 1 || s.Sandbox() == nil || seenSandbox != s.Sandbox() || seenRoot != e.ws.Default() {
		t.Errorf("created=%d sandbox=%v root=%s", created, s.Sandbox(), seenRoot)
	}
	sb := s.Sandbox()
	m.Close(time.Second)
	if _, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "true"}, nil); !errors.Is(err, sandbox.ErrClosed) {
		t.Errorf("exec after close = %v", err)
	}

	// 沙箱创建失败时提交被拒绝，不留下 running 轮次。
	m2, err := NewManager(Runtime{
		Provider: llmtest.NewScripted(), Store: db, Workspaces: e.ws,
		Sandbox: func(string) (*sandbox.Sandbox, error) { return nil, sandbox.ErrUnsupported },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close(time.Second)
	s2, _ := m2.Create("")
	if _, err := s2.Submit("x"); !errors.Is(err, ErrSandboxUnavailable) {
		t.Errorf("submit = %v", err)
	}
	if running, _ := db.RunningTurns(context.Background()); len(running) != 0 {
		t.Errorf("running turns = %+v", running)
	}
}

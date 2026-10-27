// Package session 管理会话及其轮次生命周期：每个会话同时只有一个活动轮次，
// 还包括取消操作、事件总线和持久化。
//
// 持久化遵循"先落盘，再通知"：条目在发出 item.completed 之前写库，轮次
// 状态在发出 turn.completed 之前写库。重启后恢复的是历史，不是执行现场：
// 上次进程退出时仍在运行的轮次会被标记为 interrupted。
package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/agent"
	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/store"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
	"github.com/Czyyy-Joey/joey-agent/internal/workspace"
)

var (
	// ErrTurnActive 在会话已有运行中的轮次时返回。
	ErrTurnActive = errors.New("session: a turn is already running")
	// ErrNotFound 在会话 id 未知时返回。
	ErrNotFound = errors.New("session: not found")
	// ErrClosed 在管理器关闭后返回。
	ErrClosed = errors.New("session: manager closed")
	// ErrWorkspaceUnavailable 在会话的工作目录已不存在或不再被允许时返回。
	ErrWorkspaceUnavailable = errors.New("session: workspace is not available")
	// errInterrupted 是用户中断触发的取消原因。
	errInterrupted = errors.New("interrupted by user")
)

// RootAgentID 标识会话的顶层 agent。
const RootAgentID = "root"

// restartError 是重启修复时写入被中断轮次的错误信息。
const restartError = "the joey process stopped before this turn finished"

// Runtime 保存所有会话共享的依赖。
type Runtime struct {
	Provider llm.Provider
	// Store 保存会话数据；为 nil 时使用由 Manager 自行关闭的内存数据库。
	Store *store.DB
	// Workspaces 决定会话可以选择的工作目录；为 nil 时只允许进程当前目录。
	Workspaces *workspace.Policy
	// Tools 为工作目录 root 构建工具路由；为 nil 时会话没有工具。
	Tools func(root string) (*tools.Router, error)
	// Instructions 返回 root 下的系统提示词；为 nil 时用 agent.DefaultInstructions。
	Instructions  func(root string) string
	Model         string
	MaxSteps      int
	ContextWindow int
	Logger        *slog.Logger
}

// TurnInfo 是已结束轮次的摘要。
type TurnInfo struct {
	ID     string              `json:"id"`
	Status protocol.TurnStatus `json:"status"`
	Error  string              `json:"error,omitempty"`
}

// Summary 是会话列表中的一项。
type Summary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Status 是 "idle" 或 "running"。
	Status string `json:"status"`
}

// Info 是用于 API 响应的会话快照。
type Info struct {
	Summary
	ActiveTurn string              `json:"active_turn,omitempty"`
	LastTurn   protocol.TurnStatus `json:"last_turn_status,omitempty"`
	// LastSeq 是 Items 完整到的最新 seq；应从它之后订阅。
	LastSeq int64           `json:"last_seq"`
	Items   []protocol.Item `json:"items"`
	// Turns 只包含已结束的轮次，按时间顺序。
	Turns []TurnInfo `json:"turns"`
}

// Session 表示一次对话。
type Session struct {
	id        string
	createdAt time.Time
	cwd       string
	m         *Manager
	bus       *Bus

	mu        sync.Mutex
	title     string
	updatedAt time.Time
	items     []protocol.Item
	turns     []TurnInfo
	active    *activeTurn
}

type activeTurn struct {
	id     string
	cancel context.CancelCauseFunc
	done   chan struct{}
	// startSeq 是 turn.started 事件的 seq。
	startSeq int64
}

// ID 返回会话 id。
func (s *Session) ID() string { return s.id }

// Cwd 返回会话的工作目录。
func (s *Session) Cwd() string { return s.cwd }

// Bus 返回会话的事件总线。
func (s *Session) Bus() *Bus { return s.bus }

// Submit 为 input 启动一个轮次并返回其 id，不等待其完成。
func (s *Session) Submit(input string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m
	if m.ctx.Err() != nil {
		return "", ErrClosed
	}
	if s.active != nil {
		return "", ErrTurnActive
	}
	// 每轮重新校验：目录可能已被删除，或配置已收紧允许范围。
	root, err := m.ws.Resolve(s.cwd)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkspaceUnavailable, err)
	}
	router, err := m.router(root)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWorkspaceUnavailable, err)
	}

	bg := context.Background()
	now := nowMS()
	title := s.title
	if title == "" {
		title = makeTitle(input)
	}
	if err := m.db.UpdateSession(bg, s.id, title, now); err != nil {
		return "", err
	}
	s.title, s.updatedAt = title, now

	turnID := protocol.NewID("turn")
	// 没有活动轮次且持有 s.mu 时，没有别的地方会向总线发布事件，
	// 因此下一个 seq 就是 turn.started 的 seq。
	startSeq := s.bus.LastSeq() + 1
	if err := m.db.StartTurn(bg, store.TurnRecord{
		ID: turnID, SessionID: s.id, Status: protocol.TurnRunning, Input: input, StartSeq: startSeq, StartedAt: now,
	}); err != nil {
		return "", err
	}
	ctx, cancel := context.WithCancelCause(m.ctx)
	at := &activeTurn{id: turnID, cancel: cancel, done: make(chan struct{}), startSeq: startSeq}
	s.active = at
	hist := append([]protocol.Item(nil), s.items...)
	s.publish(protocol.Event{Type: protocol.EventTurnStarted, TurnID: turnID,
		Data: protocol.TurnData{Status: protocol.TurnRunning, Input: input}})
	go s.run(ctx, at, router, root, hist, input)
	return turnID, nil
}

func (s *Session) run(ctx context.Context, at *activeTurn, router *tools.Router, root string, hist []protocol.Item, input string) {
	defer close(at.done)
	defer at.cancel(nil)
	m := s.m
	bg := context.Background()
	env := agent.TurnEnv{
		Provider:      m.rt.Provider,
		Router:        router,
		Instructions:  m.instructions(root),
		TurnID:        at.id,
		MaxSteps:      m.rt.MaxSteps,
		ContextWindow: m.rt.ContextWindow,
		Emit:          func(e protocol.Event) { s.publish(e) },
		Record:        func(it protocol.Item) error { return m.db.AppendItem(bg, s.id, it) },
	}
	res := agent.RunTurn(ctx, env, hist, input)

	// 补齐写库失败时缺少的工具输出。缺口只可能出现在本轮末尾，所以
	// 追加到库里的顺序与内存中一致。
	items, added := history.Normalize(res.Items, history.MissingMessage)
	for _, it := range added {
		if err := m.db.AppendItem(bg, s.id, it); err != nil {
			m.logger.Error("save synthetic tool output", "session", s.id, "err", err)
		}
	}
	if res.Status == protocol.TurnInterrupted {
		reason := history.AbortedByShutdown
		if errors.Is(context.Cause(ctx), errInterrupted) {
			reason = history.AbortedByUser
		}
		marker := history.AbortMarker(at.id, reason)
		if err := m.db.AppendItem(bg, s.id, marker); err != nil {
			m.logger.Error("save abort marker", "session", s.id, "err", err)
		} else {
			items = append(items, marker)
		}
	}

	data := protocol.TurnData{Status: res.Status, FinalMessage: res.FinalMessage, Usage: &res.Usage}
	if res.Err != nil {
		data.Error = res.Err.Error()
	}
	now := nowMS()
	if err := m.db.FinishTurn(bg, store.TurnRecord{ID: at.id, Status: res.Status, Error: data.Error, Usage: res.Usage, EndedAt: now}); err != nil {
		m.logger.Error("finish turn", "session", s.id, "turn", at.id, "err", err)
	}

	// turn.completed 在轮次释放前发布，以确保后续轮次的事件
	// 绝不会排在它之前。
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := m.db.UpdateSession(bg, s.id, s.title, now); err != nil {
		m.logger.Error("update session", "session", s.id, "err", err)
	}
	s.updatedAt = now
	s.items = append(s.items, items...)
	s.turns = append(s.turns, TurnInfo{ID: at.id, Status: res.Status, Error: data.Error})
	s.publish(protocol.Event{Type: protocol.EventTurnCompleted, TurnID: at.id, Data: data})
	s.active = nil
}

// Interrupt 取消正在运行的轮次，最多等待 timeout 让其停止。
// 如果没有正在运行的轮次，则返回 false。
func (s *Session) Interrupt(timeout time.Duration) bool {
	s.mu.Lock()
	at := s.active
	s.mu.Unlock()
	if at == nil {
		return false
	}
	at.cancel(errInterrupted)
	select {
	case <-at.done:
	case <-time.After(timeout):
	}
	return true
}

// Wait 阻塞直到没有轮次运行或 ctx 结束。
func (s *Session) Wait(ctx context.Context) error {
	s.mu.Lock()
	at := s.active
	s.mu.Unlock()
	if at == nil {
		return nil
	}
	select {
	case <-at.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Info 返回会话的快照。
func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := Info{
		Summary: s.summaryLocked(),
		LastSeq: s.bus.LastSeq(),
		Items:   append([]protocol.Item{}, s.items...),
		Turns:   append([]TurnInfo{}, s.turns...),
	}
	if n := len(s.turns); n > 0 {
		info.LastTurn = s.turns[n-1].Status
	}
	if s.active != nil {
		info.ActiveTurn = s.active.id
		// Items 只包含已结束的轮次，因此将客户端指向运行中轮次的
		// 起始位置以重放其事件。
		info.LastSeq = s.active.startSeq - 1
	}
	return info
}

func (s *Session) summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summaryLocked()
}

func (s *Session) summaryLocked() Summary {
	sum := Summary{ID: s.id, Title: s.title, Cwd: s.cwd, CreatedAt: s.createdAt, UpdatedAt: s.updatedAt, Status: "idle"}
	if s.active != nil {
		sum.Status = "running"
	}
	return sum
}

func (s *Session) publish(e protocol.Event) protocol.Event {
	e.SessionID = s.id
	if e.AgentID == "" {
		e.AgentID = RootAgentID
	}
	if e.TS.IsZero() {
		e.TS = nowMS()
	}
	return s.bus.Publish(e)
}

// nowMS 返回截断到毫秒的当前时间，与数据库的时间精度一致，使内存中的
// 快照和重启后加载的快照完全相同。
func nowMS() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// makeTitle 取输入的第一行非空文本作为会话标题。
func makeTitle(input string) string {
	for line := range strings.SplitSeq(input, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > 40 {
			return string(r[:40]) + "…"
		}
		return line
	}
	return ""
}

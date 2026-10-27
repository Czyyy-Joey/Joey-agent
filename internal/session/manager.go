package session

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/agent"
	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/store"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
	"github.com/Czyyy-Joey/joey-agent/internal/workspace"
)

// Manager 创建、加载和列出会话。已加载的会话缓存在内存中。
type Manager struct {
	rt     Runtime
	db     *store.DB
	ownsDB bool
	ws     *workspace.Policy
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager 返回一个其会话共享 rt 的管理器。它会先修复上次进程退出时
// 仍处于 running 的轮次。
func NewManager(rt Runtime) (*Manager, error) {
	m := &Manager{rt: rt, db: rt.Store, ws: rt.Workspaces, logger: rt.Logger, sessions: make(map[string]*Session)}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	if m.ws == nil {
		ws, err := workspace.NewPolicy(".", nil)
		if err != nil {
			return nil, err
		}
		m.ws = ws
	}
	if m.db == nil {
		db, err := store.Open("")
		if err != nil {
			return nil, err
		}
		m.db, m.ownsDB = db, true
	}
	if err := m.recoverTurns(); err != nil {
		if m.ownsDB {
			m.db.Close()
		}
		return nil, err
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

// Workspaces 返回工作目录策略。
func (m *Manager) Workspaces() *workspace.Policy { return m.ws }

// recoverTurns 把上次进程留下的 running 轮次收尾：补齐缺失的工具输出、
// 追加中断标记、写入 interrupted 状态和一条 turn.completed 事件，使
// 历史和事件流都和正常中断时一样完整。
func (m *Manager) recoverTurns() error {
	ctx := context.Background()
	running, err := m.db.RunningTurns(ctx)
	if err != nil {
		return err
	}
	for _, t := range running {
		items, err := m.db.LoadItems(ctx, t.SessionID)
		if err != nil {
			return err
		}
		_, added := history.Normalize(items, history.RestartedMessage)
		added = append(added, history.AbortMarker(t.ID, history.AbortedByRestart))
		for _, it := range added {
			if err := m.db.AppendItem(ctx, t.SessionID, it); err != nil {
				return err
			}
		}
		now := nowMS()
		if err := m.db.FinishTurn(ctx, store.TurnRecord{
			ID: t.ID, Status: protocol.TurnInterrupted, Error: restartError, Usage: t.Usage, EndedAt: now,
		}); err != nil {
			return err
		}
		seq, err := m.db.LastEventSeq(ctx, t.SessionID)
		if err != nil {
			return err
		}
		if err := m.db.AppendEvent(ctx, protocol.Event{
			Seq: seq + 1, SessionID: t.SessionID, AgentID: RootAgentID, TurnID: t.ID,
			Type: protocol.EventTurnCompleted, TS: now,
			Data: protocol.TurnData{Status: protocol.TurnInterrupted, Error: restartError},
		}); err != nil {
			return err
		}
		m.logger.Warn("recovered interrupted turn", "session", t.SessionID, "turn", t.ID, "synthetic_outputs", len(added)-1)
	}
	return nil
}

// Create 在工作目录 cwd 中创建一个新的空会话；cwd 为 "" 时使用默认目录。
// cwd 不合法时返回 workspace 包的错误。
func (m *Manager) Create(cwd string) (*Session, error) {
	root, err := m.ws.Resolve(cwd)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return nil, ErrClosed
	}
	now := nowMS()
	rec := store.SessionRecord{ID: protocol.NewID("sess"), Cwd: root, Model: m.rt.Model, CreatedAt: now, UpdatedAt: now}
	if err := m.db.CreateSession(context.Background(), rec); err != nil {
		return nil, err
	}
	s := m.newSession(rec, nil, nil, 0)
	m.sessions[s.id] = s
	return s, nil
}

// Get 返回 id 对应的会话，必要时从数据库加载。
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		return s, nil
	}
	if m.ctx.Err() != nil {
		return nil, ErrClosed
	}
	ctx := context.Background()
	rec, err := m.db.GetSession(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	items, err := m.db.LoadItems(ctx, id)
	if err != nil {
		return nil, err
	}
	// 库中的历史在写入时已经规范化；这里再检查一次，防止异常数据让
	// 下一次模型请求被拒绝。
	if norm, added := history.Normalize(items, history.MissingMessage); len(added) > 0 {
		m.logger.Warn("stored history was not normalized", "session", id, "synthetic_outputs", len(added))
		items = norm
	}
	recs, err := m.db.ListTurns(ctx, id)
	if err != nil {
		return nil, err
	}
	turns := make([]TurnInfo, 0, len(recs))
	for _, t := range recs {
		turns = append(turns, TurnInfo{ID: t.ID, Status: t.Status, Error: t.Error})
	}
	lastSeq, err := m.db.LastEventSeq(ctx, id)
	if err != nil {
		return nil, err
	}
	s := m.newSession(rec, items, turns, lastSeq)
	m.sessions[id] = s
	return s, nil
}

func (m *Manager) newSession(rec store.SessionRecord, items []protocol.Item, turns []TurnInfo, lastSeq int64) *Session {
	return &Session{
		id:        rec.ID,
		createdAt: rec.CreatedAt,
		cwd:       rec.Cwd,
		m:         m,
		bus:       NewBus(rec.ID, lastSeq, m.db, m.logger),
		title:     rec.Title,
		updatedAt: rec.UpdatedAt,
		items:     items,
		turns:     turns,
	}
}

// List 按最近更新时间返回最多 limit 个会话。
func (m *Manager) List(limit int) ([]Summary, error) {
	recs, err := m.db.ListSessions(context.Background(), limit)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	loaded := make(map[string]*Session, len(m.sessions))
	for id, s := range m.sessions {
		loaded[id] = s
	}
	m.mu.Unlock()
	out := make([]Summary, 0, len(recs))
	for _, r := range recs {
		if s, ok := loaded[r.ID]; ok {
			out = append(out, s.summary())
			continue
		}
		out = append(out, Summary{ID: r.ID, Title: r.Title, Cwd: r.Cwd, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Status: "idle"})
	}
	return out, nil
}

func (m *Manager) router(root string) (*tools.Router, error) {
	if m.rt.Tools == nil {
		return tools.NewRouter()
	}
	return m.rt.Tools(root)
}

func (m *Manager) instructions(root string) string {
	if m.rt.Instructions == nil {
		return agent.DefaultInstructions(root)
	}
	return m.rt.Instructions(root)
}

// Close 取消所有正在运行的轮次，最多等待 timeout，并关闭所有事件总线。
// 超时仍未结束的轮次在库中保持 running，下次启动时被修复。
func (m *Manager) Close(timeout time.Duration) {
	m.mu.Lock()
	m.cancel()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, s := range sessions {
		_ = s.Wait(ctx)
		s.bus.Close()
	}
	if m.ownsDB {
		m.db.Close()
	}
}

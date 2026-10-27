package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// AppendItem 把 it 追加到会话历史的末尾。
func (s *DB) AppendItem(ctx context.Context, sessionID string, it protocol.Item) error {
	payload, err := json.Marshal(it)
	if err != nil {
		return wrap("append item", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO items (session_id, id, turn_id, kind, payload_json) VALUES (?, ?, ?, ?, ?)`,
		sessionID, it.ID, it.TurnID, string(it.Kind), string(payload))
	return wrap("append item", err)
}

// LoadItems 按追加顺序返回会话的全部历史条目。
func (s *DB) LoadItems(ctx context.Context, sessionID string) ([]protocol.Item, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT payload_json FROM items WHERE session_id = ? ORDER BY rowid`, sessionID)
	if err != nil {
		return nil, wrap("load items", err)
	}
	defer rows.Close()
	var out []protocol.Item
	for rows.Next() {
		var payload string
		var it protocol.Item
		if err := rows.Scan(&payload); err != nil {
			return nil, wrap("load items", err)
		}
		if err := json.Unmarshal([]byte(payload), &it); err != nil {
			return nil, wrap("load items", err)
		}
		out = append(out, it)
	}
	return out, wrap("load items", rows.Err())
}

// storedEvent 是 events.payload_json 的内容：信封中除了已有独立列的字段。
type storedEvent struct {
	AgentID string          `json:"agent_id"`
	ItemID  string          `json:"item_id,omitempty"`
	TS      int64           `json:"ts"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// AppendEvent 持久化 e。e.Seq 由调用方分配，同一会话内不能重复。
func (s *DB) AppendEvent(ctx context.Context, e protocol.Event) error {
	var data json.RawMessage
	if e.Data != nil {
		b, err := json.Marshal(e.Data)
		if err != nil {
			return wrap("append event", err)
		}
		data = b
	}
	payload, err := json.Marshal(storedEvent{AgentID: e.AgentID, ItemID: e.ItemID, TS: e.TS.UnixMicro(), Data: data})
	if err != nil {
		return wrap("append event", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO events (session_id, seq, turn_id, type, payload_json) VALUES (?, ?, ?, ?, ?)`,
		e.SessionID, e.Seq, e.TurnID, string(e.Type), string(payload))
	return wrap("append event", err)
}

// EventsAfter 返回 after < seq <= upTo 的事件，按 seq 升序。
// 返回事件的 Data 是 json.RawMessage。
func (s *DB) EventsAfter(ctx context.Context, sessionID string, after, upTo int64) ([]protocol.Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, turn_id, type, payload_json FROM events WHERE session_id = ? AND seq > ? AND seq <= ? ORDER BY seq`,
		sessionID, after, upTo)
	if err != nil {
		return nil, wrap("events after", err)
	}
	defer rows.Close()
	var out []protocol.Event
	for rows.Next() {
		var e protocol.Event
		var typ, payload string
		if err := rows.Scan(&e.Seq, &e.TurnID, &typ, &payload); err != nil {
			return nil, wrap("events after", err)
		}
		var se storedEvent
		if err := json.Unmarshal([]byte(payload), &se); err != nil {
			return nil, wrap("events after", err)
		}
		e.SessionID, e.Type, e.AgentID, e.ItemID = sessionID, protocol.EventType(typ), se.AgentID, se.ItemID
		e.TS = timeFromMicro(se.TS)
		if len(se.Data) > 0 {
			e.Data = se.Data
		}
		out = append(out, e)
	}
	return out, wrap("events after", rows.Err())
}

// LastEventSeq 返回会话中最大的事件 seq，没有事件时返回 0。
func (s *DB) LastEventSeq(ctx context.Context, sessionID string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events WHERE session_id = ?`, sessionID).Scan(&seq)
	return seq, wrap("last event seq", err)
}

// StartTurn 插入一个新轮次。
func (s *DB) StartTurn(ctx context.Context, t TurnRecord) error {
	usage, _ := json.Marshal(t.Usage)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO turns (id, session_id, status, input, error, start_seq, usage_json, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.SessionID, string(t.Status), t.Input, t.Error, t.StartSeq, string(usage), ms(t.StartedAt))
	return wrap("start turn", err)
}

// FinishTurn 写入轮次的最终状态、错误、用量和结束时间。
func (s *DB) FinishTurn(ctx context.Context, t TurnRecord) error {
	usage, _ := json.Marshal(t.Usage)
	res, err := s.db.ExecContext(ctx,
		`UPDATE turns SET status = ?, error = ?, usage_json = ?, ended_at = ? WHERE id = ?`,
		string(t.Status), t.Error, string(usage), ms(t.EndedAt), t.ID)
	if err != nil {
		return wrap("finish turn", err)
	}
	return expectOne(res)
}

const turnCols = `id, session_id, status, input, error, start_seq, usage_json, started_at, ended_at`

func (s *DB) queryTurns(ctx context.Context, query string, args ...any) ([]TurnRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+turnCols+` FROM turns `+query, args...)
	if err != nil {
		return nil, wrap("query turns", err)
	}
	defer rows.Close()
	var out []TurnRecord
	for rows.Next() {
		var t TurnRecord
		var status, usage string
		var started int64
		var ended sql.NullInt64
		if err := rows.Scan(&t.ID, &t.SessionID, &status, &t.Input, &t.Error, &t.StartSeq, &usage, &started, &ended); err != nil {
			return nil, wrap("query turns", err)
		}
		t.Status = protocol.TurnStatus(status)
		_ = json.Unmarshal([]byte(usage), &t.Usage)
		t.StartedAt = fromMS(started)
		if ended.Valid {
			t.EndedAt = fromMS(ended.Int64)
		}
		out = append(out, t)
	}
	return out, wrap("query turns", rows.Err())
}

// ListTurns 按开始顺序返回会话的全部轮次。
func (s *DB) ListTurns(ctx context.Context, sessionID string) ([]TurnRecord, error) {
	return s.queryTurns(ctx, `WHERE session_id = ? ORDER BY started_at, rowid`, sessionID)
}

// RunningTurns 返回所有状态仍为 running 的轮次。进程正常运行时它们都有
// 对应的 goroutine；启动时还存在的则是上次进程异常退出留下的。
func (s *DB) RunningTurns(ctx context.Context) ([]TurnRecord, error) {
	return s.queryTurns(ctx, `WHERE status = ? ORDER BY started_at, rowid`, string(protocol.TurnRunning))
}

func expectOne(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func wrap(op string, err error) error {
	if err == nil || errors.Is(err, ErrNotFound) {
		return err
	}
	return fmt.Errorf("store: %s: %w", op, err)
}

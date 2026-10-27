// Package store 用 SQLite 持久化会话、轮次、历史条目和事件。
//
// 所有写入走同一个连接（SetMaxOpenConns(1)），因此 SQLite 的写锁不会
// 在进程内部发生冲突；跨进程的并发由数据库旁边的锁文件阻止。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// ErrNotFound 在记录不存在时返回。
var ErrNotFound = errors.New("store: not found")

// SessionRecord 是 sessions 表的一行。
type SessionRecord struct {
	ID        string
	Title     string
	Cwd       string
	Model     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// TurnRecord 是 turns 表的一行。EndedAt 为零值表示轮次尚未结束。
type TurnRecord struct {
	ID        string
	SessionID string
	Status    protocol.TurnStatus
	Input     string
	Error     string
	// StartSeq 是该轮次 turn.started 事件的 seq。
	StartSeq  int64
	Usage     protocol.Usage
	StartedAt time.Time
	EndedAt   time.Time
}

// DB 是 SQLite 存储。
type DB struct {
	db     *sql.DB
	unlock func()
}

// Open 打开（必要时创建）path 处的数据库并执行迁移。path 为 "" 时使用
// 仅存在于本进程内存中的数据库。同一个文件同时只能被一个进程打开。
func Open(path string) (*DB, error) {
	dsn := "file::memory:"
	unlock := func() {}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		var err error
		if unlock, err = lockFile(path + ".lock"); err != nil {
			return nil, err
		}
		dsn = "file:" + path
	}
	dsn += "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("store: %w", err)
	}
	// 内存数据库只存在于创建它的连接上，文件数据库则借此串行化写入。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	s := &DB{db: db, unlock: unlock}
	if err := s.migrate(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库并释放锁文件。
func (s *DB) Close() error {
	err := s.db.Close()
	s.unlock()
	return err
}

func (s *DB) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema v%d is newer than this binary (v%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// migrations[i] 把 schema 从 v(i) 升级到 v(i+1)。只能追加，不能修改。
var migrations = []string{
	`CREATE TABLE sessions (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL DEFAULT '',
		cwd        TEXT NOT NULL,
		model      TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	);
	CREATE INDEX sessions_updated ON sessions(updated_at DESC);

	CREATE TABLE turns (
		id         TEXT PRIMARY KEY,
		session_id TEXT NOT NULL REFERENCES sessions(id),
		status     TEXT NOT NULL,
		input      TEXT NOT NULL,
		error      TEXT NOT NULL DEFAULT '',
		start_seq  INTEGER NOT NULL,
		usage_json TEXT NOT NULL DEFAULT '{}',
		started_at INTEGER NOT NULL,
		ended_at   INTEGER
	);
	CREATE INDEX turns_session ON turns(session_id, started_at);
	CREATE INDEX turns_status ON turns(status);

	-- rowid 决定历史顺序。
	CREATE TABLE items (
		session_id   TEXT NOT NULL REFERENCES sessions(id),
		id           TEXT NOT NULL,
		turn_id      TEXT NOT NULL,
		kind         TEXT NOT NULL,
		payload_json TEXT NOT NULL
	);
	CREATE INDEX items_session ON items(session_id);

	CREATE TABLE events (
		session_id   TEXT NOT NULL REFERENCES sessions(id),
		seq          INTEGER NOT NULL,
		turn_id      TEXT NOT NULL DEFAULT '',
		type         TEXT NOT NULL,
		payload_json TEXT NOT NULL,
		PRIMARY KEY (session_id, seq)
	) WITHOUT ROWID;`,
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func timeFromMicro(v int64) time.Time { return time.UnixMicro(v).UTC() }

// CreateSession 插入一个新会话。
func (s *DB) CreateSession(ctx context.Context, r SessionRecord) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, cwd, model, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.Title, r.Cwd, r.Model, ms(r.CreatedAt), ms(r.UpdatedAt))
	return wrap("create session", err)
}

// UpdateSession 修改会话的标题和更新时间。
func (s *DB) UpdateSession(ctx context.Context, id, title string, updatedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`, title, ms(updatedAt), id)
	if err != nil {
		return wrap("update session", err)
	}
	return expectOne(res)
}

const sessionCols = `id, title, cwd, model, created_at, updated_at`

func scanSession(sc interface{ Scan(...any) error }) (SessionRecord, error) {
	var r SessionRecord
	var created, updated int64
	err := sc.Scan(&r.ID, &r.Title, &r.Cwd, &r.Model, &created, &updated)
	r.CreatedAt, r.UpdatedAt = fromMS(created), fromMS(updated)
	return r, err
}

// GetSession 返回 id 对应的会话，不存在时返回 ErrNotFound。
func (s *DB) GetSession(ctx context.Context, id string) (SessionRecord, error) {
	r, err := scanSession(s.db.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, wrap("get session", err)
}

// ListSessions 按更新时间倒序返回最多 limit 个会话。
func (s *DB) ListSessions(ctx context.Context, limit int) ([]SessionRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions ORDER BY updated_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, wrap("list sessions", err)
	}
	defer rows.Close()
	var out []SessionRecord
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, wrap("list sessions", err)
		}
		out = append(out, r)
	}
	return out, wrap("list sessions", rows.Err())
}

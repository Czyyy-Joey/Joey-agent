// Package server 通过 HTTP 暴露会话：用 REST 处理命令，用 SSE 传输
// 事件流。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/dialog"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/session"
	"github.com/Czyyy-Joey/joey-agent/internal/workspace"
)

// maxBodyBytes 限制请求体大小。
const maxBodyBytes = 1 << 20

// Options 配置 Server。
type Options struct {
	Manager *session.Manager
	// AuthToken 在设置后必须通过 "Authorization: Bearer <token>" 或
	// ?token= 提供（EventSource 无法设置请求头）。
	AuthToken string
	// AllowedOrigins 列出除回环地址外额外允许的浏览器来源。
	AllowedOrigins []string
	// Static 提供构建后的前端；为 nil 时禁用。
	Static fs.FS
	// InterruptTimeout 限制 POST /interrupt 等待轮次停止的时长。
	InterruptTimeout time.Duration
	// Heartbeat 是 SSE 保活间隔。
	Heartbeat time.Duration
	// PickFolder 在服务器桌面上弹出原生目录选择器（dialog.PickFolder）；
	// 为 nil 时 POST /api/workspace/pick 返回 501，前端改用内置的目录浏览。
	PickFolder func(ctx context.Context, prompt, start string) (string, error)
	Logger     *slog.Logger
}

// Server 是 HTTP API。
type Server struct {
	opts Options
	mux  *http.ServeMux
	// picking 保证同一时间只有一个原生对话框。
	picking sync.Mutex
}

// New 构建处理器树。
func New(opts Options) *Server {
	if opts.InterruptTimeout <= 0 {
		opts.InterruptTimeout = 2 * time.Second
	}
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 15 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /api/sessions", s.listSessions)
	s.mux.HandleFunc("POST /api/sessions", s.createSession)
	s.mux.HandleFunc("GET /api/workspace/dirs", s.listDirs)
	s.mux.HandleFunc("POST /api/workspace/pick", s.pickFolder)
	s.mux.HandleFunc("GET /api/sessions/{id}", s.getSession)
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	s.mux.HandleFunc("POST /api/sessions/{id}/turns", s.submitTurn)
	s.mux.HandleFunc("POST /api/sessions/{id}/interrupt", s.interrupt)
	s.mux.HandleFunc("POST /api/sessions/{id}/approvals/{approval}", s.resolveApproval)
	s.mux.HandleFunc("GET /api/sessions/{id}/events", s.events)
	if opts.Static != nil {
		s.mux.Handle("GET /", spaHandler(opts.Static))
	}
	return s
}

// ServeHTTP 在路由前执行来源和鉴权检查。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if !s.originAllowed(r) {
			writeError(w, http.StatusForbidden, "origin not allowed")
			return
		}
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "missing or invalid token")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// originAllowed 阻止跨站浏览器请求。该 API 可以读取文件（之后还会执行
// 命令），因此任意网页都不能驱动它。
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	for _, o := range s.opts.AllowedOrigins {
		if origin == o {
			return true
		}
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) authorized(r *http.Request) bool {
	if s.opts.AuthToken == "" {
		return true
	}
	if v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && v == s.opts.AuthToken {
		return true
	}
	return r.URL.Query().Get("token") == s.opts.AuthToken
}

// decodeBody 严格解析 JSON 请求体；allowEmpty 时空请求体视为 {}。
func decodeBody(w http.ResponseWriter, r *http.Request, v any, allowEmpty bool) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !(allowEmpty && errors.Is(err, io.EOF)) {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	list, err := s.opts.Manager.List(limit)
	if err != nil {
		s.internalError(w, "list sessions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd string `json:"cwd"`
	}
	if !decodeBody(w, r, &body, true) {
		return
	}
	sess, err := s.opts.Manager.Create(body.Cwd)
	switch {
	case errors.Is(err, session.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil && workspaceError(w, err):
	case err != nil:
		s.internalError(w, "create session", err)
	default:
		writeJSON(w, http.StatusCreated, sess.Info())
	}
}

// workspaceError 把工作目录校验错误写成 4xx 响应；不是这类错误时返回 false。
func workspaceError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, workspace.ErrOutside):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, workspace.ErrInvalid), errors.Is(err, workspace.ErrNotFound):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		return false
	}
	return true
}

func (s *Server) listDirs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	l, err := s.opts.Manager.Workspaces().List(q.Get("path"), q.Get("hidden") == "1" || q.Get("hidden") == "true")
	if err != nil {
		if !workspaceError(w, err) {
			s.internalError(w, "list dirs", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// pickFolderTimeout 是原生对话框最多保持打开的时间。
const pickFolderTimeout = 10 * time.Minute

// pickFolder 弹出原生目录选择器并返回校验后的路径。对话框出现在运行 joey
// 的机器上，所以只接受来自本机的请求；远程客户端得到 501 并改用目录浏览。
func (s *Server) pickFolder(w http.ResponseWriter, r *http.Request) {
	if s.opts.PickFolder == nil {
		writeError(w, http.StatusNotImplemented, "native folder dialog is disabled")
		return
	}
	if !isLoopbackAddr(r.RemoteAddr) {
		writeError(w, http.StatusNotImplemented, "native folder dialog is only available when the browser runs on the same machine as joey")
		return
	}
	var body struct {
		Start string `json:"start"`
	}
	if !decodeBody(w, r, &body, true) {
		return
	}
	policy := s.opts.Manager.Workspaces()
	start, err := policy.Resolve(body.Start)
	if err != nil {
		start = policy.Default()
	}
	if !s.picking.TryLock() {
		writeError(w, http.StatusConflict, "a folder dialog is already open")
		return
	}
	defer s.picking.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), pickFolderTimeout)
	defer cancel()
	picked, err := s.opts.PickFolder(ctx, "选择 Joey 会话的工作目录", start)
	switch {
	case errors.Is(err, dialog.ErrCancelled), errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
		return
	case errors.Is(err, dialog.ErrUnsupported):
		writeError(w, http.StatusNotImplemented, err.Error())
		return
	case r.Context().Err() != nil:
		return
	case err != nil:
		s.internalError(w, "pick folder", err)
		return
	}
	resolved, err := policy.Resolve(picked)
	if err != nil {
		if !workspaceError(w, err) {
			s.internalError(w, "pick folder", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": resolved})
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) internalError(w http.ResponseWriter, op string, err error) {
	s.opts.Logger.Error(op, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
	sess, err := s.opts.Manager.Get(r.PathValue("id"))
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, session.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		s.internalError(w, "load session", err)
	default:
		return sess, true
	}
	return nil, false
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	if sess, ok := s.lookup(w, r); ok {
		writeJSON(w, http.StatusOK, sess.Info())
	}
}

// deleteSession 删除会话。会话正在运行时返回 409，需要先中断。
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	err := s.opts.Manager.Delete(r.PathValue("id"))
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, session.ErrNotFound):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, session.ErrTurnActive):
		writeError(w, http.StatusConflict, "a turn is running in this session; interrupt it first")
	case errors.Is(err, session.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		s.internalError(w, "delete session", err)
	}
}

func (s *Server) submitTurn(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decodeBody(w, r, &body, false) {
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	// 输入 /compact 时只压缩上下文，和 joey chat 一致。
	submit := sess.Submit
	if strings.TrimSpace(body.Text) == session.CompactCommand {
		submit = func(string) (string, error) { return sess.Compact() }
	}
	turnID, err := submit(body.Text)
	switch {
	case errors.Is(err, session.ErrTurnActive):
		writeError(w, http.StatusConflict, "a turn is already running in this session")
	case errors.Is(err, session.ErrWorkspaceUnavailable):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, session.ErrSandboxUnavailable):
		s.opts.Logger.Error("create sandbox", "err", err)
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, session.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		s.internalError(w, "submit turn", err)
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"turn_id": turnID})
	}
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.lookup(w, r)
	if !ok {
		return
	}
	interrupted := sess.Interrupt(s.opts.InterruptTimeout)
	writeJSON(w, http.StatusOK, map[string]bool{"interrupted": interrupted})
}

// resolveApproval 记录用户对一次审批的决定：
// {"decision": "approved" | "approved_for_session" | "denied" | "abort"}。
func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.lookup(w, r)
	if !ok {
		return
	}
	var body struct {
		Decision protocol.ReviewDecision `json:"decision"`
	}
	if !decodeBody(w, r, &body, false) {
		return
	}
	switch err := sess.Resolve(r.PathValue("approval"), body.Decision); {
	case errors.Is(err, session.ErrInvalidDecision):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, session.ErrApprovalNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		s.internalError(w, "resolve approval", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"decision": string(body.Decision)})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// spaHandler 从 fsys 提供文件，并对客户端路由回退到 index.html。
func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(fsys, p); err != nil {
				r = r.Clone(r.Context())
				r.URL.Path = "/"
			}
		}
		files.ServeHTTP(w, r)
	})
}

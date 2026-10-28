// Command joey 将 Joey agent 作为 HTTP 服务器或交互式 CLI 运行。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/agent"
	"github.com/Czyyy-Joey/joey-agent/internal/config"
	"github.com/Czyyy-Joey/joey-agent/internal/dialog"
	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/openaicompat"
	"github.com/Czyyy-Joey/joey-agent/internal/prompt"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
	"github.com/Czyyy-Joey/joey-agent/internal/server"
	"github.com/Czyyy-Joey/joey-agent/internal/session"
	"github.com/Czyyy-Joey/joey-agent/internal/store"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
	"github.com/Czyyy-Joey/joey-agent/internal/tools/builtin"
	"github.com/Czyyy-Joey/joey-agent/internal/workspace"
)

const usage = `Usage:
  joey serve [flags]           run the HTTP API and web UI
  joey chat  [flags] [prompt]  chat in the terminal (reads stdin lines when no prompt)
  joey sandbox exec [flags] -- <command>   run a shell command in the configured sandbox
  joey sandbox info [flags]                show the sandbox policy and the generated Seatbelt profile

Run "joey <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "chat":
		err = runChat(os.Args[2:])
	case "sandbox":
		err = runSandbox(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	var code exitCode
	if errors.As(err, &code) {
		os.Exit(int(code))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "joey:", err)
		os.Exit(1)
	}
}

type commonFlags struct {
	configPath string
	provider   string
	script     string
	workspace  string
	db         string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.configPath, "config", "", "path to a TOML config file (default: joey.toml if present)")
	fs.StringVar(&c.provider, "provider", "oneapi", "model provider: oneapi | scripted")
	fs.StringVar(&c.script, "script", "", "scenario JSON for -provider=scripted")
	fs.StringVar(&c.workspace, "workspace", "", "default working directory for new sessions (overrides config)")
	fs.StringVar(&c.db, "db", "", `SQLite database path, or ":memory:" (overrides config)`)
}

// setup 加载配置并构建会话管理器。调用方负责 Close 管理器和数据库。
func (c *commonFlags) setup(logger *slog.Logger) (config.Config, *session.Manager, *store.DB, error) {
	path := c.configPath
	if path == "" {
		if _, err := os.Stat("joey.toml"); err == nil {
			path = "joey.toml"
		}
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		return cfg, nil, nil, err
	}
	if c.workspace != "" {
		cfg.Workspace.Root = c.workspace
	}
	if c.db != "" {
		cfg.Store.Path = c.db
	}
	ws, err := workspace.NewPolicy(cfg.Workspace.Root, cfg.Workspace.AllowedRoots)
	if err != nil {
		return cfg, nil, nil, err
	}
	// 尽早发现 writable_roots 写错之类的问题，而不是等到第一次提交。
	if _, err := cfg.SandboxPolicy(ws.Default()); err != nil {
		return cfg, nil, nil, err
	}

	var provider llm.Provider
	switch c.provider {
	case "oneapi":
		if err := cfg.RequireAPI(); err != nil {
			return cfg, nil, nil, err
		}
		provider, err = openaicompat.New(openaicompat.Config{
			BaseURL:    cfg.Provider.BaseURL,
			APIKey:     cfg.Provider.APIKey,
			Model:      cfg.ModelInfo(),
			MaxRetries: cfg.Provider.MaxRetries,
			HTTPClient: &http.Client{Timeout: time.Duration(cfg.Provider.TimeoutSeconds) * time.Second},
		})
	case "scripted":
		if c.script == "" {
			return cfg, nil, nil, errors.New("-provider=scripted requires -script")
		}
		provider, err = llmtest.LoadScript(c.script)
	default:
		err = fmt.Errorf("unknown provider %q", c.provider)
	}
	if err != nil {
		return cfg, nil, nil, err
	}

	dbPath := workspace.ExpandHome(cfg.Store.Path)
	if dbPath == ":memory:" {
		dbPath = ""
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return cfg, nil, nil, err
	}
	mgr, err := session.NewManager(session.Runtime{
		Provider:   provider,
		Store:      db,
		Workspaces: ws,
		Sandbox: func(root string) (*sandbox.Sandbox, error) {
			p, err := cfg.SandboxPolicy(root)
			if err != nil {
				return nil, err
			}
			return sandbox.New(p)
		},
		Tools: func(env session.ToolEnv) (*tools.Router, error) {
			// 文件工具与命令共用会话沙箱的策略，路径守卫是强制的。
			mode, rules := cfg.SandboxRules()
			return tools.NewRouter(builtin.Tools(builtin.Env{
				WS:       builtin.WorkspaceFromGuard(sandbox.NewGuard(env.Sandbox.Policy())),
				Sandbox:  env.Sandbox,
				Approval: mode,
				Rules:    rules,
			})...)
		},
		Instructions:     func(root string) string { return instructions(cfg, root) },
		ApprovalTimeout:  time.Duration(cfg.Agent.ApprovalTimeoutSeconds) * time.Second,
		Model:            cfg.Model.Name,
		MaxSteps:         cfg.Agent.MaxSteps,
		ContextWindow:    cfg.Model.ContextWindow,
		AutoCompactLimit: history.AutoCompactLimit(cfg.Model.ContextWindow, cfg.Model.AutoCompactTokenLimit),
		ToolOutputTokens: cfg.Model.ToolOutputTokens,
		Logger:           logger,

		PostTurnCompactPercent:   cfg.Model.PostTurnCompactPercent,
		BackgroundCompactPercent: cfg.Model.BackgroundCompactPercent,
	})
	if err != nil {
		db.Close()
		return cfg, nil, nil, err
	}
	return cfg, mgr, db, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	addr := fs.String("addr", "", "listen address (overrides config)")
	webDir := fs.String("web", "web/dist", "directory with the built web UI; empty disables it")
	nativeDialog := fs.Bool("native-dialog", true, "let local browsers open the OS folder picker on this machine")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, mgr, db, err := common.setup(logger)
	if err != nil {
		return err
	}
	defer db.Close()
	if *addr != "" {
		cfg.Server.Addr = *addr
	}
	if !isLoopback(cfg.Server.Addr) && cfg.Server.AuthToken == "" {
		mgr.Close(0)
		return fmt.Errorf("refusing to listen on %s without %s: the API can read workspace files", cfg.Server.Addr, cfg.Server.AuthTokenEnv)
	}

	opts := server.Options{
		Manager:        mgr,
		AuthToken:      cfg.Server.AuthToken,
		AllowedOrigins: cfg.Server.AllowedOrigins,
		Logger:         logger,
	}
	if *nativeDialog {
		opts.PickFolder = dialog.PickFolder
	}
	if *webDir != "" {
		if info, err := os.Stat(filepath.Join(*webDir, "index.html")); err == nil && !info.IsDir() {
			opts.Static = os.DirFS(*webDir)
		} else {
			logger.Warn("web UI not built; serving API only", "dir", *webDir)
		}
	}

	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		mgr.Close(0)
		return err
	}
	srv := &http.Server{Handler: server.New(opts), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if cfg.Sandbox.Mode != string(sandbox.FullAccess) && !sandbox.Available() {
		logger.Warn("macOS Seatbelt is not available; turns will fail until sandbox.mode is danger-full-access", "err", sandbox.ErrUnsupported)
	}
	logger.Info("joey listening", "addr", "http://"+ln.Addr().String(), "provider", common.provider, "model", cfg.Model.Name,
		"db", cfg.Store.Path, "default_workspace", mgr.Workspaces().Default(), "allowed_roots", mgr.Workspaces().Roots())
	// e2e 测试读取这一行以发现端口。
	fmt.Fprintf(os.Stdout, "listening on http://%s\n", ln.Addr())

	select {
	case err := <-errc:
		mgr.Close(3 * time.Second)
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	mgr.Close(3 * time.Second)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// instructions 为工作目录 root 装配系统提示词：基础指令、环境上下文和
// 项目指令（deny_read 覆盖的文件不读）。
func instructions(cfg config.Config, root string) string {
	mode, _ := cfg.SandboxRules()
	env := prompt.Env{
		Workspace: root, Shell: "/bin/sh", SandboxMode: cfg.Sandbox.Mode, Network: cfg.Sandbox.Network,
		Approval: string(mode), Date: time.Now(),
	}
	var docs []prompt.Doc
	if cfg.Agent.ProjectDocs {
		var guard *sandbox.Guard
		if p, err := cfg.SandboxPolicy(root); err == nil {
			guard = sandbox.NewGuard(p)
		}
		for _, d := range prompt.LoadDocs(root, workspace.ExpandHome("~")) {
			if guard == nil || !guard.Denied(d.Path) {
				docs = append(docs, d)
			}
		}
	}
	return prompt.Build(agent.BaseInstructions, env, docs)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func runChat(args []string) error {
	fs := flag.NewFlagSet("chat", flag.ExitOnError)
	var common commonFlags
	common.register(fs)
	resume := fs.String("session", "", "continue an existing session instead of creating one")
	cwd := fs.String("cwd", "", "working directory for the new session (default: -workspace)")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(logger)
	_, mgr, db, err := common.setup(logger)
	if err != nil {
		return err
	}
	defer db.Close()
	defer mgr.Close(2 * time.Second)

	var sess *session.Session
	if *resume != "" {
		sess, err = mgr.Get(*resume)
	} else {
		sess, err = mgr.Create(*cwd)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "session %s · %s\n", sess.ID(), sess.Cwd())
	return chatLoop(sess, fs.Args(), os.Stdin, os.Stdout)
}

// Package config 从 TOML 文件和环境加载 Joey 的配置。
// API 密钥可以写在文件里，也可以放在环境变量中；其他机密只从环境变量读取。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
)

// Config 是完整的应用配置。
type Config struct {
	Provider  Provider  `toml:"provider"`
	Model     Model     `toml:"model"`
	Agent     Agent     `toml:"agent"`
	Server    Server    `toml:"server"`
	Workspace Workspace `toml:"workspace"`
	Store     Store     `toml:"store"`
	Sandbox   Sandbox   `toml:"sandbox"`
}

// Provider 是模型网关。
type Provider struct {
	// BaseURL 包含版本前缀，例如 "https://host/v1"。
	BaseURL string `toml:"base_url"`
	// APIKeyEnv 指定保存 API 密钥的环境变量名。该变量非空时优先于 APIKey。
	APIKeyEnv  string `toml:"api_key_env"`
	MaxRetries int    `toml:"max_retries"`
	// TimeoutSeconds 限制整个模型请求的时长，包括流式传输。
	TimeoutSeconds int `toml:"timeout_seconds"`

	// APIKey 可以直接写在配置文件里；加载后是最终生效的密钥。
	APIKey string `toml:"api_key"`
}

// Model 保存聊天模型及其能力。
type Model struct {
	Name              string `toml:"name"`
	ContextWindow     int    `toml:"context_window"`
	Streaming         bool   `toml:"streaming"`
	StreamUsage       bool   `toml:"stream_usage"`
	Tools             string `toml:"tools"`
	ParallelToolCalls bool   `toml:"parallel_tool_calls"`
	MaxTokensParam    string `toml:"max_tokens_param"`
	MaxOutputTokens   int    `toml:"max_output_tokens"`
	// AutoCompactTokenLimit 是自动压缩阈值；0 表示上下文窗口的 90%。
	// 配置值大于 90% 时仍取 90%。
	AutoCompactTokenLimit int `toml:"auto_compact_token_limit"`
	// ToolOutputTokens 是单个工具输出写入历史时保留的 token 数（首尾各留
	// 一半）；0 表示默认 10000。
	ToolOutputTokens int `toml:"tool_output_tokens"`
	// PostTurnCompactPercent：一轮结束时上下文达到自动压缩阈值的这个百分比
	// 就压缩；0 表示关闭。
	PostTurnCompactPercent int `toml:"post_turn_compact_percent"`
	// BackgroundCompactPercent：上下文达到自动压缩阈值的这个百分比后，在
	// 后台提前生成摘要；0 表示关闭。
	BackgroundCompactPercent int `toml:"background_compact_percent"`
}

// Agent 用于调节轮次循环。
type Agent struct {
	MaxSteps int `toml:"max_steps"`
	// ProjectDocs 为 false 时不读取 AGENTS.md / JOEY.md。
	ProjectDocs bool `toml:"project_docs"`
	// ApprovalTimeoutSeconds 是等待用户审批的时长，超时按拒绝处理。
	ApprovalTimeoutSeconds int `toml:"approval_timeout_seconds"`
}

// Server 配置 HTTP API。
type Server struct {
	Addr           string   `toml:"addr"`
	AllowedOrigins []string `toml:"allowed_origins"`
	// AuthTokenEnv 指定保存 API 令牌的环境变量名。
	AuthTokenEnv string `toml:"auth_token_env"`

	AuthToken string `toml:"-"`
}

// Workspace 配置会话的工作目录。
type Workspace struct {
	// Root 是新会话未指定目录时使用的默认工作目录。
	Root string `toml:"root"`
	// AllowedRoots 列出会话可以选择的目录树（Root 总是允许的）。
	// 支持 "~"。
	AllowedRoots []string `toml:"allowed_roots"`
}

// Sandbox 配置命令执行的隔离层（见 internal/sandbox）。命令在 macOS Seatbelt
// 中执行；danger-full-access 下不套沙箱。
type Sandbox struct {
	// Mode：read-only、workspace-write、danger-full-access。
	Mode string `toml:"mode"`
	// Network 为 false 时命令不能联网（包括本机端口和 unix socket）。
	Network bool `toml:"network"`
	// WritableRoots 是工作区之外额外可写的目录，支持 "~"。
	WritableRoots []string `toml:"writable_roots"`
	// ReadOnlySubpaths 是每个可写根下仍然只读的子路径。
	ReadOnlySubpaths []string `toml:"read_only_subpaths"`
	// DenyRead 是命令和文件工具都不能读写的路径，支持 "~"。
	DenyRead []string `toml:"deny_read"`
	// Approval：untrusted、on-request、never。
	Approval string `toml:"approval"`
	// Allow、Deny 是命令前缀规则，例如 "go test"、"git push"。
	Allow []string `toml:"allow"`
	Deny  []string `toml:"deny"`
}

// DefaultDenyRead 是默认禁止命令读取的凭据目录。Seatbelt 下命令能读整个
// 文件系统，这几处最容易泄露密钥。
var DefaultDenyRead = []string{"~/.ssh", "~/.gnupg", "~/.aws", "~/.netrc"}

// Store 配置持久化。
type Store struct {
	// Path 是 SQLite 数据库文件，支持 "~"。"" 或 ":memory:" 表示只保存在
	// 内存中，进程退出即丢失。
	Path string `toml:"path"`
}

// Default 返回没有任何文件设置取值时使用的配置。
func Default() Config {
	return Config{
		Provider: Provider{
			BaseURL:        "https://oneapi-comate.baidu-int.com/v1",
			APIKeyEnv:      "ONEAPI_API_KEY",
			MaxRetries:     3,
			TimeoutSeconds: 300,
		},
		Model: Model{
			Name: "gpt-6.1-sol",
			// 实测（2026-10-28）：90 万 token 的输入被接受，100 万被拒绝。
			ContextWindow: 900_000,
			// 接近窗口的请求一次要一分钟以上、费用也高，所以提前压缩。
			AutoCompactTokenLimit:    200_000,
			PostTurnCompactPercent:   80,
			BackgroundCompactPercent: 60,
			Streaming:                true,
			Tools:                    string(llm.ToolsNative),
			MaxTokensParam:           "max_completion_tokens",
		},
		Agent:     Agent{MaxSteps: 30, ApprovalTimeoutSeconds: 600, ProjectDocs: true},
		Server:    Server{Addr: "127.0.0.1:8080", AuthTokenEnv: "JOEY_AUTH_TOKEN"},
		Workspace: Workspace{Root: ".", AllowedRoots: []string{"~"}},
		Store:     Store{Path: "~/.joey/joey.db"},
		Sandbox: Sandbox{
			Mode:             string(sandbox.WorkspaceWrite),
			ReadOnlySubpaths: sandbox.DefaultReadOnlySubpaths,
			Approval:         string(sandbox.ApproveOnRequest),
			DenyRead:         DefaultDenyRead,
		},
	}
}

// Load 读取 path（可选；"" 表示跳过文件），然后应用环境变量
// 覆盖：ONEAPI_BASE_URL、JOEY_MODEL、JOEY_ADDR、JOEY_WORKSPACE、
// JOEY_ALLOWED_ROOTS（按系统路径列表分隔符切分）、JOEY_DB、
// JOEY_SANDBOX_MODE、JOEY_APPROVAL、JOEY_MAX_STEPS、JOEY_CONTEXT_WINDOW。
func Load(path string, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Default()
	if path != "" {
		md, err := toml.DecodeFile(path, &cfg)
		if err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
		if undec := md.Undecoded(); len(undec) > 0 {
			keys := make([]string, len(undec))
			for i, k := range undec {
				keys[i] = k.String()
			}
			return Config{}, fmt.Errorf("config: unknown keys in %s: %s", path, strings.Join(keys, ", "))
		}
		if cfg.Provider.APIKey != "" {
			// 文件里有密钥时，不让文件工具和命令读写这个文件。
			abs, err := filepath.Abs(path)
			if err != nil {
				return Config{}, fmt.Errorf("config: %w", err)
			}
			cfg.Sandbox.DenyRead = append(slices.Clone(cfg.Sandbox.DenyRead), abs)
		}
	}
	if v := getenv("ONEAPI_BASE_URL"); v != "" {
		cfg.Provider.BaseURL = v
	}
	if v := getenv("JOEY_MODEL"); v != "" {
		cfg.Model.Name = v
	}
	if v := getenv("JOEY_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := getenv("JOEY_WORKSPACE"); v != "" {
		cfg.Workspace.Root = v
	}
	if v := getenv("JOEY_ALLOWED_ROOTS"); v != "" {
		cfg.Workspace.AllowedRoots = filepath.SplitList(v)
	}
	if v := getenv("JOEY_DB"); v != "" {
		cfg.Store.Path = v
	}
	if v := getenv("JOEY_SANDBOX_MODE"); v != "" {
		cfg.Sandbox.Mode = v
	}
	if v := getenv("JOEY_APPROVAL"); v != "" {
		cfg.Sandbox.Approval = v
	}
	if v := getenv("JOEY_CONTEXT_WINDOW"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: JOEY_CONTEXT_WINDOW: %w", err)
		}
		cfg.Model.ContextWindow = n
	}
	if v := getenv("JOEY_MAX_STEPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: JOEY_MAX_STEPS: %w", err)
		}
		cfg.Agent.MaxSteps = n
	}
	if cfg.Provider.APIKeyEnv != "" {
		if v := getenv(cfg.Provider.APIKeyEnv); v != "" {
			cfg.Provider.APIKey = v
		}
	}
	if cfg.Server.AuthTokenEnv != "" {
		cfg.Server.AuthToken = getenv(cfg.Server.AuthTokenEnv)
	}
	return cfg, cfg.Validate()
}

// Validate 检查不依赖于所选 provider 的取值。
func (c Config) Validate() error {
	var errs []error
	if c.Model.Name == "" {
		errs = append(errs, errors.New("model.name is required"))
	}
	switch llm.ToolMode(c.Model.Tools) {
	case llm.ToolsNative, llm.ToolsNone:
	default:
		errs = append(errs, fmt.Errorf("model.tools must be %q or %q, got %q", llm.ToolsNative, llm.ToolsNone, c.Model.Tools))
	}
	switch c.Model.MaxTokensParam {
	case "max_completion_tokens", "max_tokens":
	default:
		errs = append(errs, fmt.Errorf("model.max_tokens_param must be max_completion_tokens or max_tokens, got %q", c.Model.MaxTokensParam))
	}
	if c.Model.ContextWindow < 0 || c.Model.MaxOutputTokens < 0 || c.Agent.MaxSteps < 0 || c.Provider.MaxRetries < 0 ||
		c.Model.AutoCompactTokenLimit < 0 || c.Model.ToolOutputTokens < 0 {
		errs = append(errs, errors.New("numeric limits must not be negative"))
	}
	for name, v := range map[string]int{
		"model.post_turn_compact_percent":  c.Model.PostTurnCompactPercent,
		"model.background_compact_percent": c.Model.BackgroundCompactPercent,
	} {
		if v < 0 || v > 100 {
			errs = append(errs, fmt.Errorf("%s must be between 0 and 100, got %d", name, v))
		}
	}
	if _, err := sandbox.ParseMode(c.Sandbox.Mode); err != nil {
		errs = append(errs, err)
	}
	if _, err := sandbox.ParseApprovalMode(c.Sandbox.Approval); err != nil {
		errs = append(errs, err)
	}
	if strings.Contains(c.Provider.BaseURL, "/chat/completions") {
		errs = append(errs, errors.New("provider.base_url must not include /chat/completions"))
	}
	return errors.Join(errs...)
}

// RequireAPI 检查调用真实网关所需的设置。
func (c Config) RequireAPI() error {
	if c.Provider.BaseURL == "" {
		return errors.New("provider.base_url is required (or set ONEAPI_BASE_URL)")
	}
	if c.Provider.APIKey == "" {
		return fmt.Errorf("no API key: set provider.api_key in joey.toml or the environment variable %s", c.Provider.APIKeyEnv)
	}
	return nil
}

// ModelInfo 把 model 配置段转换为 llm.ModelInfo。
func (c Config) ModelInfo() llm.ModelInfo {
	return llm.ModelInfo{
		Name:              c.Model.Name,
		ContextWindow:     c.Model.ContextWindow,
		Streaming:         c.Model.Streaming,
		StreamUsage:       c.Model.StreamUsage,
		ToolMode:          llm.ToolMode(c.Model.Tools),
		ParallelToolCalls: c.Model.ParallelToolCalls,
		MaxTokensParam:    c.Model.MaxTokensParam,
		MaxOutputTokens:   c.Model.MaxOutputTokens,
	}
}

// SandboxPolicy 为工作目录 root 构建沙箱策略。
func (c Config) SandboxPolicy(root string) (sandbox.Policy, error) {
	return sandbox.NewPolicy(sandbox.Mode(c.Sandbox.Mode), root, sandbox.PolicyOptions{
		WritableRoots:    expandAll(c.Sandbox.WritableRoots),
		ReadOnlySubpaths: c.Sandbox.ReadOnlySubpaths,
		DenyRead:         expandAll(c.Sandbox.DenyRead),
		Network:          c.Sandbox.Network,
	})
}

func expandAll(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = expandHome(p)
	}
	return out
}

// SandboxRules 返回审批模式和命令规则。
func (c Config) SandboxRules() (sandbox.ApprovalMode, sandbox.Rules) {
	return sandbox.ApprovalMode(c.Sandbox.Approval), sandbox.Rules{Allow: c.Sandbox.Allow, Deny: c.Sandbox.Deny}
}

func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// Package config 从 TOML 文件和环境加载 Joey 的配置。
// 机密信息只从环境变量读取。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
)

// Config 是完整的应用配置。
type Config struct {
	Provider  Provider  `toml:"provider"`
	Model     Model     `toml:"model"`
	Agent     Agent     `toml:"agent"`
	Server    Server    `toml:"server"`
	Workspace Workspace `toml:"workspace"`
	Store     Store     `toml:"store"`
}

// Provider 是模型网关。
type Provider struct {
	// BaseURL 包含版本前缀，例如 "https://host/v1"。
	BaseURL string `toml:"base_url"`
	// APIKeyEnv 指定保存 API 密钥的环境变量名。
	APIKeyEnv  string `toml:"api_key_env"`
	MaxRetries int    `toml:"max_retries"`
	// TimeoutSeconds 限制整个模型请求的时长，包括流式传输。
	TimeoutSeconds int `toml:"timeout_seconds"`

	// APIKey 在加载时从 APIKeyEnv 解析得到，绝不从文件中读取。
	APIKey string `toml:"-"`
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
}

// Agent 用于调节轮次循环。
type Agent struct {
	MaxSteps int `toml:"max_steps"`
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
			Name:           "gpt-6.1-sol",
			ContextWindow:  32000,
			Streaming:      true,
			Tools:          string(llm.ToolsNative),
			MaxTokensParam: "max_completion_tokens",
		},
		Agent:     Agent{MaxSteps: 30},
		Server:    Server{Addr: "127.0.0.1:8080", AuthTokenEnv: "JOEY_AUTH_TOKEN"},
		Workspace: Workspace{Root: ".", AllowedRoots: []string{"~"}},
		Store:     Store{Path: "~/.joey/joey.db"},
	}
}

// Load 读取 path（可选；"" 表示跳过文件），然后应用环境变量
// 覆盖：ONEAPI_BASE_URL、JOEY_MODEL、JOEY_ADDR、JOEY_WORKSPACE、
// JOEY_ALLOWED_ROOTS（按系统路径列表分隔符切分）、JOEY_DB、JOEY_MAX_STEPS。
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
	if v := getenv("JOEY_MAX_STEPS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("config: JOEY_MAX_STEPS: %w", err)
		}
		cfg.Agent.MaxSteps = n
	}
	if cfg.Provider.APIKeyEnv != "" {
		cfg.Provider.APIKey = getenv(cfg.Provider.APIKeyEnv)
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
	if c.Model.ContextWindow < 0 || c.Model.MaxOutputTokens < 0 || c.Agent.MaxSteps < 0 || c.Provider.MaxRetries < 0 {
		errs = append(errs, errors.New("numeric limits must not be negative"))
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
		return fmt.Errorf("environment variable %s is not set", c.Provider.APIKeyEnv)
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

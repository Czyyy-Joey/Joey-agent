package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Name != "gpt-6.1-sol" || cfg.Model.MaxTokensParam != "max_completion_tokens" || cfg.Server.Addr != "127.0.0.1:8080" {
		t.Errorf("defaults = %+v", cfg)
	}
	if err := cfg.RequireAPI(); err == nil || !strings.Contains(err.Error(), "ONEAPI_API_KEY") {
		t.Errorf("RequireAPI = %v", err)
	}
	if cfg.Model.ContextWindow != 900_000 || cfg.Model.AutoCompactTokenLimit != 200_000 || !cfg.Agent.ProjectDocs {
		t.Errorf("context defaults = %+v %+v", cfg.Model, cfg.Agent)
	}
}

func TestContextEnv(t *testing.T) {
	cfg, err := Load("", env(map[string]string{"JOEY_CONTEXT_WINDOW": "8000"}))
	if err != nil || cfg.Model.ContextWindow != 8000 {
		t.Errorf("JOEY_CONTEXT_WINDOW: %v %d", err, cfg.Model.ContextWindow)
	}
	if _, err := Load("", env(map[string]string{"JOEY_CONTEXT_WINDOW": "big"})); err == nil {
		t.Error("bad JOEY_CONTEXT_WINDOW accepted")
	}
}

func TestExampleFileAndEnv(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "joey.example.toml"), env(map[string]string{
		"ONEAPI_API_KEY":  "k",
		"ONEAPI_BASE_URL": "http://gw/v1",
		"JOEY_MODEL":      "other",
		"JOEY_AUTH_TOKEN": "t",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.APIKey != "k" || cfg.Provider.BaseURL != "http://gw/v1" || cfg.Model.Name != "other" || cfg.Server.AuthToken != "t" {
		t.Errorf("cfg = %+v", cfg)
	}
	mi := cfg.ModelInfo()
	if !mi.ParallelToolCalls || mi.StreamUsage || mi.ToolMode != llm.ToolsNative {
		t.Errorf("model info = %+v", mi)
	}
	if cfg.RequireAPI() != nil {
		t.Error("RequireAPI failed with key set")
	}
}

func TestRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown key":   "[model]\nnmae = \"x\"\n",
		"bad tools":     "[model]\ntools = \"maybe\"\n",
		"bad param":     "[model]\nmax_tokens_param = \"tokens\"\n",
		"full endpoint": "[provider]\nbase_url = \"http://x/v1/chat/completions\"\n",
		"negative":      "[agent]\nmax_steps = -1\n",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_")+".toml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p, env(nil)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestWorkspaceAndStoreEnv(t *testing.T) {
	cfg, err := Load("", env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != "~/.joey/joey.db" || len(cfg.Workspace.AllowedRoots) != 1 || cfg.Workspace.AllowedRoots[0] != "~" {
		t.Errorf("defaults = %+v %+v", cfg.Store, cfg.Workspace)
	}
	list := strings.Join([]string{"/a", "/b"}, string(os.PathListSeparator))
	cfg, err = Load("", env(map[string]string{"JOEY_DB": ":memory:", "JOEY_ALLOWED_ROOTS": list, "JOEY_WORKSPACE": "/a"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.Path != ":memory:" || len(cfg.Workspace.AllowedRoots) != 2 || cfg.Workspace.AllowedRoots[1] != "/b" || cfg.Workspace.Root != "/a" {
		t.Errorf("env overrides = %+v %+v", cfg.Store, cfg.Workspace)
	}
}

func TestSandboxConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "joey.example.toml"), env(map[string]string{"JOEY_SANDBOX_MODE": "read-only"}))
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	p, err := cfg.SandboxPolicy(ws)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if p.Mode != "read-only" || p.Network || len(p.DenyRead) < 4 || p.DenyRead[0] != filepath.Join(home, ".ssh") {
		t.Errorf("policy = %+v", p)
	}
	// docker 后端已移除，旧配置要报错而不是被悄悄忽略。
	old := filepath.Join(t.TempDir(), "old.toml")
	if err := os.WriteFile(old, []byte("[sandbox]\nbackend = \"docker\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(old, env(nil)); err == nil || !strings.Contains(err.Error(), "sandbox.backend") {
		t.Errorf("old backend key = %v", err)
	}
	if _, err := Load("", env(map[string]string{"JOEY_SANDBOX_MODE": "rw"})); err == nil {
		t.Error("bad mode accepted")
	}
}

func TestAPIKeyFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "joey.toml")
	if err := os.WriteFile(path, []byte("[provider]\napi_key = \"file-key\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.APIKey != "file-key" || cfg.RequireAPI() != nil {
		t.Errorf("api key = %q, RequireAPI = %v", cfg.Provider.APIKey, cfg.RequireAPI())
	}
	if !slices.Contains(cfg.Sandbox.DenyRead, path) || slices.Contains(DefaultDenyRead, path) {
		t.Errorf("deny_read = %v, want it to include %s", cfg.Sandbox.DenyRead, path)
	}
	p, err := cfg.SandboxPolicy(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.NewGuard(p).Resolve("joey.toml", sandbox.Read); err == nil {
		t.Error("file tools can read the config file that holds the key")
	}

	cfg, err = Load(path, env(map[string]string{"ONEAPI_API_KEY": "env-key"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider.APIKey != "env-key" {
		t.Errorf("env should take precedence, got %q", cfg.Provider.APIKey)
	}
}

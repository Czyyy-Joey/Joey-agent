package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
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
		"unknown key":    "[model]\nnmae = \"x\"\n",
		"secret in file": "[provider]\napi_key = \"sk-123\"\n",
		"bad tools":      "[model]\ntools = \"maybe\"\n",
		"bad param":      "[model]\nmax_tokens_param = \"tokens\"\n",
		"full endpoint":  "[provider]\nbase_url = \"http://x/v1/chat/completions\"\n",
		"negative":       "[agent]\nmax_steps = -1\n",
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

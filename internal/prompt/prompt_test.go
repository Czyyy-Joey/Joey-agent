package prompt

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// project 创建 base/repo/{.git, AGENTS.md, svc/JOEY.md, svc/api/AGENTS.md+JOEY.md}，
// 以及 repo 之上的 base/AGENTS.md（不应被读到）。
func project(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, base+"/AGENTS.md", "outside the project")
	write(t, base+"/repo/.git/HEAD", "ref: refs/heads/main\n")
	write(t, base+"/repo/AGENTS.md", "Use Go 1.25.\n")
	write(t, base+"/repo/svc/JOEY.md", "Run make test before finishing.\n")
	write(t, base+"/repo/svc/api/AGENTS.md", "API handlers live here.\n")
	write(t, base+"/repo/svc/api/JOEY.md", "shadowed by AGENTS.md")
	return base
}

func TestLoadDocs(t *testing.T) {
	base := project(t)
	docs := LoadDocs(base+"/repo/svc/api", "")
	var got []string
	for _, d := range docs {
		got = append(got, strings.TrimPrefix(d.Path, base))
	}
	if strings.Join(got, ",") != "/repo/AGENTS.md,/repo/svc/JOEY.md,/repo/svc/api/AGENTS.md" {
		t.Errorf("docs = %v", got)
	}

	// 没有 .git：只看工作目录本身。
	write(t, base+"/plain/sub/AGENTS.md", "sub")
	write(t, base+"/plain/AGENTS.md", "parent")
	docs = LoadDocs(base+"/plain/sub", "")
	if len(docs) != 1 || docs[0].Content != "sub" {
		t.Errorf("no repo = %+v", docs)
	}
	// stop 之上不找 .git。
	if docs := LoadDocs(base+"/repo/svc", base+"/repo/svc"); len(docs) != 1 || !strings.HasSuffix(docs[0].Path, "svc/JOEY.md") {
		t.Errorf("stop = %+v", docs)
	}
}

func TestLoadDocsLimit(t *testing.T) {
	base := project(t)
	write(t, base+"/repo/AGENTS.md", strings.Repeat("长", MaxDocBytes)) // 3 倍于上限
	docs := LoadDocs(base+"/repo/svc", "")
	if len(docs) != 1 || !docs[0].Truncated || len(docs[0].Content) > MaxDocBytes || !strings.HasPrefix(docs[0].Content, "长") {
		t.Fatalf("docs = %d truncated=%v", len(docs), len(docs) > 0 && docs[0].Truncated)
	}
	if strings.ContainsRune(docs[0].Content, '�') {
		t.Error("cut inside a character")
	}
}

func TestBuildGolden(t *testing.T) {
	base := project(t)
	docs := LoadDocs(base+"/repo/svc/api", "")
	for i := range docs {
		docs[i].Path = strings.Replace(docs[i].Path, base, "/home/u", 1)
	}
	date := time.Date(2026, 10, 28, 9, 0, 0, 0, time.FixedZone("Asia/Shanghai", 8*3600))
	got := Build("You are Joey.\n", Env{
		Workspace: "/home/u/repo/svc/api", Shell: "/bin/sh", SandboxMode: "workspace-write",
		Approval: "on-request", Date: date,
	}, docs)
	golden := filepath.Join("testdata", "build.golden")
	if *update {
		write(t, golden, got)
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("Build() mismatch; run go test ./internal/prompt -update\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestEnvironmentMinimal(t *testing.T) {
	got := Environment(Env{Workspace: "/w"})
	if got != "<environment_context>\n  <cwd>/w</cwd>\n</environment_context>" {
		t.Errorf("minimal = %q", got)
	}
	if !strings.Contains(Environment(Env{Workspace: "/w", SandboxMode: "read-only", Network: true}), "<network_access>enabled</network_access>") {
		t.Error("network")
	}
}

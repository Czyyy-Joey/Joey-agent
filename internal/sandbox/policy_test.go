package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fixture 创建 base/{ws/{src,.git}, extra, outside/secret.txt} 以及几个
// 指向外部的符号链接，返回解析后的 base。
func fixture(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"ws/src", "ws/.git", "extra", "outside", "secrets"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(base, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("ws/README.md", "# ws")
	write("ws/.git/config", "[core]")
	write("outside/secret.txt", "TOP SECRET")
	write("secrets/id_rsa", "PRIVATE KEY")
	link := func(target, name string) {
		if err := os.Symlink(target, filepath.Join(base, name)); err != nil {
			t.Fatal(err)
		}
	}
	link(filepath.Join(base, "outside"), "ws/out-dir")
	link(filepath.Join(base, "outside", "secret.txt"), "ws/out-file")
	link(filepath.Join(base, "outside", "new.txt"), "ws/dangling")
	link(".git", "ws/git-alias")
	link(filepath.Join(base, "secrets"), "ws/secrets-link")
	return base
}

// mustPolicy 返回 base/ws 上的策略；base/secrets 始终在 deny_read 中。
func mustPolicy(t *testing.T, mode Mode, base string, extra ...string) Policy {
	t.Helper()
	p, err := NewPolicy(mode, filepath.Join(base, "ws"), PolicyOptions{
		WritableRoots: extra,
		DenyRead:      []string{filepath.Join(base, "secrets")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGuard(t *testing.T) {
	base := fixture(t)
	ws := filepath.Join(base, "ws")
	extra := filepath.Join(base, "extra")
	write := NewGuard(mustPolicy(t, WorkspaceWrite, base, extra))
	ro := NewGuard(mustPolicy(t, ReadOnly, base))
	full := NewGuard(mustPolicy(t, FullAccess, base))

	type tc struct {
		name   string
		g      *Guard
		path   string
		access Access
		want   string // 期望解析结果；为空表示期望失败
		reason Reason
	}
	tests := []tc{
		{"read file", write, "README.md", Read, ws + "/README.md", 0},
		{"read workspace root", write, "", Read, ws, 0},
		{"read .git is allowed", write, ".git/config", Read, ws + "/.git/config", 0},
		{"read missing", write, "nope.md", Read, "", ReasonNotExist},
		{"read dotdot", write, "../outside/secret.txt", Read, "", ReasonOutside},
		{"read absolute outside", write, base + "/outside/secret.txt", Read, "", ReasonOutside},
		{"read via dir symlink", write, "out-dir/secret.txt", Read, "", ReasonEscapes},
		{"read via file symlink", write, "out-file", Read, "", ReasonEscapes},
		{"read extra root", write, extra, Read, extra, 0},
		{"read NUL", write, "a\x00b", Read, "", ReasonInvalid},
		{"read deny_read", write, base + "/secrets/id_rsa", Read, "", ReasonDenied},
		{"read deny_read via symlink", write, "secrets-link/id_rsa", Read, "", ReasonDenied},

		{"write new file", write, "src/new.go", Write, ws + "/src/new.go", 0},
		{"write new nested dirs", write, "a/b/c.txt", Write, ws + "/a/b/c.txt", 0},
		{"write existing", write, "README.md", Write, ws + "/README.md", 0},
		{"write extra root", write, extra + "/x", Write, extra + "/x", 0},
		{"write .git", write, ".git/config", Write, "", ReasonOutside},
		{"write new file in .git", write, ".git/hooks/pre-commit", Write, "", ReasonOutside},
		{"write .git via alias symlink", write, "git-alias/config", Write, "", ReasonReadOnlySubpath},
		{"write dotdot", write, "../outside/x", Write, "", ReasonOutside},
		{"write via dir symlink", write, "out-dir/x", Write, "", ReasonEscapes},
		{"write dangling symlink", write, "dangling", Write, "", ReasonEscapes},
		{"write in read-only mode", ro, "README.md", Write, "", ReasonReadOnlyPolicy},
		{"read in read-only mode", ro, "README.md", Read, ws + "/README.md", 0},

		{"full access reads outside", full, base + "/outside/secret.txt", Read, base + "/outside/secret.txt", 0},
		{"full access writes .git", full, ".git/x", Write, ws + "/.git/x", 0},
		{"full access ignores deny_read", full, base + "/secrets/id_rsa", Read, base + "/secrets/id_rsa", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.g.Resolve(tt.path, tt.access)
			if tt.want != "" {
				if err != nil || got != tt.want {
					t.Fatalf("Resolve = %q, %v; want %q", got, err, tt.want)
				}
				return
			}
			var pe *PathError
			if !errors.As(err, &pe) || pe.Reason != tt.reason {
				t.Fatalf("Resolve = %q, %v; want reason %d", got, err, tt.reason)
			}
		})
	}
}

func TestNewPolicy(t *testing.T) {
	base := fixture(t)
	ws := base + "/ws"
	if _, err := NewPolicy("sometimes", ws, PolicyOptions{}); err == nil {
		t.Error("bad mode accepted")
	}
	if _, err := NewPolicy(ReadOnly, base+"/missing", PolicyOptions{}); err == nil {
		t.Error("missing workspace accepted")
	}
	if _, err := NewPolicy(ReadOnly, ws, PolicyOptions{ReadOnlySubpaths: []string{"../x"}}); err == nil {
		t.Error("escaping read-only subpath accepted")
	}
	if _, err := NewPolicy(ReadOnly, ws, PolicyOptions{DenyRead: []string{"relative"}}); err == nil {
		t.Error("relative deny_read accepted")
	}
	if _, err := NewPolicy(ReadOnly, ws, PolicyOptions{DenyRead: []string{base}}); err == nil {
		t.Error("deny_read containing the workspace accepted")
	}
	p, err := NewPolicy(WorkspaceWrite, ws, PolicyOptions{
		WritableRoots: []string{base + "/extra", base + "/extra", ws},
		DenyRead:      []string{ws + "/secrets-link", base + "/not-yet"},
		Network:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ExtraWritable) != 1 || !p.Network {
		t.Errorf("policy = %+v", p)
	}
	// 符号链接同时保留别名和目标；不存在的路径原样保留。
	if want := []string{ws + "/secrets-link", base + "/secrets", base + "/not-yet"}; !slices.Equal(p.DenyRead, want) {
		t.Errorf("deny_read = %v, want %v", p.DenyRead, want)
	}
	roots := p.WritableRoots()
	if len(roots) != 2 || !slices.Equal(roots[0].ReadOnly, []string{ws + "/.git", ws + "/.joey"}) {
		t.Errorf("writable roots = %+v", roots)
	}
	if exec := p.ExecWritableRoots(); len(exec) <= len(roots) {
		t.Errorf("exec roots should add temp dirs: %+v", exec)
	}
	ro, _ := NewPolicy(ReadOnly, ws, PolicyOptions{})
	if len(ro.WritableRoots()) != 0 || len(ro.ExecWritableRoots()) != 0 {
		t.Error("read-only policy has writable roots")
	}
}

// 工作区位于另一个可写根（$TMPDIR）之下时，那个根也要排除工作区的 .git。
func TestExecWritableRootsProtectNestedWorkspace(t *testing.T) {
	base := fixture(t)
	t.Setenv("TMPDIR", base)
	p := mustPolicy(t, WorkspaceWrite, base)
	var tmp *WritableRoot
	for _, w := range p.ExecWritableRoots() {
		if w.Root == base {
			tmp = &w
		}
	}
	if tmp == nil || !slices.Contains(tmp.ReadOnly, base+"/ws/.git") {
		t.Errorf("temp root = %+v", tmp)
	}
}

func TestHeadTail(t *testing.T) {
	b := newHeadTail(10)
	b.Write([]byte("01234"))
	b.Write([]byte("56789"))
	if s, trunc := b.String(); s != "0123456789" || trunc {
		t.Fatalf("exact fit = %q %v", s, trunc)
	}
	b.Write([]byte("abc"))
	s, trunc := b.String()
	if !trunc || s != "01234\n[... 3 bytes omitted ...]\n89abc" {
		t.Errorf("overflow = %q", s)
	}
}

package e2e

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// sandboxTmp 是 TestSandboxCLI 给 joey 子进程用的 $TMPDIR。
var sandboxTmp = os.TempDir()

// sandboxExec 运行 joey sandbox exec 并返回合并输出和退出码。
func sandboxExec(t *testing.T, bin, ws string, args ...string) (string, int) {
	t.Helper()
	return runJoey(t, bin, append([]string{"sandbox", "exec", "-workspace", ws}, args...)...)
}

// runJoey 运行 joey 并返回合并输出和退出码。
func runJoey(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	// t.TempDir 位于 $TMPDIR 之下，而 $TMPDIR 对命令可写；换成测试自己的
	// 目录，"写到工作区外"才真的越权。
	cmd.Env = testEnv("ONEAPI_API_KEY=sk-e2e-should-not-leak", "TMPDIR="+sandboxTmp)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(out), 0
}

func TestSandboxCLI(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the sandbox is macOS only")
	}
	bin := buildJoey(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, outside := filepath.Join(base, "ws"), filepath.Join(base, "outside")
	sandboxTmp = filepath.Join(base, "tmp")
	t.Cleanup(func() { sandboxTmp = os.TempDir() })
	for _, d := range []string{ws, outside, sandboxTmp} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		code int
		want []string
	}{
		{"read-only command runs", []string{"--", "ls"}, 0, []string{"decision: skip", "seatbelt=true", "hello.txt", "── exit 0"}},
		{"exit code is passed through", []string{"--", "exit 7"}, 7, []string{"── exit 7"}},
		{"timeout kills", []string{"-timeout", "300ms", "--", "sleep 30"}, 124, []string{"timed out"}},
		{"write outside is denied", []string{"--", "echo x > " + outside + "/x"}, 1, []string{"Operation not permitted", "likely denied by the sandbox"}},
		{"deny_read", []string{"-deny-read", outside, "--", "cat " + outside + "/secret.txt"}, 1, []string{"Operation not permitted"}},
		{"needs approval without a terminal", []string{"-approval", "untrusted", "--", "touch x"}, 126, []string{"needs_approval", "not approved"}},
		{"forbidden", []string{"-approval", "never", "--", "sudo ls"}, 126, []string{"forbidden"}},
		{"secrets are scrubbed", []string{"--", "env"}, 0, []string{"JOEY_SANDBOX=seatbelt", "JOEY_SANDBOX_NETWORK_DISABLED=1"}},
		{"full access runs without a sandbox", []string{"-mode", "danger-full-access", "--", "echo x > " + outside + "/full"}, 0, []string{"seatbelt=false"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, code := sandboxExec(t, bin, ws, c.args...)
			if code != c.code {
				t.Errorf("exit = %d, want %d\n%s", code, c.code, out)
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("output missing %q:\n%s", w, out)
				}
			}
			if strings.Contains(out, "sk-e2e-should-not-leak") {
				t.Errorf("API key leaked:\n%s", out)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(ws, "x")); err == nil {
		t.Error("unapproved command ran")
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Error("write outside the workspace succeeded")
	}
	if _, err := os.Stat(filepath.Join(outside, "full")); err != nil {
		t.Error("danger-full-access write did not happen")
	}

	out, code := runJoey(t, bin, "sandbox", "info", "-workspace", ws, "-profile")
	for _, w := range []string{"mode:      workspace-write", "(deny default)", "-DWRITABLE_ROOT_0=" + ws, "/.ssh"} {
		if code != 0 || !strings.Contains(out, w) {
			t.Errorf("info missing %q (exit %d):\n%s", w, code, out)
		}
	}
}

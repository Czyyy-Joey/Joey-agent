//go:build unix

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// newSandbox 返回 p 的沙箱；本机没有 Seatbelt 时跳过测试。
func newSandbox(t *testing.T, p Policy) *Sandbox {
	t.Helper()
	if p.Mode != FullAccess && !Available() {
		t.Skip("seatbelt is macOS only")
	}
	sb, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sb.Close() })
	return sb
}

// sandboxes 返回 workspace-write（Seatbelt）和 danger-full-access（不套沙箱）
// 两个沙箱，进程控制相关的行为两者必须一致。
func sandboxes(t *testing.T, base string) map[string]*Sandbox {
	t.Helper()
	return map[string]*Sandbox{
		"seatbelt": newSandbox(t, mustPolicy(t, WorkspaceWrite, base)),
		"none":     newSandbox(t, mustPolicy(t, FullAccess, base)),
	}
}

func run(t *testing.T, sb *Sandbox, cmd string, mod ...func(*ExecRequest)) ExecResult {
	t.Helper()
	req := ExecRequest{Command: cmd, Timeout: 10 * time.Second}
	for _, m := range mod {
		m(&req)
	}
	res, err := sb.Exec(context.Background(), req, nil)
	if err != nil {
		t.Fatalf("Exec(%q): %v", cmd, err)
	}
	return res
}

func TestExecBasics(t *testing.T) {
	base := fixture(t)
	for name, sb := range sandboxes(t, base) {
		t.Run(name, func(t *testing.T) {
			ws := sb.Policy().Workspace
			res := run(t, sb, "pwd; echo err >&2; exit 3")
			if res.ExitCode != 3 || strings.TrimSpace(res.Stdout) != ws || strings.TrimSpace(res.Stderr) != "err" {
				t.Errorf("result = %+v", res)
			}
			if res := run(t, sb, "pwd", func(r *ExecRequest) { r.Cwd = "src" }); strings.TrimSpace(res.Stdout) != ws+"/src" {
				t.Errorf("cwd = %q", res.Stdout)
			}
			// danger-full-access 下 Guard 不限制路径，cwd 也可以在工作区外。
			if _, err := sb.Exec(context.Background(), ExecRequest{Command: "pwd", Cwd: "../outside"}, nil); (err == nil) != (name == "none") {
				t.Errorf("cwd outside the workspace: err = %v", err)
			}
			if _, err := sb.Exec(context.Background(), ExecRequest{Command: "  "}, nil); err == nil {
				t.Error("empty command accepted")
			}
			// 交互式命令读 stdin 时得到 EOF，而不是挂起。
			if res := run(t, sb, "read x; echo got=$?"); !strings.Contains(res.Stdout, "got=1") {
				t.Errorf("stdin = %q", res.Stdout)
			}
			if res := run(t, sb, "kill -9 $$"); res.ExitCode != 137 {
				t.Errorf("signal exit = %d", res.ExitCode)
			}
		})
	}
}

func TestExecEnvHasNoSecrets(t *testing.T) {
	t.Setenv("ONEAPI_API_KEY", "sk-test-should-not-leak")
	t.Setenv("JOEY_AUTH_TOKEN", "tok-should-not-leak")
	t.Setenv("SOME_RANDOM_VAR", "also-not-passed")
	base := fixture(t)
	for name, sb := range sandboxes(t, base) {
		t.Run(name, func(t *testing.T) {
			res := run(t, sb, "env", func(r *ExecRequest) {
				r.Env = map[string]string{"GOFLAGS": "-count=1", "GITHUB_TOKEN": "ghp-should-not-pass"}
			})
			for _, leak := range []string{"sk-test", "tok-should", "also-not", "ghp-should"} {
				if strings.Contains(res.Stdout, leak) {
					t.Errorf("env leaked %q:\n%s", leak, res.Stdout)
				}
			}
			for _, want := range []string{"PATH=", "GOFLAGS=-count=1", "JOEY_SANDBOX=" + name + "\n", "GIT_PAGER=cat"} {
				if !strings.Contains(res.Stdout, want) {
					t.Errorf("env missing %q", want)
				}
			}
			if got := strings.Contains(res.Stdout, "JOEY_SANDBOX_NETWORK_DISABLED=1"); got != (name == "seatbelt") {
				t.Errorf("JOEY_SANDBOX_NETWORK_DISABLED present = %v", got)
			}
		})
	}
}

func TestExecTimeoutKillsProcessTree(t *testing.T) {
	base := fixture(t)
	for name, sb := range sandboxes(t, base) {
		t.Run(name, func(t *testing.T) {
			marker := fmt.Sprintf("joey-tree-%d-%s", time.Now().UnixNano(), name)
			start := time.Now()
			// 两个子进程：一个后台、一个前台；都带上独特的参数以便查找。
			cmd := fmt.Sprintf("sh -c 'sleep 100' %s & sh -c 'sleep 101' %s", marker, marker)
			res := run(t, sb, cmd, func(r *ExecRequest) { r.Timeout = 300 * time.Millisecond })
			if !res.TimedOut || time.Since(start) > 3*time.Second {
				t.Fatalf("result = %+v after %s", res, time.Since(start))
			}
			waitNoProcess(t, marker)
		})
	}
}

func TestExecBackgroundChildCleanedUp(t *testing.T) {
	base := fixture(t)
	for name, sb := range sandboxes(t, base) {
		t.Run(name, func(t *testing.T) {
			marker := fmt.Sprintf("joey-bg-%d-%s", time.Now().UnixNano(), name)
			start := time.Now()
			res := run(t, sb, fmt.Sprintf("sh -c 'sleep 100' %s & echo started", marker))
			if res.ExitCode != 0 || !strings.Contains(res.Stdout, "started") || time.Since(start) > 3*time.Second {
				t.Fatalf("result = %+v after %s", res, time.Since(start))
			}
			waitNoProcess(t, marker)
		})
	}
}

func TestExecCancel(t *testing.T) {
	base := fixture(t)
	for name, sb := range sandboxes(t, base) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var once sync.Once
			started := make(chan struct{})
			go func() {
				<-started
				cancel()
			}()
			begin := time.Now()
			res, err := sb.Exec(ctx, ExecRequest{Command: "echo ready; trap '' TERM; sleep 30"}, func(Stream, []byte) {
				once.Do(func() { close(started) })
			})
			if err != nil {
				t.Fatal(err)
			}
			// 命令忽略了 SIGTERM，要靠 killGrace 之后的 SIGKILL 结束。
			if !res.Cancelled || time.Since(begin) > time.Second+killGrace {
				t.Errorf("result = %+v after %s", res, time.Since(begin))
			}
		})
	}
}

func TestExecOutputTruncated(t *testing.T) {
	base := fixture(t)
	for name, sb := range sandboxes(t, base) {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			streamed := 0
			res, err := sb.Exec(context.Background(), ExecRequest{
				Command:   "echo FIRST; i=0; while [ $i -lt 5000 ]; do echo line-$i; i=$((i+1)); done; echo LAST",
				MaxOutput: 1000,
			}, func(_ Stream, b []byte) {
				mu.Lock()
				streamed += len(b)
				mu.Unlock()
			})
			if err != nil {
				t.Fatal(err)
			}
			if !res.Truncated || len(res.Stdout) > 1100 || !strings.HasPrefix(res.Stdout, "FIRST") || !strings.HasSuffix(res.Stdout, "LAST\n") {
				t.Errorf("truncated=%v len=%d head=%q tail=%q", res.Truncated, len(res.Stdout), res.Stdout[:20], res.Stdout[len(res.Stdout)-20:])
			}
			if streamed < 40000 {
				t.Errorf("onOutput saw %d bytes, want the full output", streamed)
			}
		})
	}
}

// isolatedTmp 把 $TMPDIR 换成 base/tmp。t.TempDir 位于 $TMPDIR 之下，而
// $TMPDIR 对命令是可写的；换掉之后"写到工作区外"才真的在可写范围之外。
func isolatedTmp(t *testing.T, base string) string {
	t.Helper()
	tmp := filepath.Join(base, "tmp")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	return tmp
}

// TestSeatbeltEnforcement 验证内核真的拦截越权的读、写和网络，并对比
// danger-full-access（不套沙箱）下同样的命令都能成功。
func TestSeatbeltEnforcement(t *testing.T) {
	base := fixture(t)
	ws, outside, secrets := base+"/ws", base+"/outside", base+"/secrets"
	tmp := isolatedTmp(t, base)
	belt := newSandbox(t, mustPolicy(t, WorkspaceWrite, base))
	none := newSandbox(t, mustPolicy(t, FullAccess, base))

	cases := []struct {
		name, cmd string
		beltOK    bool
	}{
		{"write workspace", "echo x > ok.txt", true},
		{"write subdir", "mkdir -p a/b && echo x > a/b/c", true},
		{"write TMPDIR", "echo x > $TMPDIR/t && rm $TMPDIR/t", true},
		{"rename inside workspace", "mkdir d1 && mv d1 d2 && rmdir d2", true},
		{"read outside", "cat " + outside + "/secret.txt", true},
		{"list root", "ls / > /dev/null", true},
		{"git works", "git rev-parse --git-dir >/dev/null 2>&1 || git --version", true},
		{"write to /dev/stderr", "echo hi > /dev/stderr", true},
		{"write .git", "echo x > .git/evil", false},
		{"create .joey", "mkdir .joey", false},
		{"remove .git", "rm -r .git", false},
		{"rename .git", "mv .git g2", false},
		{"write .git via symlink", "echo x > git-alias/evil2", false},
		{"rename workspace", "cd .. && mv ws ws2", false},
		{"write outside", "echo x > " + outside + "/evil.txt", false},
		{"write via symlink", "echo x > out-dir/evil3.txt", false},
		{"write /etc", "echo x > /etc/joey-evil", false},
		{"read deny_read", "cat " + secrets + "/id_rsa", false},
		{"list deny_read", "ls " + secrets, false},
		{"read deny_read via symlink", "cat secrets-link/id_rsa", false},
		{"hard link into deny_read", "ln " + secrets + "/id_rsa hl", false},
		{"unix socket", "/usr/bin/nc -U /var/run/syslog </dev/null", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := run(t, belt, c.cmd)
			if got := res.ExitCode == 0; got != c.beltOK {
				t.Errorf("seatbelt ok=%v, want %v: %+v", got, c.beltOK, res)
			}
			if !c.beltOK && c.name != "unix socket" && !res.LikelyDenied() {
				t.Errorf("denial not recognised: %+v", res)
			}
		})
	}
	// 同样的越权命令在不套沙箱时都能成功，说明上面的失败来自 Seatbelt。
	for _, cmd := range []string{
		"echo x > .git/evil", "echo x > " + outside + "/evil.txt", "cat " + secrets + "/id_rsa",
	} {
		if res := run(t, none, cmd); res.ExitCode != 0 {
			t.Errorf("full access %q: %+v", cmd, res)
		}
	}
	for _, f := range []string{ws + "/.git/g2", ws + "/.joey", base + "/ws2", "/etc/joey-evil", ws + "/hl", ws + "/out-dir/evil3.txt"} {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("%s was created inside the sandbox", f)
		}
	}
	if _, err := os.Stat(tmp + "/t"); err == nil {
		t.Error("temp file not removed")
	}
}

// TestSeatbeltProtectsAncestors：只读路径、deny_read 路径不在可写根的第一层时，
// 改名它们的上级目录也必须被拒绝，否则 mv conf conf2 之后 conf2/locked 不再受保护。
func TestSeatbeltProtectsAncestors(t *testing.T) {
	base := fixture(t)
	isolatedTmp(t, base)
	if err := os.MkdirAll(base+"/ws/conf/locked", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base+"/ws/keys", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+"/ws/keys/.env", []byte("TOKEN=x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := NewPolicy(WorkspaceWrite, base+"/ws", PolicyOptions{
		ReadOnlySubpaths: []string{"conf/locked"},
		DenyRead:         []string{base + "/ws/keys/.env"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sb := newSandbox(t, p)
	for cmd, ok := range map[string]bool{
		"echo x > conf/ok":       true,
		"echo x > conf/locked/x": false,
		"mv conf conf2":          false,
		"mv conf/locked conf/l2": false,
		"cat keys/.env":          false,
		"mv keys keys2":          false,
		"echo x > keys/other":    true,
	} {
		if got := run(t, sb, cmd).ExitCode == 0; got != ok {
			t.Errorf("%q ok=%v, want %v", cmd, got, ok)
		}
	}
}

// TestSeatbeltReadOnlySubpathUnderTempDir：工作区位于 $TMPDIR 下时，.git 仍然只读。
func TestSeatbeltReadOnlySubpathUnderTempDir(t *testing.T) {
	base := fixture(t)
	t.Setenv("TMPDIR", base)
	sb := newSandbox(t, mustPolicy(t, WorkspaceWrite, base))
	if run(t, sb, "echo x > .git/evil").ExitCode == 0 {
		t.Error(".git became writable through the temp dir root")
	}
	if run(t, sb, "echo x > ../outside/tmp-ok").ExitCode != 0 {
		t.Error("temp dir should be writable")
	}
}

func TestSeatbeltReadOnlyMode(t *testing.T) {
	base := fixture(t)
	sb := newSandbox(t, mustPolicy(t, ReadOnly, base))
	if res := run(t, sb, "cat README.md"); res.ExitCode != 0 || res.Stdout != "# ws" {
		t.Errorf("read = %+v", res)
	}
	for _, cmd := range []string{"echo x > ro.txt", "echo x > $TMPDIR/ro.txt", "echo x > /tmp/joey-ro.txt"} {
		if run(t, sb, cmd).ExitCode == 0 {
			t.Errorf("read-only sandbox allowed %q", cmd)
		}
	}
}

// TestSeatbeltNetwork：network=false 时连本机 TCP 都建立不了；用本机监听端口，
// 测试不依赖外网。
func TestSeatbeltNetwork(t *testing.T) {
	base := fixture(t)
	port := listenLocal(t)
	cmd := fmt.Sprintf("/usr/bin/nc -z -w 2 127.0.0.1 %d", port)
	off := mustPolicy(t, WorkspaceWrite, base)
	if run(t, newSandbox(t, off), cmd).ExitCode == 0 {
		t.Error("seatbelt allowed a TCP connection with network=false")
	}
	on := off
	on.Network = true
	if res := run(t, newSandbox(t, on), cmd); res.ExitCode != 0 {
		t.Errorf("seatbelt blocked TCP with network=true: %+v", res)
	}
}

func TestProfileUsesParams(t *testing.T) {
	base := fixture(t)
	weird := filepath.Join(base, `ws") (allow file-write* (subpath "/`)
	if err := os.MkdirAll(weird, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := NewPolicy(WorkspaceWrite, weird, PolicyOptions{DenyRead: []string{base + "/secrets"}})
	if err != nil {
		t.Fatal(err)
	}
	profile, params := Profile(p)
	if strings.Contains(profile, base) {
		t.Errorf("paths must be passed as parameters, not embedded:\n%s", profile)
	}
	joined := strings.Join(params, " ")
	for _, want := range []string{"-DWRITABLE_ROOT_0=" + weird, "-DWRITABLE_ROOT_0_RO_0=" + weird + "/.git", "-DDENY_READ_0=" + base + "/secrets"} {
		if !strings.Contains(joined, want) {
			t.Errorf("params missing %q: %v", want, params)
		}
	}
	if prof, _ := Profile(mustPolicy(t, FullAccess, base)); prof != "" {
		t.Error("full access should not use a profile")
	}
	isolatedTmp(t, base)
	sb := newSandbox(t, p)
	if run(t, sb, "echo x > "+base+"/outside/q").ExitCode == 0 {
		t.Error("injection through the workspace path widened the profile")
	}
}

func TestNewWithoutSeatbelt(t *testing.T) {
	if Available() {
		t.Skip("seatbelt is available")
	}
	base := fixture(t)
	if _, err := New(mustPolicy(t, WorkspaceWrite, base)); err != ErrUnsupported {
		t.Errorf("New = %v, want ErrUnsupported", err)
	}
	if _, err := New(mustPolicy(t, FullAccess, base)); err != nil {
		t.Errorf("full access = %v", err)
	}
}

func TestLikelyDenied(t *testing.T) {
	for _, tt := range []struct {
		res  ExecResult
		want bool
	}{
		{ExecResult{Sandboxed: true, ExitCode: 1, Stderr: "sh: x: Operation not permitted"}, true},
		{ExecResult{Sandboxed: true, ExitCode: 1, Stdout: "Permission denied"}, true},
		{ExecResult{Sandboxed: false, ExitCode: 1, Stderr: "Operation not permitted"}, false},
		{ExecResult{Sandboxed: true, ExitCode: 0, Stderr: "Operation not permitted"}, false},
		{ExecResult{Sandboxed: true, ExitCode: 127, Stderr: "sandbox: command not found"}, false},
		{ExecResult{Sandboxed: true, ExitCode: 1, Stderr: "FAIL: TestFoo"}, false},
	} {
		if got := tt.res.LikelyDenied(); got != tt.want {
			t.Errorf("LikelyDenied(%+v) = %v", tt.res, got)
		}
	}
}

// listenLocal 在 127.0.0.1 上监听一个随机端口并返回端口号。
func listenLocal(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitNoProcess 等待命令行中带 marker 的进程全部消失。
func waitNoProcess(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, _ := exec.Command("pgrep", "-f", marker).Output()
		if len(strings.TrimSpace(string(out))) == 0 {
			return
		}
		if time.Now().After(deadline) {
			pids := strings.Fields(string(out))
			for _, p := range pids {
				var pid int
				fmt.Sscan(p, &pid)
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			t.Fatalf("processes still alive: %v", pids)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestUnsandboxedRequest：批准后的 Unsandboxed 请求不经过 Seatbelt，其余限制照旧。
func TestUnsandboxedRequest(t *testing.T) {
	base := fixture(t)
	isolatedTmp(t, base)
	sb := newSandbox(t, mustPolicy(t, WorkspaceWrite, base))
	cmd := "echo x > " + base + "/outside/escalated.txt && env"
	if res := run(t, sb, cmd); res.ExitCode == 0 || !res.Sandboxed {
		t.Errorf("sandboxed run = %+v", res)
	}
	res := run(t, sb, cmd, func(r *ExecRequest) { r.Unsandboxed = true })
	if res.ExitCode != 0 || res.Sandboxed || !strings.Contains(res.Stdout, "JOEY_SANDBOX=none") {
		t.Errorf("unsandboxed run = %+v", res)
	}
	if _, err := sb.Exec(context.Background(), ExecRequest{Command: "pwd", Cwd: "../outside", Unsandboxed: true}, nil); err == nil {
		t.Error("unsandboxed request may still not leave the workspace for its cwd")
	}
}

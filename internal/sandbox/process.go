//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// runProcess 在新的进程组里执行 argv。超时或取消时先向整个进程组发
// SIGTERM，killGrace 后发 SIGKILL；主进程正常退出后也会杀掉组内残留的
// 后台进程，避免它们占着输出管道或在用户不知情时继续运行。
// 这对应 Codex exec.rs 中的 kill_child_process_group。
func runProcess(ctx context.Context, argv []string, cwd string, env []string, req ExecRequest, onOutput OutputFunc) (ExecResult, error) {
	stdout, stderr := newHeadTail(req.MaxOutput), newHeadTail(req.MaxOutput)
	outR, outW, err := os.Pipe()
	if err != nil {
		return ExecResult{}, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return ExecResult{}, err
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = nil // /dev/null：交互式命令读到 EOF 而不是挂起
	cmd.Stdout, cmd.Stderr = outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{outR, outW, errR, errW} {
			f.Close()
		}
		return ExecResult{}, fmt.Errorf("sandbox: start: %w", err)
	}
	// 子进程已经继承了写端；父进程关闭自己的副本，读端才能在子进程
	// 全部退出后读到 EOF。
	outW.Close()
	errW.Close()
	pgid := cmd.Process.Pid

	var readers sync.WaitGroup
	pump := func(r *os.File, buf *headTail, s Stream) {
		defer readers.Done()
		chunk := make([]byte, 8<<10)
		for {
			n, err := r.Read(chunk)
			if n > 0 {
				buf.Write(chunk[:n])
				if onOutput != nil {
					onOutput(s, append([]byte(nil), chunk[:n]...))
				}
			}
			if err != nil {
				return
			}
		}
	}
	readers.Add(2)
	go pump(outR, stdout, Stdout)
	go pump(errR, stderr, Stderr)

	timer := time.NewTimer(req.Timeout)
	defer timer.Stop()
	exited := make(chan struct{})
	var timedOut, cancelled bool
	var killMu sync.Mutex
	go func() {
		select {
		case <-exited:
			return
		case <-timer.C:
			killMu.Lock()
			timedOut = true
			killMu.Unlock()
		case <-ctx.Done():
			killMu.Lock()
			cancelled = true
			killMu.Unlock()
		}
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(killGrace):
		}
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}()

	waitErr := cmd.Wait()
	close(exited)
	// 主进程已退出：清理组内残留进程（例如 `sleep 100 &`）。
	_ = syscall.Kill(-pgid, syscall.SIGKILL)

	// 脱离了进程组的孙进程（setsid）仍可能持有管道；最多再等一会儿。
	done := make(chan struct{})
	go func() { readers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		outR.Close()
		errR.Close()
		<-done
	}
	outR.Close()
	errR.Close()

	res := ExecResult{Duration: time.Since(start), ExitCode: exitCode(cmd.ProcessState)}
	killMu.Lock()
	res.TimedOut, res.Cancelled = timedOut, cancelled
	killMu.Unlock()
	var t1, t2 bool
	res.Stdout, t1 = stdout.String()
	res.Stderr, t2 = stderr.String()
	res.Truncated = t1 || t2
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return res, fmt.Errorf("sandbox: wait: %w", waitErr)
	}
	return res, nil
}

func exitCode(ps *os.ProcessState) int {
	if ps == nil {
		return -1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// passEnv 是从 joey 进程透传给命令的环境变量。其他变量（尤其是
// ONEAPI_API_KEY、JOEY_AUTH_TOKEN、云厂商凭据）一律不传。
var passEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "TERM", "TZ", "TMPDIR"}

// fixedEnv 让常见工具不要进入分页器或交互模式。
var fixedEnv = []string{"PAGER=cat", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0", "NO_COLOR=1"}

// secretMarkers 出现在变量名中时，即使是请求显式传入的也会被丢弃。
var secretMarkers = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL", "COOKIE", "SESSION"}

// IsSecretEnvName 报告 name 看起来是否保存着凭据。
func IsSecretEnvName(name string) bool {
	up := strings.ToUpper(name)
	for _, m := range secretMarkers {
		if strings.Contains(up, m) {
			return true
		}
	}
	return false
}

// buildEnv 返回命令的环境变量。JOEY_SANDBOX、JOEY_SANDBOX_NETWORK_DISABLED
// 告诉命令自己处在什么环境（同 Codex 的 CODEX_SANDBOX、
// CODEX_SANDBOX_NETWORK_DISABLED），测试可以据此跳过需要联网的用例。
func buildEnv(extra map[string]string, p Policy, sandboxed bool) []string {
	env := make([]string, 0, len(passEnv)+len(fixedEnv)+len(extra)+2)
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "LC_") {
			env = append(env, kv)
		}
	}
	env = append(env, fixedEnv...)
	if !sandboxed {
		env = append(env, "JOEY_SANDBOX=none")
	} else {
		env = append(env, "JOEY_SANDBOX=seatbelt")
		if !p.Network {
			env = append(env, "JOEY_SANDBOX_NETWORK_DISABLED=1")
		}
	}
	for k, v := range extra {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || IsSecretEnvName(k) {
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

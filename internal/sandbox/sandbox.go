package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTimeout 是未指定超时的命令的时限（Codex 为 10s，这里放宽，
	// 因为编译、测试类命令经常超过 10 秒）。
	DefaultTimeout = 30 * time.Second
	// MaxTimeout 是允许请求的最长超时。
	MaxTimeout = 10 * time.Minute
	// DefaultMaxOutput 是每个输出流保留的字节数，超出部分从中间截掉。
	DefaultMaxOutput = 64 << 10
	// killGrace 是 SIGTERM 之后等待进程自行退出的时间，之后发送 SIGKILL。
	killGrace = 500 * time.Millisecond
)

// sandboxExec 是 macOS 自带的 Seatbelt 命令行入口。只用 /usr/bin 下的这一份，
// 不在 PATH 中查找，防止被替换（与 Codex 的 MACOS_PATH_TO_SEATBELT_EXECUTABLE
// 相同）。它已被标记为 deprecated，但 Codex、Chrome 等仍在使用。
const sandboxExec = "/usr/bin/sandbox-exec"

// ErrUnsupported 表示当前系统没有 Seatbelt（不是 macOS，或缺少 sandbox-exec）。
var ErrUnsupported = errors.New("sandbox: seatbelt requires macOS with " + sandboxExec)

// ErrClosed 在沙箱关闭后执行命令时返回。
var ErrClosed = errors.New("sandbox: closed")

// Available 报告当前系统能否使用 Seatbelt。
func Available() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := os.Stat(sandboxExec)
	return err == nil
}

// ExecRequest 是一次命令执行。
type ExecRequest struct {
	// Command 由 /bin/sh -c 执行。
	Command string
	// Cwd 是工作目录，相对路径相对于工作区；必须在工作区或额外可写根内。
	Cwd     string
	Timeout time.Duration
	// MaxOutput 是 stdout、stderr 各自保留的字节数。
	MaxOutput int
	// Env 是额外的环境变量。名字像密钥的（含 KEY、TOKEN、SECRET 等）会被丢弃。
	Env map[string]string
	// Unsandboxed 让这一条命令在 Seatbelt 之外执行，对应 Codex 的
	// sandbox_permissions = "require_escalated"。只能在用户批准之后设置；
	// 工作目录校验、环境变量清洗、超时仍然生效。
	Unsandboxed bool
}

// Stream 标识输出来自 stdout 还是 stderr。
type Stream int

const (
	Stdout Stream = iota
	Stderr
)

// OutputFunc 接收实时输出片段，可以为 nil。它在读取输出的 goroutine 中被
// 调用，不应阻塞。
type OutputFunc func(Stream, []byte)

// ExecResult 是一次执行的结果。命令以非零状态退出不算错误。
type ExecResult struct {
	// ExitCode 是退出码；被信号终止时为 128+信号值。
	ExitCode int
	Stdout   string
	Stderr   string
	// TimedOut：超过 Timeout 后被终止。
	TimedOut bool
	// Cancelled：ctx 被取消（例如用户中断）后被终止。
	Cancelled bool
	// Truncated：至少一个输出流超过 MaxOutput，中间部分被省略。
	Truncated bool
	Duration  time.Duration
	// Sandboxed：命令是否在 Seatbelt 中执行（danger-full-access 下为 false）。
	Sandboxed bool
}

// Sandbox 在 Seatbelt 中执行一个工作区的命令。可以被并发使用。
type Sandbox struct {
	guard   *Guard
	profile string
	params  []string
	closed  chan struct{}
	once    sync.Once
}

// New 返回执行策略 p 的沙箱。p.Mode 不是 danger-full-access 时要求本机有
// Seatbelt，否则返回 ErrUnsupported：没有内核隔离就不执行命令，而不是
// 悄悄退化为无保护的执行。
func New(p Policy) (*Sandbox, error) {
	s := &Sandbox{guard: NewGuard(p), closed: make(chan struct{})}
	if p.Mode == FullAccess {
		return s, nil
	}
	if !Available() {
		return nil, ErrUnsupported
	}
	s.profile, s.params = Profile(p)
	return s, nil
}

// Policy 返回沙箱执行的策略。
func (s *Sandbox) Policy() Policy { return s.guard.Policy() }

// Enforced 报告命令是否由 Seatbelt 限制；danger-full-access 下为 false。
func (s *Sandbox) Enforced() bool { return s.profile != "" }

// Profile 返回生成的 Seatbelt 配置和 -D 参数，用于展示和调试。
func (s *Sandbox) Profile() (profile string, params []string) { return s.profile, s.params }

// Close 让之后的 Exec 返回 ErrClosed。正在执行的命令不受影响，它们会
// 按自己的超时或 ctx 结束。
func (s *Sandbox) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// Exec 执行 req。超时、取消时会杀掉命令的整个进程组。
func (s *Sandbox) Exec(ctx context.Context, req ExecRequest, onOutput OutputFunc) (ExecResult, error) {
	select {
	case <-s.closed:
		return ExecResult{}, ErrClosed
	default:
	}
	cwd, err := s.normalize(&req)
	if err != nil {
		return ExecResult{}, err
	}
	sandboxed := s.Enforced() && !req.Unsandboxed
	argv := []string{"/bin/sh", "-c", req.Command}
	if sandboxed {
		args := append([]string{sandboxExec, "-p", s.profile}, s.params...)
		argv = append(append(args, "--"), argv...)
	}
	res, err := runProcess(ctx, argv, cwd, buildEnv(req.Env, s.Policy(), sandboxed), req, onOutput)
	res.Sandboxed = sandboxed
	return res, err
}

// normalize 填充默认值并校验请求，返回解析后的工作目录。工作目录必须在
// 文件工具的可读范围内（工作区和额外可写根）：Seatbelt 下命令虽能读整个
// 文件系统，但"在哪个目录里干活"应当和文件工具一致。
func (s *Sandbox) normalize(req *ExecRequest) (string, error) {
	if strings.TrimSpace(req.Command) == "" {
		return "", errors.New("sandbox: empty command")
	}
	if strings.ContainsRune(req.Command, 0) {
		return "", errors.New("sandbox: command contains NUL")
	}
	if req.Timeout <= 0 {
		req.Timeout = DefaultTimeout
	}
	if req.Timeout > MaxTimeout {
		req.Timeout = MaxTimeout
	}
	if req.MaxOutput <= 0 {
		req.MaxOutput = DefaultMaxOutput
	}
	cwd, err := s.guard.Resolve(req.Cwd, Read)
	if err != nil {
		return "", fmt.Errorf("sandbox: cwd: %w", err)
	}
	return cwd, nil
}

// Summary 返回一行结果描述，例如 "exit 0 in 12ms" 或 "timed out after 1s"。
func (r ExecResult) Summary() string {
	d := r.Duration.Round(time.Millisecond)
	switch {
	case r.TimedOut:
		return fmt.Sprintf("timed out after %s (killed)", d)
	case r.Cancelled:
		return fmt.Sprintf("cancelled after %s (killed)", d)
	}
	s := fmt.Sprintf("exit %d in %s", r.ExitCode, d)
	if r.Truncated {
		s += ", output truncated"
	}
	if r.LikelyDenied() {
		s += ", likely denied by the sandbox"
	}
	return s
}

// deniedMarkers 是被 Seatbelt 拒绝时常见的错误文本。
var deniedMarkers = []string{"operation not permitted", "permission denied", "read-only file system", "sandbox"}

// LikelyDenied 猜测命令是否因为沙箱限制而失败，对应 Codex 的
// is_likely_sandbox_denied。内核拒绝时命令只会看到 EPERM，无法确定失败
// 原因，所以这只是启发式：退出码为 0、2（用法错误）、126、127（命令无法
// 执行或不存在）时不算；否则看输出里是否有典型的拒绝文本。Phase 5 用它
// 提示模型"可能需要申请更高权限"。
func (r ExecResult) LikelyDenied() bool {
	if !r.Sandboxed || r.TimedOut || r.Cancelled {
		return false
	}
	switch r.ExitCode {
	case 0, 2, 126, 127:
		return false
	}
	out := strings.ToLower(r.Stderr + "\n" + r.Stdout)
	for _, m := range deniedMarkers {
		if strings.Contains(out, m) {
			return true
		}
	}
	return false
}

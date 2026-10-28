//go:build !unix

package sandbox

import "context"

// Joey 的命令执行只支持 macOS；这里只是让其他系统上的其余功能仍能编译。
func runProcess(context.Context, []string, string, []string, ExecRequest, OutputFunc) (ExecResult, error) {
	return ExecResult{}, ErrUnsupported
}

func buildEnv(map[string]string, Policy, bool) []string { return nil }

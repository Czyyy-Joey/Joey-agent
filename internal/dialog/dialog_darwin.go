package dialog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// pickScript 通过 argv 接收提示语和初始目录，避免把它们拼进脚本文本。
// activate 让对话框出现在最前面，而不是藏在浏览器窗口后面。
var pickScript = []string{
	"on run argv",
	"activate",
	"set chosen to choose folder with prompt (item 1 of argv) default location (POSIX file (item 2 of argv))",
	"return POSIX path of chosen",
	"end run",
}

func pickFolder(ctx context.Context, prompt, start string) (string, error) {
	args := make([]string, 0, 2*len(pickScript)+2)
	for _, line := range pickScript {
		args = append(args, "-e", line)
	}
	args = append(args, prompt, start)
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		// -128 是 AppleScript 的 "User canceled."；-1713 表示当前会话不能
		// 弹出界面（例如通过 SSH 运行），此时让前端改用内置的目录浏览。
		if strings.Contains(msg, "(-128)") {
			return "", ErrCancelled
		}
		if strings.Contains(msg, "(-1713)") {
			return "", fmt.Errorf("%w: %s", ErrUnsupported, msg)
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return "", fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return "", fmt.Errorf("dialog: osascript: %s", msg)
	}
	p := cleanPath(stdout.String())
	if p == "" {
		return "", errors.New("dialog: osascript returned an empty path")
	}
	return p, nil
}

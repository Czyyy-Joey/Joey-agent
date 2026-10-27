// Package dialog 在运行 joey 的机器上弹出系统原生对话框。
//
// 只有当浏览器和 joey 在同一台机器上时这才有意义：对话框出现在服务器的
// 桌面上。调用方负责确认请求来自本机。
package dialog

import (
	"context"
	"errors"
	"strings"
)

var (
	// ErrCancelled 表示用户关闭了对话框而没有选择。
	ErrCancelled = errors.New("dialog: cancelled by user")
	// ErrUnsupported 表示当前系统不支持原生对话框。
	ErrUnsupported = errors.New("dialog: native dialogs are not supported on this system")
)

// PickFolder 弹出原生目录选择器，初始位于 start，阻塞到用户选择或取消。
// ctx 结束时对话框会被关闭。返回的路径不带末尾的 "/"（根目录除外）。
func PickFolder(ctx context.Context, prompt, start string) (string, error) {
	return pickFolder(ctx, prompt, start)
}

// cleanPath 去掉命令输出末尾的换行和目录分隔符。
func cleanPath(out string) string {
	p := strings.TrimRight(out, "\r\n")
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

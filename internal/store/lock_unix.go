//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// lockFile 对 path 加独占的 flock，并返回释放函数。进程退出（包括崩溃）
// 时内核会自动释放，因此不会残留过期的锁。
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("store: %s is in use by another joey process", path[:len(path)-len(".lock")])
		}
		return nil, fmt.Errorf("store: lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

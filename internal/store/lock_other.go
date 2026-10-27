//go:build !unix

package store

// lockFile 在非 unix 平台上不加锁；同时运行两个使用同一数据库的进程
// 可能导致启动修复误判。
func lockFile(string) (func(), error) { return func() {}, nil }

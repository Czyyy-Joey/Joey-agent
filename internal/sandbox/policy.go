// Package sandbox 是命令执行和文件访问的隔离层，只支持 macOS。
//
// 设计与 Codex 的 macOS 沙箱一致（codex-rs/sandboxing/src/seatbelt.rs）：
//
//   - Policy 描述允许做什么：只读、可写工作区、完全访问，以及网络开关、
//     只读子目录（.git）和禁止读取的目录（~/.ssh）；
//   - Guard 对文件工具的路径执行这套规则；
//   - Sandbox 用 /usr/bin/sandbox-exec 执行命令，由 macOS 内核（Seatbelt）
//     拦截越权的读、写和网络访问。danger-full-access 下不套沙箱；
//   - Assess 根据策略和审批模式判断一条命令能否直接执行。
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Mode 是文件系统权限档位，对应 Codex 的 SandboxPolicy。
type Mode string

const (
	ReadOnly       Mode = "read-only"
	WorkspaceWrite Mode = "workspace-write"
	FullAccess     Mode = "danger-full-access"
)

// DefaultReadOnlySubpaths 是每个可写根下始终只读的子路径，对应 Codex 的
// PROTECTED_METADATA_PATH_NAMES（.git、.codex 等）。
var DefaultReadOnlySubpaths = []string{".git", ".joey"}

// ParseMode 校验 s 并返回对应的 Mode。
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ReadOnly, WorkspaceWrite, FullAccess:
		return m, nil
	}
	return "", fmt.Errorf("sandbox: mode must be %q, %q or %q, got %q", ReadOnly, WorkspaceWrite, FullAccess, s)
}

// PolicyOptions 是 NewPolicy 的可选部分。
type PolicyOptions struct {
	// WritableRoots 是工作区之外额外允许写入的目录（只在 workspace-write 下生效）。
	WritableRoots []string
	// ReadOnlySubpaths 是相对于每个可写根的只读子路径；nil 表示 DefaultReadOnlySubpaths。
	ReadOnlySubpaths []string
	// DenyRead 是绝对路径，命令和文件工具都不能读写其中的内容（对应 Codex 的
	// unreadable roots）。路径可以不存在。
	DenyRead []string
	Network  bool
}

// Policy 是一个工作区的权限规则。用 NewPolicy 构造，路径都已解析为
// 不含符号链接的绝对路径。
type Policy struct {
	Mode      Mode
	Workspace string
	// ExtraWritable 是工作区之外额外允许写入的目录。
	ExtraWritable []string
	// ReadOnlySubpaths 是相对于每个可写根的只读子路径，例如 ".git"。
	ReadOnlySubpaths []string
	// DenyRead 是禁止读写的绝对路径；存在且含符号链接时同时包含解析后的路径。
	DenyRead []string
	Network  bool
}

// NewPolicy 解析 workspace 和 opts 中的路径。workspace 和可写根必须是已存在的目录。
func NewPolicy(mode Mode, workspace string, opts PolicyOptions) (Policy, error) {
	if _, err := ParseMode(string(mode)); err != nil {
		return Policy{}, err
	}
	ws, err := resolveDir(workspace)
	if err != nil {
		return Policy{}, fmt.Errorf("sandbox: workspace: %w", err)
	}
	p := Policy{Mode: mode, Workspace: ws, Network: opts.Network, ReadOnlySubpaths: opts.ReadOnlySubpaths}
	if p.ReadOnlySubpaths == nil {
		p.ReadOnlySubpaths = DefaultReadOnlySubpaths
	}
	for _, sub := range p.ReadOnlySubpaths {
		if sub == "" || filepath.IsAbs(sub) || slices.Contains(strings.Split(filepath.ToSlash(sub), "/"), "..") {
			return Policy{}, fmt.Errorf("sandbox: read-only subpath %q must be a relative path inside the root", sub)
		}
	}
	for _, e := range opts.WritableRoots {
		r, err := resolveDir(e)
		if err != nil {
			return Policy{}, fmt.Errorf("sandbox: writable root %q: %w", e, err)
		}
		if !slices.Contains(p.ExtraWritable, r) && r != ws {
			p.ExtraWritable = append(p.ExtraWritable, r)
		}
	}
	for _, d := range opts.DenyRead {
		if strings.ContainsRune(d, 0) || !filepath.IsAbs(d) {
			return Policy{}, fmt.Errorf("sandbox: deny_read %q must be an absolute path", d)
		}
		for _, v := range withResolved(filepath.Clean(d)) {
			for _, r := range append([]string{ws}, p.ExtraWritable...) {
				if within(v, r) {
					return Policy{}, fmt.Errorf("sandbox: deny_read %q contains the allowed directory %s", d, r)
				}
			}
			if !slices.Contains(p.DenyRead, v) {
				p.DenyRead = append(p.DenyRead, v)
			}
		}
	}
	return p, nil
}

// WritableRoot 是一个可写目录及其中仍然只读的路径（绝对路径）。
type WritableRoot struct {
	Root     string
	ReadOnly []string
}

// WritableRoots 返回文件工具可以写入的目录。read-only 下为空；
// danger-full-access 下也为空，调用方应先检查 Mode。
func (p Policy) WritableRoots() []WritableRoot {
	if p.Mode != WorkspaceWrite {
		return nil
	}
	return p.withReadOnly(append([]string{p.Workspace}, p.ExtraWritable...))
}

// ExecWritableRoots 在 WritableRoots 的基础上加入临时目录（$TMPDIR 和
// /tmp），因为很多命令需要写临时文件，Codex 的 workspace-write 也是如此。
// 文件工具不使用它。
func (p Policy) ExecWritableRoots() []WritableRoot {
	if p.Mode != WorkspaceWrite {
		return nil
	}
	roots := append([]string{p.Workspace}, p.ExtraWritable...)
	for _, t := range []string{os.TempDir(), "/tmp"} {
		r, err := resolveDir(t)
		if err != nil || slices.ContainsFunc(roots, func(root string) bool { return within(root, r) }) {
			continue
		}
		roots = append(roots, r)
	}
	return p.withReadOnly(roots)
}

// withReadOnly 为每个根计算只读路径。只读路径来自所有根，而不只是根自身：
// 工作区位于 $TMPDIR 之下时，$TMPDIR 这个根也必须排除工作区的 .git，
// 否则 .git 会经由 $TMPDIR 重新变成可写。
func (p Policy) withReadOnly(roots []string) []WritableRoot {
	var protected []string
	for _, r := range append([]string{p.Workspace}, p.ExtraWritable...) {
		for _, sub := range p.ReadOnlySubpaths {
			for _, v := range withResolved(filepath.Join(r, sub)) {
				if !slices.Contains(protected, v) {
					protected = append(protected, v)
				}
			}
		}
	}
	out := make([]WritableRoot, 0, len(roots))
	for _, r := range roots {
		w := WritableRoot{Root: r}
		for _, ro := range protected {
			if within(r, ro) && ro != r {
				w.ReadOnly = append(w.ReadOnly, ro)
			}
		}
		out = append(out, w)
	}
	return out
}

// Readable 返回文件工具可以读取的目录：工作区加额外的可写根。
// danger-full-access 下不限制，返回 nil。
//
// 注意命令的读取范围更大：Seatbelt 下命令可以读整个文件系统（DenyRead 除外），
// 与 Codex 相同。文件工具更严格，是为了让模型的文件访问集中在工作区。
func (p Policy) Readable() []string {
	if p.Mode == FullAccess {
		return nil
	}
	return append([]string{p.Workspace}, p.ExtraWritable...)
}

// String 返回策略的一行描述，用于日志和系统提示词。
func (p Policy) String() string {
	net := "off"
	if p.Network {
		net = "on"
	}
	return fmt.Sprintf("mode=%s workspace=%s network=%s", p.Mode, p.Workspace, net)
}

// withResolved 返回 p，以及 p 存在且解析符号链接后不同时解析后的路径。
// Seatbelt 按内核看到的真实路径匹配规则，两个都写进配置才能覆盖
// "通过别名访问"和"直接访问目标"两种方式。
func withResolved(p string) []string {
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		out = append(out, r)
	}
	return out
}

func resolveDir(dir string) (string, error) {
	if strings.ContainsRune(dir, 0) {
		return "", errors.New("invalid path")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return resolved, nil
}

// within 报告 p 是否等于 root 或位于 root 之下。两者都应是干净的绝对路径。
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

package sandbox

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Access 是一次文件访问的类型。
type Access int

const (
	Read Access = iota
	Write
)

func (a Access) String() string {
	if a == Write {
		return "write"
	}
	return "read"
}

// Reason 说明路径为什么被拒绝。
type Reason int

const (
	// ReasonInvalid：路径格式不合法（例如包含 NUL）。
	ReasonInvalid Reason = iota + 1
	// ReasonOutside：路径本身在允许的目录之外。
	ReasonOutside
	// ReasonEscapes：路径在允许的目录内，但解析符号链接后跑到了外面。
	ReasonEscapes
	// ReasonReadOnlySubpath：路径落在 .git 这类只读子目录里。
	ReasonReadOnlySubpath
	// ReasonReadOnlyPolicy：策略是 read-only，任何写入都不允许。
	ReasonReadOnlyPolicy
	// ReasonNotExist：要读取的路径不存在。
	ReasonNotExist
	// ReasonDenied：路径落在 deny_read 列出的目录里（例如 ~/.ssh）。
	ReasonDenied
)

// PathError 是 Guard 拒绝一个路径时返回的错误。
type PathError struct {
	Path   string
	Access Access
	Reason Reason
	Err    error
}

func (e *PathError) Error() string {
	switch e.Reason {
	case ReasonInvalid:
		return fmt.Sprintf("invalid path %q", e.Path)
	case ReasonOutside:
		return fmt.Sprintf("%s %q is outside the allowed directories", e.Access, e.Path)
	case ReasonEscapes:
		return fmt.Sprintf("%s %q resolves outside the allowed directories", e.Access, e.Path)
	case ReasonReadOnlySubpath:
		return fmt.Sprintf("%q is inside a read-only directory", e.Path)
	case ReasonReadOnlyPolicy:
		return fmt.Sprintf("cannot write %q: the sandbox is read-only", e.Path)
	case ReasonNotExist:
		return fmt.Sprintf("%q does not exist", e.Path)
	case ReasonDenied:
		return fmt.Sprintf("%s %q is denied by the sandbox (deny_read)", e.Access, e.Path)
	}
	return fmt.Sprintf("%s %q: %v", e.Access, e.Path, e.Err)
}

func (e *PathError) Unwrap() error { return e.Err }

// Guard 对文件路径执行 Policy（对应计划中的 fsguard）。所有文件类工具都
// 必须经过它。它在检查前解析符号链接，但无法防止检查和使用之间文件被
// 替换（TOCTOU），这是已知并接受的限制。
type Guard struct {
	p Policy
}

// NewGuard 返回执行 p 的 Guard。
func NewGuard(p Policy) *Guard { return &Guard{p: p} }

// Policy 返回 Guard 执行的策略。
func (g *Guard) Policy() Policy { return g.p }

// Root 返回工作区目录。
func (g *Guard) Root() string { return g.p.Workspace }

// Resolve 把 p 映射为允许以 access 方式访问的绝对路径。相对路径相对于
// 工作区。读取要求路径存在；写入允许目标不存在，但它最近的已存在上级
// 目录必须在可写范围内。
func (g *Guard) Resolve(p string, access Access) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", &PathError{Path: p, Access: access, Reason: ReasonInvalid}
	}
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.p.Workspace, p)
	}
	p = filepath.Clean(p)
	if access == Write && g.p.Mode == ReadOnly {
		return "", &PathError{Path: g.Rel(p), Access: access, Reason: ReasonReadOnlyPolicy}
	}
	if g.denied(p) {
		return "", &PathError{Path: p, Access: access, Reason: ReasonDenied}
	}
	if !g.allowed(p, access) {
		return "", &PathError{Path: p, Access: access, Reason: ReasonOutside}
	}
	resolved, err := resolveExisting(p, access)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", &PathError{Path: g.Rel(p), Access: access, Reason: ReasonNotExist, Err: err}
		}
		return "", &PathError{Path: g.Rel(p), Access: access, Err: err}
	}
	if g.denied(resolved) {
		return "", &PathError{Path: g.Rel(p), Access: access, Reason: ReasonDenied}
	}
	if !g.allowed(resolved, access) {
		reason := ReasonEscapes
		if access == Write && g.readOnlySub(resolved) {
			reason = ReasonReadOnlySubpath
		}
		return "", &PathError{Path: g.Rel(p), Access: access, Reason: reason}
	}
	return resolved, nil
}

// Rel 返回 p 相对于工作区的路径以便展示；在工作区之外则原样返回。
func (g *Guard) Rel(p string) string {
	if !within(g.p.Workspace, p) {
		return p
	}
	rel, err := filepath.Rel(g.p.Workspace, p)
	if err != nil {
		return p
	}
	return rel
}

func (g *Guard) allowed(p string, access Access) bool {
	if g.p.Mode == FullAccess {
		return true
	}
	if access == Read {
		for _, r := range g.p.Readable() {
			if within(r, p) {
				return true
			}
		}
		return false
	}
	for _, w := range g.p.WritableRoots() {
		if within(w.Root, p) && !underAny(w.ReadOnly, p) {
			return true
		}
	}
	return false
}

// Denied 报告绝对路径 p 是否落在 deny_read 中。遍历目录的工具（search_files）
// 用它跳过受保护的子树，而不必对每个文件调用 Resolve。
func (g *Guard) Denied(p string) bool { return g.denied(filepath.Clean(p)) }

// denied 报告 p 是否落在 deny_read 中。danger-full-access 不套沙箱，
// deny_read 也随之失效，文件工具与命令保持一致。
func (g *Guard) denied(p string) bool {
	return g.p.Mode != FullAccess && underAny(g.p.DenyRead, p)
}

func (g *Guard) readOnlySub(p string) bool {
	for _, w := range g.p.WritableRoots() {
		if within(w.Root, p) && underAny(w.ReadOnly, p) {
			return true
		}
	}
	return false
}

func underAny(roots []string, p string) bool {
	for _, r := range roots {
		if within(r, p) {
			return true
		}
	}
	return false
}

// resolveExisting 解析 p 的符号链接。写入时 p 可以不存在：解析最近的已存在
// 上级目录，再接上剩余部分（已经 Clean 过，不含 ".."）。途中遇到悬空的
// 符号链接时跟随它，因为写入会在链接指向的位置创建文件。
func resolveExisting(p string, access Access) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil || access == Read || !errors.Is(err, fs.ErrNotExist) {
		return resolved, err
	}
	return resolveMissing(p, 0)
}

func resolveMissing(p string, depth int) (string, error) {
	if depth > 40 {
		return "", errors.New("too many levels of symbolic links")
	}
	dir, rest := p, ""
	for {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			if rest == "" {
				return r, nil
			}
			info, err := os.Stat(r)
			if err != nil {
				return "", err
			}
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", dir)
			}
			return filepath.Join(r, rest), nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if fi, err := os.Lstat(dir); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(dir)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(dir), target)
			}
			return resolveMissing(filepath.Join(filepath.Clean(target), rest), depth+1)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fs.ErrNotExist
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

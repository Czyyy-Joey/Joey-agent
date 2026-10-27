package builtin

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// Workspace 把文件工具限制在一棵目录树内。
//
// 这是 Phase 1 的最小路径防护；Phase 4 会把它移入 sandbox 包。
// 它会在检查包含关系之前解析符号链接，但文件仍可能在
// 检查与实际使用之间被替换。
type Workspace struct {
	root string
}

// NewWorkspace 把 root 解析为绝对路径且不含符号链接的目录。
func NewWorkspace(root string) (*Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("workspace is not a directory: " + root)
	}
	return &Workspace{root: resolved}, nil
}

// Root 返回解析后的工作区目录。
func (w *Workspace) Root() string { return w.root }

// Resolve 把模型提供的路径映射为工作区内一个已存在的绝对路径。
// 相对路径相对于根目录；"" 表示根目录。
func (w *Workspace) Resolve(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", tools.Errorf("invalid path")
	}
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.root, p)
	}
	p = filepath.Clean(p)
	if !w.contains(p) {
		return "", tools.Errorf("path %q is outside the workspace %s", p, w.root)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", tools.Errorf("path %q does not exist", w.Rel(p))
		}
		return "", tools.Errorf("cannot access %q: %v", w.Rel(p), err)
	}
	if !w.contains(resolved) {
		return "", tools.Errorf("path %q resolves outside the workspace", w.Rel(p))
	}
	return resolved, nil
}

// Rel 返回 p 相对于根目录的路径以便展示；如果 p 在根目录之外，则返回 p 本身。
func (w *Workspace) Rel(p string) string {
	rel, err := filepath.Rel(w.root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return p
	}
	return rel
}

func (w *Workspace) contains(p string) bool {
	rel, err := filepath.Rel(w.root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

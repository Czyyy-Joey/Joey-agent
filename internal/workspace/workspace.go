// Package workspace 决定会话可以把哪些本地目录作为工作目录。
//
// 每个会话在创建时选择一个工作目录，文件工具只能访问该目录。Policy
// 再限制可选目录的范围：必须位于某个允许的根目录之下（默认工作目录
// 本身总是允许的）。这是 API 层的访问控制，不是沙箱。
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	// ErrInvalid 表示路径格式不合法（例如不是绝对路径）。
	ErrInvalid = errors.New("invalid directory")
	// ErrNotFound 表示目录不存在或不是目录。
	ErrNotFound = errors.New("directory not found")
	// ErrOutside 表示目录不在任何允许的根目录之下。
	ErrOutside = errors.New("directory is outside the allowed roots")
)

// Policy 是会话工作目录的选择规则。用 NewPolicy 构造。
type Policy struct {
	def   string
	roots []string
}

// NewPolicy 解析默认目录 def 和允许的根目录列表。路径可以用 "~" 开头；
// def 可以是相对路径（相对于进程当前目录）。不存在的目录会报错。
func NewPolicy(def string, allowed []string) (*Policy, error) {
	d, err := resolveDir(ExpandHome(def))
	if err != nil {
		return nil, fmt.Errorf("workspace: default directory %q: %w", def, err)
	}
	p := &Policy{def: d, roots: []string{d}}
	for _, a := range allowed {
		r, err := resolveDir(ExpandHome(a))
		if err != nil {
			return nil, fmt.Errorf("workspace: allowed root %q: %w", a, err)
		}
		p.roots = append(p.roots, r)
	}
	p.roots = dedupe(p.roots)
	return p, nil
}

// Default 返回解析后的默认工作目录。
func (p *Policy) Default() string { return p.def }

// Roots 返回允许的根目录（含默认目录），已去掉被其他根包含的项。
func (p *Policy) Roots() []string { return append([]string(nil), p.roots...) }

// Resolve 校验 dir 并返回解析掉符号链接后的绝对路径。"" 表示默认目录。
func (p *Policy) Resolve(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return p.def, nil
	}
	dir = ExpandHome(strings.TrimSpace(dir))
	if strings.ContainsRune(dir, 0) || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w: %q must be an absolute path", ErrInvalid, dir)
	}
	resolved, err := resolveDir(dir)
	if err != nil {
		return "", err
	}
	if !p.allowed(resolved) {
		return "", fmt.Errorf("%w: %s", ErrOutside, resolved)
	}
	return resolved, nil
}

func (p *Policy) allowed(dir string) bool {
	for _, r := range p.roots {
		if Contains(r, dir) {
			return true
		}
	}
	return false
}

// Entry 是目录列表中的一个子目录。
type Entry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Listing 是 List 的结果。
type Listing struct {
	// Path 是被列出的目录；列出根目录时为 ""。
	Path string `json:"path"`
	// Parent 是可以返回的上级目录；已经在根目录或列出根目录时为 ""。
	Parent    string  `json:"parent"`
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated,omitempty"`
	Roots     []Entry `json:"roots"`
	Default   string  `json:"default"`
}

// maxEntries 限制一次列出的子目录数量。
const maxEntries = 1000

// List 列出 dir 的子目录，供前端选择工作目录。dir 为 "" 时列出根目录。
// 隐藏目录（以 "." 开头）只在 hidden 为 true 时返回。
func (p *Policy) List(dir string, hidden bool) (Listing, error) {
	l := Listing{Default: p.def, Entries: []Entry{}}
	for _, r := range p.roots {
		l.Roots = append(l.Roots, Entry{Name: displayName(r), Path: r})
	}
	if strings.TrimSpace(dir) == "" {
		l.Entries = l.Roots
		return l, nil
	}
	resolved, err := p.Resolve(dir)
	if err != nil {
		return l, err
	}
	l.Path = resolved
	if parent := filepath.Dir(resolved); parent != resolved && p.allowed(parent) {
		l.Parent = parent
	}
	des, err := os.ReadDir(resolved)
	if err != nil {
		return l, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	for _, de := range des {
		name := de.Name()
		if !hidden && strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(resolved, name)
		isDir := de.IsDir()
		if de.Type()&fs.ModeSymlink != 0 {
			info, err := os.Stat(full)
			isDir = err == nil && info.IsDir()
		}
		if !isDir {
			continue
		}
		if len(l.Entries) == maxEntries {
			l.Truncated = true
			break
		}
		l.Entries = append(l.Entries, Entry{Name: name, Path: full})
	}
	sort.Slice(l.Entries, func(i, j int) bool {
		return strings.ToLower(l.Entries[i].Name) < strings.ToLower(l.Entries[j].Name)
	})
	return l, nil
}

// ExpandHome 把开头的 "~" 或 "~/" 替换为用户主目录。
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// Contains 报告 p 是否等于 root 或位于 root 之下。两者都应是干净的绝对路径。
func Contains(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func resolveDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotFound, abs)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a directory", ErrNotFound, abs)
	}
	return resolved, nil
}

// dedupe 去掉重复的根以及被其他根包含的根，保持原有顺序。
func dedupe(roots []string) []string {
	var out []string
	for i, r := range roots {
		covered := false
		for j, o := range roots {
			if i != j && Contains(o, r) && (o != r || j < i) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, r)
		}
	}
	return out
}

func displayName(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if h, err := filepath.EvalSymlinks(home); err == nil && Contains(h, p) {
			rel, _ := filepath.Rel(h, p)
			if rel == "." {
				return "~"
			}
			return "~/" + rel
		}
	}
	return p
}

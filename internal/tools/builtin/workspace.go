package builtin

import (
	"errors"

	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// Workspace 是文件工具使用的路径守卫，它把 sandbox.Guard 的拒绝转换成
// 回传给模型的工具错误。所有文件类工具都必须经过它。
type Workspace struct {
	g *sandbox.Guard
}

// NewWorkspace 返回只能读取 root 的守卫。
func NewWorkspace(root string) (*Workspace, error) {
	p, err := sandbox.NewPolicy(sandbox.ReadOnly, root, sandbox.PolicyOptions{})
	if err != nil {
		return nil, err
	}
	return &Workspace{g: sandbox.NewGuard(p)}, nil
}

// WorkspaceFromGuard 返回执行 g 的策略的守卫。
func WorkspaceFromGuard(g *sandbox.Guard) *Workspace { return &Workspace{g: g} }

// Root 返回解析后的工作区目录。
func (w *Workspace) Root() string { return w.g.Root() }

// Resolve 把模型提供的路径映射为允许读取的绝对路径。
func (w *Workspace) Resolve(p string) (string, error) {
	return w.resolve(p, sandbox.Read)
}

// ResolveWrite 把模型提供的路径映射为允许写入的绝对路径（目标可以不存在）。
func (w *Workspace) ResolveWrite(p string) (string, error) {
	return w.resolve(p, sandbox.Write)
}

func (w *Workspace) resolve(p string, a sandbox.Access) (string, error) {
	r, err := w.g.Resolve(p, a)
	var pe *sandbox.PathError
	if errors.As(err, &pe) {
		return "", tools.Errorf("%s", pe.Error())
	}
	return r, err
}

// Rel 返回 p 相对于工作区的路径以便展示；在工作区之外则返回 p 本身。
func (w *Workspace) Rel(p string) string { return w.g.Rel(p) }

// Policy 返回守卫执行的沙箱策略。
func (w *Workspace) Policy() sandbox.Policy { return w.g.Policy() }

// Denied 报告绝对路径 p 是否在 deny_read 中。
func (w *Workspace) Denied(p string) bool { return w.g.Denied(p) }

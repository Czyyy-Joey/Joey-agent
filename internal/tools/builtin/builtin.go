package builtin

import (
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// Env 是内置工具共用的会话资源。
type Env struct {
	// WS 是文件工具的路径守卫，必填。
	WS *Workspace
	// Sandbox 执行命令；为 nil 时不提供 exec_command。
	Sandbox *sandbox.Sandbox
	// Approval、Rules 决定命令和文件修改何时需要用户批准。
	Approval sandbox.ApprovalMode
	Rules    sandbox.Rules
	// MaxOutput 是 exec_command 每个输出流保留的字节数；0 表示默认值。
	MaxOutput int
}

// Tools 返回绑定到 env 的全部内置工具：read_file、list_directory、
// search_files、write_file、apply_patch，有沙箱时还有 exec_command。
func Tools(env Env) []tools.Tool {
	ts := []tools.Tool{
		&ReadFile{WS: env.WS},
		&ListDirectory{WS: env.WS},
		&SearchFiles{WS: env.WS},
		&WriteFile{Env: env},
		&ApplyPatch{Env: env},
	}
	if env.Sandbox != nil {
		ts = append(ts, &ExecCommand{Env: env})
	}
	return ts
}

// ReadOnlyTools 返回只读的文件工具（read_file、list_directory、search_files）。
func ReadOnlyTools(ws *Workspace) []tools.Tool {
	return []tools.Tool{&ReadFile{WS: ws}, &ListDirectory{WS: ws}, &SearchFiles{WS: ws}}
}

// editNeedsApproval 报告修改文件前是否要先问用户。与 Codex 相同：
// untrusted 下每次修改都要确认；on-request、never 下，可写范围内的修改
// 直接执行（范围外的修改由 Guard 拒绝，不会进入审批）。
func (e Env) editNeedsApproval() bool {
	return e.Approval == sandbox.ApproveUntrusted
}

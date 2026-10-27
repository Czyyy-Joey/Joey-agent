package builtin

import "github.com/Czyyy-Joey/joey-agent/internal/tools"

// Tools 返回绑定到 ws 的 Phase 1 只读工具。
func Tools(ws *Workspace) []tools.Tool {
	return []tools.Tool{
		&ReadFile{WS: ws},
		&ListDirectory{WS: ws},
	}
}

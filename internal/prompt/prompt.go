// Package prompt 分层装配系统提示词：
//
//  1. 基础指令（Joey 是谁、怎么用工具）；
//  2. 环境上下文：工作目录、shell、沙箱策略、审批模式、日期；
//  3. 项目指令：从项目根（含 .git 的最近祖先目录）到工作目录，沿途每一级
//     的 AGENTS.md 或 JOEY.md，按从上到下的顺序拼接。
//
// 它对应 Codex 的 agents_md.rs（AGENTS.md 发现）和 environment_context。
// Codex 把后两层作为用户消息写进历史，压缩后再重新注入；Joey 每次请求都
// 重新生成系统提示词，所以压缩时不需要额外处理。
package prompt

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DocNames 是项目指令文件名。同一目录下两者都存在时只读第一个。
var DocNames = []string{"AGENTS.md", "JOEY.md"}

// MaxDocBytes 是所有项目指令加起来的上限，与 Codex 的
// project_doc_max_bytes 默认值相同。
const MaxDocBytes = 32 << 10

// Env 是环境上下文。
type Env struct {
	Workspace string
	Shell     string
	// SandboxMode、Network、Approval 描述命令的执行限制；SandboxMode 为空
	// 表示会话不能执行命令。
	SandboxMode string
	Network     bool
	Approval    string
	Date        time.Time
}

// Doc 是一个项目指令文件。
type Doc struct {
	Path    string
	Content string
	// Truncated 表示因为超过 MaxDocBytes 只保留了开头。
	Truncated bool
}

// Build 返回完整的系统提示词。base 是基础指令。
func Build(base string, env Env, docs []Doc) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(base))
	b.WriteString("\n\n")
	b.WriteString(Environment(env))
	for _, d := range docs {
		fmt.Fprintf(&b, "\n\n# %s instructions for %s\n\n<INSTRUCTIONS>\n%s\n", filepath.Base(d.Path), filepath.Dir(d.Path), strings.TrimSpace(d.Content))
		if d.Truncated {
			b.WriteString("(truncated)\n")
		}
		b.WriteString("</INSTRUCTIONS>")
	}
	return b.String()
}

// Environment 把 env 渲染成 <environment_context> 块。
func Environment(env Env) string {
	var b strings.Builder
	b.WriteString("<environment_context>\n")
	fmt.Fprintf(&b, "  <cwd>%s</cwd>\n", env.Workspace)
	if env.Shell != "" {
		fmt.Fprintf(&b, "  <shell>%s</shell>\n", env.Shell)
	}
	if env.SandboxMode != "" {
		fmt.Fprintf(&b, "  <sandbox_mode>%s</sandbox_mode>\n", env.SandboxMode)
		network := "restricted"
		if env.Network {
			network = "enabled"
		}
		fmt.Fprintf(&b, "  <network_access>%s</network_access>\n", network)
	}
	if env.Approval != "" {
		fmt.Fprintf(&b, "  <approval_policy>%s</approval_policy>\n", env.Approval)
	}
	if !env.Date.IsZero() {
		fmt.Fprintf(&b, "  <current_date>%s</current_date>\n", env.Date.Format("2006-01-02"))
		fmt.Fprintf(&b, "  <timezone>%s</timezone>\n", env.Date.Location())
	}
	b.WriteString("</environment_context>")
	return b.String()
}

// LoadDocs 找出 workspace 适用的项目指令。项目根是 workspace 往上第一个
// 含有 .git 的目录；没有找到时只看 workspace 本身。不会越过项目根，也不
// 会越过 stop（通常是用户主目录；为空表示不限制）。读不到的文件被跳过。
func LoadDocs(workspace, stop string) []Doc {
	dirs := []string{workspace}
	for d := workspace; ; {
		if exists(filepath.Join(d, ".git")) {
			// dirs 的最后一个就是项目根。
			break
		}
		parent := filepath.Dir(d)
		if parent == d || d == stop {
			dirs = []string{workspace}
			break
		}
		d = parent
		dirs = append(dirs, d)
	}
	var docs []Doc
	remaining := MaxDocBytes
	for i := len(dirs) - 1; i >= 0 && remaining > 0; i-- {
		for _, name := range DocNames {
			p := filepath.Join(dirs[i], name)
			b, err := os.ReadFile(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				// 例如 deny_read 或权限问题；不让它阻止会话。
				break
			}
			if strings.TrimSpace(string(b)) == "" {
				break
			}
			d := Doc{Path: p, Content: string(b)}
			if len(b) > remaining {
				d.Content, d.Truncated = strings.ToValidUTF8(string(b[:remaining]), ""), true
			}
			remaining -= len(d.Content)
			if d.Truncated {
				remaining = 0
			}
			docs = append(docs, d)
			break
		}
	}
	return docs
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

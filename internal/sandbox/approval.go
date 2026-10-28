package sandbox

import (
	"fmt"
	"regexp"
	"strings"
)

// ApprovalMode 决定什么时候需要人工确认，对应 Codex 的 AskForApproval。
type ApprovalMode string

const (
	// ApproveUntrusted：只有已知安全的只读命令自动执行，其余都要确认。
	ApproveUntrusted ApprovalMode = "untrusted"
	// ApproveOnRequest：交给沙箱兜底，只有危险命令需要确认。Phase 5 起模型
	// 还可以主动申请在沙箱外执行，那时也需要确认。
	ApproveOnRequest ApprovalMode = "on-request"
	// ApproveNever：从不询问。能执行的直接执行，危险命令直接拒绝。
	ApproveNever ApprovalMode = "never"
)

// ParseApprovalMode 校验 s 并返回对应的 ApprovalMode。
func ParseApprovalMode(s string) (ApprovalMode, error) {
	switch m := ApprovalMode(s); m {
	case ApproveUntrusted, ApproveOnRequest, ApproveNever:
		return m, nil
	}
	return "", fmt.Errorf("sandbox: approval must be %q, %q or %q, got %q", ApproveUntrusted, ApproveOnRequest, ApproveNever, s)
}

// Decision 是一条命令的审批判定，对应 Codex 的 ExecApprovalRequirement。
type Decision int

const (
	// Skip：不需要确认，直接执行。
	Skip Decision = iota
	// NeedsApproval：执行前需要用户确认。
	NeedsApproval
	// Forbidden：不允许执行。
	Forbidden
)

func (d Decision) String() string {
	switch d {
	case Skip:
		return "skip"
	case NeedsApproval:
		return "needs_approval"
	}
	return "forbidden"
}

// Assessment 是判定结果及其原因（会展示给用户和模型）。
type Assessment struct {
	Decision Decision
	Reason   string
}

// Rules 是配置中的命令前缀规则。前缀按空白切分后逐词比较，例如
// "git push" 匹配 "git push origin main"，但不匹配 "git pushx"。
type Rules struct {
	Allow []string
	Deny  []string
}

// Assess 判断 command 在策略 p、审批模式 mode 下能否直接执行，对应 Codex
// exec_policy.rs 的 render_decision_for_unmatched_command。
//
// 判定顺序：
//  1. 命中 Deny 规则 → Forbidden；
//  2. 危险命令（rm -rf /、sudo、mkfs 等）→ never 模式下 Forbidden，否则 NeedsApproval。
//     即使有沙箱也要确认：danger-full-access 下没有沙箱，而且 workspace-write
//     下 rm -rf . 仍能清空整个工作区；
//  3. 命中 Allow 规则，或是已知安全的只读命令 → Skip；
//  4. 其余按审批模式：untrusted → NeedsApproval；on-request、never → Skip，
//     由 Seatbelt 拦截越权操作（danger-full-access 下用户已选择不加限制）。
func Assess(command string, p Policy, mode ApprovalMode, rules Rules) Assessment {
	cmd := strings.TrimSpace(command)
	words := strings.Fields(cmd)
	for _, d := range rules.Deny {
		if hasPrefixWords(words, d) {
			return Assessment{Forbidden, fmt.Sprintf("matches deny rule %q", d)}
		}
	}
	if why := dangerous(cmd); why != "" {
		if mode == ApproveNever {
			return Assessment{Forbidden, why + "; approval is disabled (approval=never)"}
		}
		return Assessment{NeedsApproval, why}
	}
	for _, a := range rules.Allow {
		if hasPrefixWords(words, a) && !hasShellControl(cmd) {
			return Assessment{Skip, fmt.Sprintf("matches allow rule %q", a)}
		}
	}
	if KnownSafe(cmd) {
		return Assessment{Skip, "known read-only command"}
	}
	switch {
	case mode == ApproveUntrusted:
	case p.Mode == FullAccess:
		return Assessment{Skip, "no sandbox restrictions (danger-full-access)"}
	default:
		return Assessment{Skip, "runs inside the " + string(p.Mode) + " sandbox"}
	}
	return Assessment{NeedsApproval, "only known read-only commands run without approval (approval=untrusted)"}
}

func hasPrefixWords(words []string, prefix string) bool {
	pw := strings.Fields(prefix)
	if len(pw) == 0 || len(pw) > len(words) {
		return false
	}
	for i := range pw {
		if words[i] != pw[i] {
			return false
		}
	}
	return true
}

// hasShellControl 报告命令是否包含会让"按前缀判断"失效的 shell 语法：
// 命令拼接、管道、重定向、命令替换、后台执行、换行。
func hasShellControl(cmd string) bool {
	return strings.ContainsAny(cmd, ";&|<>`\n\r") || strings.Contains(cmd, "$(")
}

// safeCommands 是不带参数限制就可以认为只读的命令。
var safeCommands = map[string]bool{
	"ls": true, "pwd": true, "cat": true, "head": true, "tail": true, "wc": true,
	"echo": true, "printf": true, "grep": true, "rg": true, "which": true, "whoami": true,
	"stat": true, "file": true, "du": true, "df": true, "tree": true, "uname": true,
	"date": true, "true": true, "false": true, "basename": true, "dirname": true,
	"realpath": true, "diff": true, "cut": true, "sort": true, "uniq": true, "tr": true,
	"nl": true,
}

// safeGit 是只读的 git 子命令。
var safeGit = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "branch": true,
	"rev-parse": true, "ls-files": true, "blame": true,
}

var sedPrint = regexp.MustCompile(`^'?\d+(,\d+)?p'?$`)

// KnownSafe 报告 cmd 是否是已知的只读命令，对应 Codex 的
// is_known_safe_command。含有任何 shell 控制语法的命令都不算。
func KnownSafe(cmd string) bool {
	if hasShellControl(cmd) {
		return false
	}
	w := strings.Fields(cmd)
	if len(w) == 0 {
		return false
	}
	switch name := w[0]; {
	case safeCommands[name]:
		return true
	case name == "git":
		if len(w) < 2 || !safeGit[w[1]] {
			return false
		}
		for _, a := range w[2:] {
			// 这些参数会让只读子命令写文件或执行外部程序。
			if a == "--output" || strings.HasPrefix(a, "--output=") || strings.HasPrefix(a, "--ext-diff") || a == "-c" {
				return false
			}
		}
		return true
	case name == "find":
		for _, a := range w[1:] {
			switch a {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls":
				return false
			}
		}
		return true
	case name == "sed":
		// 只允许 sed -n '1,20p' file 这种只打印的用法。
		return len(w) >= 3 && w[1] == "-n" && sedPrint.MatchString(w[2])
	}
	return false
}

var dangerousPatterns = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(^|[\s;&|(])sudo\s`), "uses sudo"},
	{regexp.MustCompile(`(^|[\s;&|(])su(\s|$)`), "switches user"},
	{regexp.MustCompile(`\brm\s+(-[a-zA-Z]*\s+)*-[a-zA-Z]*[rR][a-zA-Z]*\s+(-[a-zA-Z]*\s+)*(/|~|\$HOME|/\*|\.\.?)(\s|/?$|/\*)`), "recursively deletes a root, home or parent directory"},
	{regexp.MustCompile(`\bmkfs(\.\w+)?\b`), "formats a file system"},
	{regexp.MustCompile(`\bdd\b.*\bof=/dev/`), "writes to a raw device"},
	{regexp.MustCompile(`>\s*/dev/(sd|disk|nvme|rdisk)`), "writes to a raw device"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), "is a fork bomb"},
	{regexp.MustCompile(`(^|[\s;&|(])(shutdown|reboot|halt|poweroff)(\s|$)`), "shuts down the machine"},
	{regexp.MustCompile(`\bchmod\s+(-R\s+)?[0-7]*777\s+/`), "makes system paths world-writable"},
	{regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(sh|bash|zsh)\b`), "pipes a download into a shell"},
	{regexp.MustCompile(`\bgit\s+push\b.*(--force|-f)\b`), "force-pushes"},
}

// dangerous 返回命令危险的原因；不危险时返回 ""。这是一份尽力而为的
// 名单，不能代替沙箱，只是保证最明显的破坏性命令一定经过确认。
func dangerous(cmd string) string {
	for _, d := range dangerousPatterns {
		if d.re.MatchString(cmd) {
			return "command " + d.why
		}
	}
	return ""
}

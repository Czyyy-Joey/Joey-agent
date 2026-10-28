package sandbox

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

var (
	//go:embed seatbelt_base.sbpl
	seatbeltBase string
	//go:embed seatbelt_network.sbpl
	seatbeltNetwork string
	//go:embed seatbelt_preferences.sbpl
	seatbeltPreferences string
)

// seatbeltTLSTrust 让系统 libcurl 能校验证书，只在联网时加入（同 Codex）。
const seatbeltTLSTrust = `(allow mach-lookup (global-name "com.apple.TrustEvaluationAgent"))`

// Profile 把策略翻译成 Seatbelt 配置和 sandbox-exec 的 -D 参数，对应 Codex
// 的 create_seatbelt_command_args。danger-full-access 返回空配置，表示不套沙箱。
//
// 配置从 deny default 开始（seatbelt_base.sbpl），再依次追加：
//
//  1. 读取：允许整个文件系统；
//  2. 写入：每个可写根一条 subpath 规则，并用 require-not 排除其中的只读路径
//     （.git 等）。排除同时写 literal 和 subpath，否则 `mkdir .git` 这种
//     "创建只读目录本身"的操作会漏掉；
//  3. 网络：network=false 时什么都不加，deny default 已经拒绝了所有网络访问，
//     包括 unix socket（否则命令可以经 /var/run/docker.sock 一类本地服务绕过限制）；
//  4. deny_read：禁止读写，放在所有 allow 之后。Seatbelt 中后出现的规则优先；
//  5. 禁止删除或改名可写根本身，以及只读路径、deny_read 路径在可写根内的
//     各级上级目录。否则 `mv ws ws2`、`mv sub sub2` 能把受保护的内容移到
//     规则匹配不到的路径下。
//
// 所有路径都通过 (param "...") 传入，不拼接进配置文本，避免路径中的引号等
// 字符改变配置的含义。
func Profile(p Policy) (profile string, params []string) {
	if p.Mode == FullAccess {
		return "", nil
	}
	var b strings.Builder
	param := func(key, path string) string {
		params = append(params, "-D"+key+"="+path)
		return fmt.Sprintf("(param %q)", key)
	}
	section := func(comment string) { fmt.Fprintf(&b, "\n; %s\n", comment) }

	b.WriteString(seatbeltBase)

	section("读取：整个文件系统（deny_read 除外，见后）")
	b.WriteString("(allow file-read*)\n")

	roots := p.ExecWritableRoots()
	var anchors []string
	if len(roots) > 0 {
		section("写入：可写根，排除其中的只读路径")
		b.WriteString("(allow file-write*\n")
		for i, w := range roots {
			root := param(fmt.Sprintf("WRITABLE_ROOT_%d", i), w.Root)
			anchors = append(anchors, root)
			if len(w.ReadOnly) == 0 {
				fmt.Fprintf(&b, "  (subpath %s)\n", root)
				continue
			}
			fmt.Fprintf(&b, "  (require-all (subpath %s)", root)
			for j, ro := range w.ReadOnly {
				ro := param(fmt.Sprintf("WRITABLE_ROOT_%d_RO_%d", i, j), ro)
				fmt.Fprintf(&b, "\n    (require-not (literal %s)) (require-not (subpath %s))", ro, ro)
			}
			b.WriteString(")\n")
		}
		b.WriteString(")\n")
	}
	// 命令的 stdout、stderr 是管道，直接写不受文件规则限制；但 `echo x > /dev/stderr`
	// 会按路径打开设备文件，需要单独放开。
	b.WriteString(`(allow file-write-data (literal "/dev/stdout") (literal "/dev/stderr") (regex #"^/dev/fd/[0-9]+$"))` + "\n")

	if p.Network {
		section("网络：允许")
		b.WriteString("(allow network-outbound)\n(allow network-inbound)\n")
		b.WriteString(seatbeltNetwork)
		b.WriteString(seatbeltTLSTrust + "\n")
	}

	// 读取不受限时才放开偏好设置服务：它能读到文件规则之外的数据（同 Codex）。
	b.WriteString(seatbeltPreferences)
	section("禁止访问 XPC 服务：它们以沙箱外的身份运行，可以代为读写文件")
	b.WriteString(`(deny mach-lookup (xpc-service-name-prefix ""))` + "\n")

	if len(p.DenyRead) > 0 {
		section("deny_read：禁止读写，优先于前面所有的 allow")
		for i, d := range p.DenyRead {
			d := param(fmt.Sprintf("DENY_READ_%d", i), d)
			fmt.Fprintf(&b, "(deny file-read* file-write* (literal %s) (subpath %s))\n", d, d)
		}
	}

	if len(roots) > 0 {
		section("禁止删除或改名可写根，以及受保护路径在可写根内的上级目录")
		for _, root := range anchors {
			fmt.Fprintf(&b, "(deny file-write-unlink (require-all (vnode-type DIRECTORY) (literal %s)))\n", root)
		}
		for i, a := range protectedAncestors(roots, p.DenyRead) {
			a := param(fmt.Sprintf("PROTECTED_ANCESTOR_%d", i), a)
			fmt.Fprintf(&b, "(deny file-write-unlink (require-all (vnode-type DIRECTORY) (literal %s)))\n", a)
		}
	}
	// 这两个 fcntl 能通过只读的文件描述符修改文件内容，绕过 file-write*。
	// F_MAKECOMPRESSED = 80，F_TRANSFEREXTENTS = 110。
	b.WriteString("(deny system-fcntl (fcntl-command 80 110))\n")
	return b.String(), params
}

// protectedAncestors 返回只读路径和 deny_read 路径在可写根内的上级目录
// （不含可写根本身，它们已经单独处理）。
func protectedAncestors(roots []WritableRoot, denyRead []string) []string {
	var out []string
	for _, w := range roots {
		for _, p := range append(slices.Clone(w.ReadOnly), denyRead...) {
			if !within(w.Root, p) {
				continue
			}
			for a := filepath.Dir(p); a != w.Root && within(w.Root, a); a = filepath.Dir(a) {
				isRoot := slices.ContainsFunc(roots, func(r WritableRoot) bool { return r.Root == a })
				if !isRoot && !slices.Contains(out, a) {
					out = append(out, a)
				}
			}
		}
	}
	return out
}

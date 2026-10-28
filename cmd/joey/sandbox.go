package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/Czyyy-Joey/joey-agent/internal/config"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
)

// exitCode 让命令以指定状态码退出而不打印错误（用于透传被执行命令的退出码）。
type exitCode int

func (c exitCode) Error() string { return fmt.Sprintf("exit status %d", int(c)) }

const sandboxUsage = `Usage:
  joey sandbox exec [flags] -- <command>
  joey sandbox info [flags]

Flags override the [sandbox] section of joey.toml.
`

type sandboxFlags struct {
	configPath, workspace, mode, approval string
	network                               bool
	writable, denyRead                    multiFlag
	profile                               bool
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func (f *sandboxFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.configPath, "config", "", "path to a TOML config file (default: joey.toml if present)")
	fs.StringVar(&f.workspace, "workspace", "", "workspace directory (default: workspace.root)")
	fs.StringVar(&f.mode, "mode", "", "read-only | workspace-write | danger-full-access")
	fs.StringVar(&f.approval, "approval", "", "untrusted | on-request | never")
	fs.BoolVar(&f.network, "network", false, "allow network access")
	fs.Var(&f.writable, "writable", "extra writable directory (repeatable)")
	fs.Var(&f.denyRead, "deny-read", "path commands cannot read (repeatable, added to sandbox.deny_read)")
}

// load 读取配置并应用命令行覆盖，返回配置和沙箱。
func (f *sandboxFlags) load(fs *flag.FlagSet) (config.Config, *sandbox.Sandbox, error) {
	path := f.configPath
	if path == "" {
		if _, err := os.Stat("joey.toml"); err == nil {
			path = "joey.toml"
		}
	}
	cfg, err := config.Load(path, nil)
	if err != nil {
		return cfg, nil, err
	}
	fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "workspace":
			cfg.Workspace.Root = f.workspace
		case "mode":
			cfg.Sandbox.Mode = f.mode
		case "approval":
			cfg.Sandbox.Approval = f.approval
		case "network":
			cfg.Sandbox.Network = f.network
		case "writable":
			cfg.Sandbox.WritableRoots = append(cfg.Sandbox.WritableRoots, f.writable...)
		case "deny-read":
			cfg.Sandbox.DenyRead = append(cfg.Sandbox.DenyRead, f.denyRead...)
		}
	})
	if err := cfg.Validate(); err != nil {
		return cfg, nil, err
	}
	p, err := cfg.SandboxPolicy(cfg.Workspace.Root)
	if err != nil {
		return cfg, nil, err
	}
	sb, err := sandbox.New(p)
	return cfg, sb, err
}

func runSandbox(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, sandboxUsage)
		return exitCode(2)
	}
	switch args[0] {
	case "exec":
		return sandboxExec(args[1:], os.Stdin, os.Stdout, os.Stderr)
	case "info":
		return sandboxInfo(args[1:], os.Stdout)
	}
	fmt.Fprintf(os.Stderr, "unknown sandbox command %q\n\n%s", args[0], sandboxUsage)
	return exitCode(2)
}

func sandboxExec(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("sandbox exec", flag.ExitOnError)
	var f sandboxFlags
	f.register(fs)
	timeout := fs.Duration("timeout", sandbox.DefaultTimeout, "kill the command after this long")
	maxOut := fs.Int("max-output", sandbox.DefaultMaxOutput, "bytes kept per output stream (the middle is dropped)")
	cwd := fs.String("cwd", "", "working directory, relative to the workspace")
	yes := fs.Bool("y", false, "approve without asking when the command needs approval")
	_ = fs.Parse(args)
	command := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if command == "" {
		return errors.New("usage: joey sandbox exec [flags] -- <command>")
	}

	cfg, sb, err := f.load(fs)
	if err != nil {
		return err
	}
	defer sb.Close()

	mode, rules := cfg.SandboxRules()
	a := sandbox.Assess(command, sb.Policy(), mode, rules)
	fmt.Fprintf(stderr, "sandbox: %s seatbelt=%v approval=%s\n", sb.Policy(), sb.Enforced(), mode)
	fmt.Fprintf(stderr, "decision: %s (%s)\n", a.Decision, a.Reason)
	switch a.Decision {
	case sandbox.Forbidden:
		return exitCode(126)
	case sandbox.NeedsApproval:
		if !*yes && !confirm(stdin, stderr) {
			fmt.Fprintln(stderr, "not approved; rerun with -y to approve")
			return exitCode(126)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var mu sync.Mutex
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: command, Cwd: *cwd, Timeout: *timeout, MaxOutput: *maxOut},
		func(s sandbox.Stream, b []byte) {
			mu.Lock()
			defer mu.Unlock()
			if s == sandbox.Stderr {
				stderr.Write(b)
			} else {
				stdout.Write(b)
			}
		})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "── %s\n", res.Summary())
	switch {
	case res.TimedOut:
		return exitCode(124)
	case res.Cancelled:
		return exitCode(130)
	case res.ExitCode != 0:
		return exitCode(res.ExitCode)
	}
	return nil
}

// confirm 在终端上询问；stdin 不是终端时视为未批准。
func confirm(stdin io.Reader, stderr io.Writer) bool {
	if f, ok := stdin.(*os.File); ok {
		if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	fmt.Fprint(stderr, "run it? [y/N] ")
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes"
}

func sandboxInfo(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sandbox info", flag.ExitOnError)
	var f sandboxFlags
	f.register(fs)
	fs.BoolVar(&f.profile, "profile", false, "print the generated Seatbelt profile and parameters")
	_ = fs.Parse(args)
	cfg, sb, err := f.load(fs)
	if err != nil {
		return err
	}
	defer sb.Close()
	p := sb.Policy()
	mode, rules := cfg.SandboxRules()
	fmt.Fprintf(out, "mode:      %s\n", p.Mode)
	fmt.Fprintf(out, "workspace: %s\n", p.Workspace)
	fmt.Fprintf(out, "network:   %v\n", p.Network)
	fmt.Fprintf(out, "approval:  %s (allow=%v deny=%v)\n", mode, rules.Allow, rules.Deny)
	if !sb.Enforced() {
		fmt.Fprintln(out, "WARNING:   danger-full-access: commands run without a sandbox")
		return nil
	}
	fmt.Fprintln(out, "reads:     whole file system, except deny_read:")
	for _, d := range p.DenyRead {
		fmt.Fprintf(out, "  %s\n", d)
	}
	fmt.Fprintln(out, "writes:")
	if len(p.ExecWritableRoots()) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, w := range p.ExecWritableRoots() {
		fmt.Fprintf(out, "  %s", w.Root)
		if len(w.ReadOnly) > 0 {
			fmt.Fprintf(out, "  (read-only: %s)", strings.Join(w.ReadOnly, ", "))
		}
		fmt.Fprintln(out)
	}
	if f.profile {
		profile, params := sb.Profile()
		fmt.Fprintf(out, "\nseatbelt profile:\n%s\nparams:\n  %s\n", profile, strings.Join(params, "\n  "))
	} else {
		fmt.Fprintln(out, "\n(run with -profile to print the generated Seatbelt profile)")
	}
	return nil
}

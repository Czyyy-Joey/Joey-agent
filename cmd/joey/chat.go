package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/session"
)

// chatLoop 为每个提示运行一个轮次并渲染其事件。提供 prompt 参数时运行
// 单个轮次；否则从 in 读取提示。轮次需要审批时，从 in 读取回答；in 已经
// 读完（例如不是终端）时按拒绝处理。输入 /compact 压缩上下文。Ctrl-C
// 中断正在运行的轮次；再次按下 Ctrl-C 退出。
func chatLoop(sess *session.Session, args []string, in io.Reader, out io.Writer) error {
	_, sub, _ := sess.Bus().Subscribe(sess.Bus().LastSeq())
	defer sub.Cancel()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	defer signal.Stop(sigc)

	lines := make(chan string)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()

	runOne := func(prompt string) error {
		submit := sess.Submit
		if prompt == session.CompactCommand {
			submit = func(string) (string, error) { return sess.Compact() }
		}
		turnID, err := submit(prompt)
		if err != nil {
			return err
		}
		interrupted := false
		r := renderer{out: out}
		var asking []protocol.ApprovalData // 等待回答的审批，按到达顺序
		for {
			var answers <-chan string
			if len(asking) > 0 {
				answers = lines
			}
			select {
			case <-sigc:
				if interrupted {
					return fmt.Errorf("aborted")
				}
				interrupted = true
				fmt.Fprintln(out, "\n^C interrupting… (press again to quit)")
				go sess.Interrupt(2 * time.Second)
			case line, ok := <-answers:
				d := parseDecision(line)
				if !ok {
					lines, d = nil, protocol.DecisionDenied
					fmt.Fprintln(out, "(no input; denied)")
				}
				if d == "" {
					fmt.Fprint(out, approvalPrompt)
					continue
				}
				if err := sess.Resolve(asking[0].ApprovalID, d); err != nil {
					fmt.Fprintf(out, "  (%v)\n", err)
				}
				asking = asking[1:]
				if len(asking) > 0 {
					r.showApproval(asking[0])
				}
			case e, ok := <-sub.C:
				if !ok {
					return fmt.Errorf("event stream closed")
				}
				if e.TurnID != turnID {
					continue
				}
				switch d, _ := e.Data.(protocol.ApprovalData); e.Type {
				case protocol.EventApprovalRequested:
					asking = append(asking, d)
					if len(asking) == 1 {
						r.showApproval(d)
					}
					continue
				case protocol.EventApprovalResolved:
					asking = slices.DeleteFunc(asking, func(a protocol.ApprovalData) bool { return a.ApprovalID == d.ApprovalID })
					fmt.Fprintf(out, "  approval: %s\n", d.Decision)
					continue
				}
				if done := r.render(e); done {
					return nil
				}
			}
		}
	}

	if prompt := strings.TrimSpace(strings.Join(args, " ")); prompt != "" {
		return runOne(prompt)
	}
	fmt.Fprint(out, "joey> ")
	for line := range lines {
		switch line = strings.TrimSpace(line); line {
		case "":
		case "/exit", "/quit":
			return nil
		default:
			if err := runOne(line); err != nil {
				return err
			}
		}
		if lines == nil {
			return nil
		}
		fmt.Fprint(out, "joey> ")
	}
	return nil
}

const approvalPrompt = "  allow? [y] once / [a] for this session / [n] deny / [q] abort turn: "

func parseDecision(s string) protocol.ReviewDecision {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "y", "yes":
		return protocol.DecisionApproved
	case "a", "always":
		return protocol.DecisionApprovedForSession
	case "n", "no":
		return protocol.DecisionDenied
	case "q", "abort":
		return protocol.DecisionAbort
	}
	return ""
}

func (r *renderer) showApproval(d protocol.ApprovalData) {
	r.endLine()
	switch d.Kind {
	case protocol.ApprovalExec:
		where := ""
		if d.Unsandboxed {
			where = " (OUTSIDE the sandbox)"
		}
		fmt.Fprintf(r.out, "? approve command%s: %s\n  in %s — %s\n", where, d.Command, d.Cwd, d.Reason)
	default:
		fmt.Fprintf(r.out, "? approve file changes: %s — %s\n", strings.Join(d.Paths, ", "), d.Reason)
	}
	fmt.Fprint(r.out, approvalPrompt)
}

type renderer struct {
	out       io.Writer
	inMessage bool
}

func (r *renderer) endLine() {
	if r.inMessage {
		fmt.Fprintln(r.out)
		r.inMessage = false
	}
}

// render 打印一个事件并报告轮次是否结束。
func (r *renderer) render(e protocol.Event) bool {
	switch e.Type {
	case protocol.EventItemDelta:
		d, ok := e.Data.(protocol.ItemData)
		switch {
		case ok && d.Kind == protocol.ItemAgentMessage:
			fmt.Fprint(r.out, d.Text)
			r.inMessage = true
		case ok && d.Kind == protocol.ItemToolCall:
			fmt.Fprint(r.out, indent(d.Output))
		}
	case protocol.EventItemStarted:
		d, ok := e.Data.(protocol.ItemData)
		switch {
		case ok && d.Kind == protocol.ItemToolCall:
			r.endLine()
			fmt.Fprintf(r.out, "→ %s(%s)\n", d.Name, d.Arguments)
		case ok && d.Kind == protocol.ItemCompaction:
			r.endLine()
			fmt.Fprintf(r.out, "→ compacting the context (%s)…\n", d.Compaction.Reason)
		}
	case protocol.EventItemCompleted:
		d, ok := e.Data.(protocol.ItemData)
		if !ok {
			break
		}
		switch d.Kind {
		case protocol.ItemToolCall:
			mark := "✓"
			if d.IsError {
				mark = "✗"
			}
			fmt.Fprintf(r.out, "  %s %s %dms %s\n", mark, d.Status, d.DurationMS, firstLine(d.Output))
		case protocol.ItemAgentMessage:
			r.endLine()
		case protocol.ItemCompaction:
			if c := d.Compaction; c != nil && d.Status == protocol.ItemCompleted {
				fmt.Fprintf(r.out, "  ✓ context compacted: %d items / ~%d tokens → %d items / ~%d tokens\n",
					c.ItemsBefore, c.TokensBefore, c.ItemsAfter, c.TokensAfter)
			} else {
				fmt.Fprintf(r.out, "  ✗ context compaction %s: %s\n", d.Status, firstLine(d.Text))
			}
		}
	case protocol.EventWarning, protocol.EventError:
		if d, ok := e.Data.(protocol.MessageData); ok {
			r.endLine()
			fmt.Fprintf(r.out, "[%s] %s\n", e.Type, d.Message)
		}
	case protocol.EventTurnCompleted:
		r.endLine()
		if d, ok := e.Data.(protocol.TurnData); ok {
			usage := ""
			if d.Usage != nil && d.Usage.TotalTokens > 0 {
				usage = fmt.Sprintf(" · %d tokens", d.Usage.TotalTokens)
			}
			fmt.Fprintf(r.out, "── %s%s\n", d.Status, usage)
		}
		return true
	}
	return false
}

// indent 给实时输出的每一行加两格缩进，和工具调用的结果行对齐。
func indent(s string) string {
	return "  │ " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  │ ") + "\n"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}

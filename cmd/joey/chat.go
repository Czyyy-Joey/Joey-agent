package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/session"
)

// chatLoop 为每个提示运行一个轮次并渲染其事件。提供 prompt 参数时运行
// 单个轮次；否则从 in 读取提示。Ctrl-C 中断正在运行的轮次；再次按下
// Ctrl-C 退出。
func chatLoop(sess *session.Session, args []string, in io.Reader, out io.Writer) error {
	_, sub, _ := sess.Bus().Subscribe(sess.Bus().LastSeq())
	defer sub.Cancel()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt)
	defer signal.Stop(sigc)

	runOne := func(prompt string) error {
		turnID, err := sess.Submit(prompt)
		if err != nil {
			return err
		}
		interrupted := false
		r := renderer{out: out}
		for {
			select {
			case <-sigc:
				if interrupted {
					return fmt.Errorf("aborted")
				}
				interrupted = true
				fmt.Fprintln(out, "\n^C interrupting… (press again to quit)")
				go sess.Interrupt(2 * time.Second)
			case e, ok := <-sub.C:
				if !ok {
					return fmt.Errorf("event stream closed")
				}
				if e.TurnID != turnID {
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
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	fmt.Fprint(out, "joey> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch line {
		case "":
		case "/exit", "/quit":
			return nil
		default:
			if err := runOne(line); err != nil {
				return err
			}
		}
		fmt.Fprint(out, "joey> ")
	}
	return sc.Err()
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
		if d, ok := e.Data.(protocol.ItemData); ok && d.Kind == protocol.ItemAgentMessage {
			fmt.Fprint(r.out, d.Text)
			r.inMessage = true
		}
	case protocol.EventItemStarted:
		if d, ok := e.Data.(protocol.ItemData); ok && d.Kind == protocol.ItemToolCall {
			r.endLine()
			fmt.Fprintf(r.out, "→ %s(%s)\n", d.Name, d.Arguments)
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

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 100 {
		s = s[:100] + "…"
	}
	return s
}

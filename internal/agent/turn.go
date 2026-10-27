package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// RunTurn 将 input 记录为用户消息，并循环直到模型在不调用工具的情况下
// 回复、达到步数上限、ctx 被取消，或发生不可恢复的错误。history 不会被
// 修改。
func RunTurn(ctx context.Context, env TurnEnv, history []protocol.Item, input string) TurnResult {
	r := &turnRunner{
		env:     env,
		history: append(make([]protocol.Item, 0, len(history)+8), history...),
		start:   len(history),
		specs:   toolSpecs(env.Router),
	}
	user := protocol.Item{ID: protocol.NewID("item"), TurnID: env.TurnID, Kind: protocol.ItemUserMessage, Text: input}
	if err := r.record(user); err != nil {
		r.emit(protocol.EventError, "", protocol.MessageData{Message: err.Error()})
		return r.result(protocol.TurnFailed, err)
	}
	r.emit(protocol.EventItemCompleted, user.ID,
		protocol.ItemData{Kind: protocol.ItemUserMessage, Status: protocol.ItemCompleted, Text: input})

	maxSteps := env.MaxSteps
	if maxSteps <= 0 {
		maxSteps = DefaultMaxSteps
	}
	for range maxSteps {
		if ctx.Err() != nil {
			return r.result(protocol.TurnInterrupted, nil)
		}
		s, err := r.sample(ctx)
		if s.usage != nil {
			r.usage.Add(*s.usage)
			r.emit(protocol.EventTokenUsage, "", protocol.UsageData{Last: *s.usage, Turn: r.usage, ContextWindow: env.ContextWindow})
		}
		if err != nil {
			if ctx.Err() != nil {
				return r.result(protocol.TurnInterrupted, nil)
			}
			r.emit(protocol.EventError, "", protocol.MessageData{Message: err.Error()})
			return r.result(protocol.TurnFailed, err)
		}
		if s.message != nil {
			r.final = s.message.Text
		}
		if s.finishReason == "length" {
			r.emit(protocol.EventWarning, "", protocol.MessageData{Message: "model output hit the token limit and may be incomplete"})
		}
		if len(s.calls) == 0 {
			return r.result(protocol.TurnCompleted, nil)
		}
		if err := r.runTools(ctx, s.calls); err != nil {
			r.emit(protocol.EventError, "", protocol.MessageData{Message: err.Error()})
			return r.result(protocol.TurnFailed, err)
		}
		if ctx.Err() != nil {
			return r.result(protocol.TurnInterrupted, nil)
		}
	}
	err := fmt.Errorf("stopped after %d model requests without a final answer", maxSteps)
	r.emit(protocol.EventWarning, "", protocol.MessageData{Message: err.Error()})
	return r.result(protocol.TurnMaxSteps, err)
}

type turnRunner struct {
	env     TurnEnv
	history []protocol.Item
	// start 是该轮次第一个项在 history 中的索引。
	start int
	specs []llm.ToolSpec
	usage protocol.Usage
	final string
}

// record 先持久化 it（若配置了 Record），成功后才把它加入历史。
func (r *turnRunner) record(it protocol.Item) error {
	if r.env.Record != nil {
		if err := r.env.Record(it); err != nil {
			return fmt.Errorf("save history: %w", err)
		}
	}
	r.history = append(r.history, it)
	return nil
}

func (r *turnRunner) result(status protocol.TurnStatus, err error) TurnResult {
	items := append([]protocol.Item(nil), r.history[r.start:]...)
	return TurnResult{Status: status, Items: items, FinalMessage: r.final, Usage: r.usage, Err: err}
}

func (r *turnRunner) emit(t protocol.EventType, itemID string, data any) {
	if r.env.Emit == nil {
		return
	}
	r.env.Emit(protocol.Event{Type: t, TurnID: r.env.TurnID, ItemID: itemID, Data: data})
}

type sampleResult struct {
	message      *protocol.Item
	calls        []protocol.ToolCall
	usage        *protocol.Usage
	finishReason string
}

// streamingItem 在其 delta 到达期间跟踪一个文本项。
type streamingItem struct {
	id   string
	kind protocol.ItemKind
	text strings.Builder
}

func (r *turnRunner) sample(ctx context.Context) (sampleResult, error) {
	var res sampleResult
	var msg, reasoning *streamingItem
	appendDelta := func(si **streamingItem, kind protocol.ItemKind, delta string) {
		if *si == nil {
			*si = &streamingItem{id: protocol.NewID("item"), kind: kind}
			r.emit(protocol.EventItemStarted, (*si).id, protocol.ItemData{Kind: kind, Status: protocol.ItemInProgress})
		}
		(*si).text.WriteString(delta)
		r.emit(protocol.EventItemDelta, (*si).id, protocol.ItemData{Kind: kind, Text: delta})
	}
	closeItem := func(si *streamingItem, status protocol.ItemStatus) {
		if si != nil {
			r.emit(protocol.EventItemCompleted, si.id, protocol.ItemData{Kind: si.kind, Status: status, Text: si.text.String()})
		}
	}

	req := llm.Request{Instructions: r.env.Instructions, Items: r.history, Tools: r.specs}
	var streamErr error
	for ev, err := range r.env.Provider.Stream(ctx, req) {
		if err != nil {
			streamErr = err
			break
		}
		switch ev.Kind {
		case llm.EventTextDelta:
			if ev.Delta != "" {
				appendDelta(&msg, protocol.ItemAgentMessage, ev.Delta)
			}
		case llm.EventReasoningDelta:
			if ev.Delta != "" {
				appendDelta(&reasoning, protocol.ItemReasoning, ev.Delta)
			}
		case llm.EventToolCall:
			if ev.Call != nil {
				res.calls = append(res.calls, *ev.Call)
			}
		case llm.EventCompleted:
			res.usage = ev.Usage
			res.finishReason = ev.FinishReason
		}
	}
	if streamErr == nil && ctx.Err() != nil {
		streamErr = ctx.Err()
	}

	if streamErr != nil {
		// 部分回答会展示给用户，但绝不会进入模型历史。
		status := protocol.ItemFailed
		if ctx.Err() != nil {
			status = protocol.ItemInterrupted
		}
		closeItem(reasoning, status)
		closeItem(msg, status)
		res.calls = nil
		return res, streamErr
	}
	closeItem(reasoning, protocol.ItemCompleted)
	if msg != nil {
		item := protocol.Item{ID: msg.id, TurnID: r.env.TurnID, Kind: protocol.ItemAgentMessage, Text: msg.text.String()}
		if err := r.record(item); err != nil {
			closeItem(msg, protocol.ItemFailed)
			res.calls = nil
			return res, err
		}
		closeItem(msg, protocol.ItemCompleted)
		res.message = &item
	}
	return res, nil
}

// runTools 先记录每个调用，然后按顺序执行它们。因轮次被取消而无法
// 运行的调用仍会得到中止输出。保存失败时立即返回；缺少输出的调用由
// 调用方用 history.Normalize 补齐。
func (r *turnRunner) runTools(ctx context.Context, calls []protocol.ToolCall) error {
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = protocol.NewID("item")
		call := c
		if err := r.record(protocol.Item{ID: ids[i], TurnID: r.env.TurnID, Kind: protocol.ItemToolCall, Call: &call}); err != nil {
			return err
		}
	}
	var fatal error
	for i, c := range calls {
		r.emit(protocol.EventItemStarted, ids[i], protocol.ItemData{
			Kind: protocol.ItemToolCall, Status: protocol.ItemInProgress,
			CallID: c.CallID, Name: c.Name, Arguments: c.Arguments,
		})
		var oc toolOutcome
		if fatal != nil {
			oc = toolOutcome{content: "skipped: an earlier tool call failed fatally", status: protocol.ItemAborted}
		} else {
			o := r.env.Router.Execute(ctx, r.env.TurnID, c)
			oc = toolOutcome{content: o.Output.Content, status: o.Status, duration: o.Duration}
			fatal = o.Fatal
		}
		output := protocol.ToolOutput{CallID: c.CallID, Content: oc.content, IsError: oc.status != protocol.ItemCompleted}
		if err := r.record(protocol.Item{ID: protocol.NewID("item"), TurnID: r.env.TurnID, Kind: protocol.ItemToolOutput, Output: &output}); err != nil {
			r.emit(protocol.EventItemCompleted, ids[i], protocol.ItemData{
				Kind: protocol.ItemToolCall, Status: protocol.ItemFailed,
				CallID: c.CallID, Name: c.Name, Arguments: c.Arguments, Output: err.Error(), IsError: true,
			})
			return err
		}
		r.emit(protocol.EventItemCompleted, ids[i], protocol.ItemData{
			Kind: protocol.ItemToolCall, Status: oc.status,
			CallID: c.CallID, Name: c.Name, Arguments: c.Arguments,
			Output: oc.content, IsError: output.IsError, DurationMS: oc.duration.Milliseconds(),
		})
	}
	return fatal
}

type toolOutcome struct {
	content  string
	status   protocol.ItemStatus
	duration time.Duration
}

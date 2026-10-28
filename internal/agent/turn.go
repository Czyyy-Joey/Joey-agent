package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// RunTurn 将 input 记录为用户消息，并循环直到模型在不调用工具的情况下
// 回复、达到步数上限、ctx 被取消，或发生不可恢复的错误。history 是模型
// 历史（history.Replay 的结果），不会被修改。
//
// 上下文在三个时机自动压缩：记录 input 之前（历史加 input 超过阈值）、
// 工具执行之后（工具输出让上下文超过阈值）、模型报告上下文超限之后（压缩
// 后重试一次）。
func RunTurn(ctx context.Context, env TurnEnv, hist []protocol.Item, input string) TurnResult {
	r := newRunner(env, hist)
	user := protocol.Item{ID: protocol.NewID("item"), TurnID: env.TurnID, Kind: protocol.ItemUserMessage, Text: input}
	if n, _ := r.tokens(); len(r.history) > 0 && r.limit() > 0 && n+history.EstimateItem(user) >= r.limit() {
		r.autoCompact(ctx, protocol.CompactPreTurn)
	}
	// 即使压缩被中断，输入也要记录下来。
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
		if err != nil && ctx.Err() == nil && llm.IsContextOverflow(err) && r.canCompact() {
			r.emit(protocol.EventWarning, "", protocol.MessageData{Message: "the model reported that the context window was exceeded; compacting and retrying"})
			if cerr := r.compactDue(ctx, protocol.CompactOverflow); cerr == nil {
				s, err = r.sample(ctx)
			}
		}
		if s.usage != nil {
			r.usage.Add(*s.usage)
			// 接下来记录的工具调用是这次响应的输出，已经计入 usage。
			r.size = history.ContextSize{Reported: int(s.usage.InputTokens + s.usage.OutputTokens), Covered: len(r.history) + len(s.calls)}
			r.emitUsage(*s.usage)
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
			r.postTurn(ctx)
			if ctx.Err() != nil {
				return r.result(protocol.TurnInterrupted, nil)
			}
			return r.result(protocol.TurnCompleted, nil)
		}
		if err := r.runTools(ctx, s.calls); err != nil {
			r.emit(protocol.EventError, "", protocol.MessageData{Message: err.Error()})
			return r.result(protocol.TurnFailed, err)
		}
		if ctx.Err() != nil {
			return r.result(protocol.TurnInterrupted, nil)
		}
		r.emitUsage(protocol.Usage{})
		if n, _ := r.tokens(); r.limit() > 0 && n >= r.limit() && r.canCompact() {
			r.autoCompact(ctx, protocol.CompactMidTurn)
		} else {
			r.maybeBackground()
		}
	}
	err := fmt.Errorf("stopped after %d model requests without a final answer", maxSteps)
	r.emit(protocol.EventWarning, "", protocol.MessageData{Message: err.Error()})
	return r.result(protocol.TurnMaxSteps, err)
}

// Compact 立即压缩 history（用户执行 /compact），不记录用户消息也不采样。
func Compact(ctx context.Context, env TurnEnv, hist []protocol.Item) TurnResult {
	r := newRunner(env, hist)
	if len(r.history) == 0 {
		r.emit(protocol.EventWarning, "", protocol.MessageData{Message: "nothing to compact: the history is empty"})
		return r.result(protocol.TurnCompleted, nil)
	}
	if err := r.compact(ctx, protocol.CompactManual); err != nil {
		if ctx.Err() != nil {
			return r.result(protocol.TurnInterrupted, nil)
		}
		r.emit(protocol.EventError, "", protocol.MessageData{Message: err.Error()})
		return r.result(protocol.TurnFailed, err)
	}
	return r.result(protocol.TurnCompleted, nil)
}

type turnRunner struct {
	env TurnEnv
	// history 是模型历史；log 是本轮新记录的条目（含压缩检查点），按顺序。
	history []protocol.Item
	log     []protocol.Item
	specs   []llm.ToolSpec
	usage   protocol.Usage
	final   string
	// size 是上游报告的上下文大小，base 是系统提示词和工具定义的估算值。
	size history.ContextSize
	base int
	// compactedAt 是上次压缩后模型历史的长度；历史没有增长时再压缩没有意义。
	compactedAt int
}

func newRunner(env TurnEnv, hist []protocol.Item) *turnRunner {
	r := &turnRunner{
		env:         env,
		history:     append(make([]protocol.Item, 0, len(hist)+8), hist...),
		specs:       toolSpecs(env.Router),
		size:        env.Size,
		compactedAt: -1,
	}
	r.base = history.EstimateBase(env.Instructions, r.specs)
	return r
}

func (r *turnRunner) limit() int { return r.env.AutoCompactLimit }

func (r *turnRunner) tokens() (int, bool) { return r.size.Tokens(r.history, r.base) }

func (r *turnRunner) canCompact() bool { return len(r.history) > r.compactedAt }

func (r *turnRunner) emitUsage(last protocol.Usage) {
	n, est := r.tokens()
	if r.size.Reported > 0 && r.size.Covered > len(r.history) {
		// 刚采样完、工具调用还没记录：上下文就是上游报告的大小。
		n, est = r.size.Reported, false
	}
	r.emit(protocol.EventTokenUsage, "", protocol.UsageData{
		Last: last, Turn: r.usage, ContextWindow: r.env.ContextWindow,
		ContextTokens: n, Estimated: est, AutoCompactLimit: r.limit(),
	})
}

// record 先持久化 it（若配置了 Record），成功后才把它加入历史。压缩检查点
// 用它的替换历史取代模型历史。
func (r *turnRunner) record(it protocol.Item) error {
	if r.env.Record != nil {
		if err := r.env.Record(it); err != nil {
			return fmt.Errorf("save history: %w", err)
		}
	}
	r.log = append(r.log, it)
	if it.Kind == protocol.ItemCompaction {
		r.history = slices.Clone(it.Compaction.Replacement)
		return nil
	}
	r.history = append(r.history, it)
	return nil
}

func (r *turnRunner) result(status protocol.TurnStatus, err error) TurnResult {
	return TurnResult{Status: status, Items: slices.Clone(r.log), FinalMessage: r.final, Usage: r.usage, Err: err, Context: r.size}
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

// runTools 先记录每个调用，然后交给 Router.RunAll 执行：连续的并行安全
// 调用同时执行，其余按顺序独占执行。每个调用完成后立即持久化输出并发出
// item.completed。因轮次被取消而无法运行的调用仍会得到中止输出。保存失败
// 时返回错误，尚未执行的调用被跳过；缺少输出的调用由调用方用
// history.Normalize 补齐。
func (r *turnRunner) runTools(ctx context.Context, calls []protocol.ToolCall) error {
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = protocol.NewID("item")
		call := c
		if err := r.record(protocol.Item{ID: ids[i], TurnID: r.env.TurnID, Kind: protocol.ItemToolCall, Call: &call}); err != nil {
			return err
		}
	}
	runs := make([]tools.Call, len(calls))
	for i, c := range calls {
		id := ids[i]
		runs[i] = tools.Call{
			ToolCall: c, TurnID: r.env.TurnID, ItemID: id, Approver: r.env.Approver,
			Output: func(chunk string) {
				r.emit(protocol.EventItemDelta, id, protocol.ItemData{Kind: protocol.ItemToolCall, CallID: c.CallID, Output: chunk})
			},
		}
	}
	started := func(i int) {
		c := calls[i]
		r.emit(protocol.EventItemStarted, ids[i], protocol.ItemData{
			Kind: protocol.ItemToolCall, Status: protocol.ItemInProgress,
			CallID: c.CallID, Name: c.Name, Arguments: c.Arguments,
		})
	}
	done := func(i int, o tools.Outcome) error {
		c := calls[i]
		output := o.Output
		output.Content, _ = history.TruncateMiddle(output.Content, history.ToolOutputLimit(r.env.ToolOutputTokens, r.limit()))
		if err := r.record(protocol.Item{ID: protocol.NewID("item"), TurnID: r.env.TurnID, Kind: protocol.ItemToolOutput, Output: &output}); err != nil {
			r.emit(protocol.EventItemCompleted, ids[i], protocol.ItemData{
				Kind: protocol.ItemToolCall, Status: protocol.ItemFailed,
				CallID: c.CallID, Name: c.Name, Arguments: c.Arguments, Output: err.Error(), IsError: true,
			})
			return err
		}
		r.emit(protocol.EventItemCompleted, ids[i], protocol.ItemData{
			Kind: protocol.ItemToolCall, Status: o.Status,
			CallID: c.CallID, Name: c.Name, Arguments: c.Arguments,
			Output: output.Content, IsError: output.IsError, DurationMS: o.Duration.Milliseconds(), Diff: output.Diff,
		})
		return nil
	}
	return r.env.Router.RunAll(ctx, runs, started, done)
}

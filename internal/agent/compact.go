package agent

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// errEmptySummary 表示模型没有返回摘要。
var errEmptySummary = errors.New("the model returned an empty summary")

// autoCompact 自动压缩：先采用后台准备好（或正在准备）的摘要，没有可用的
// 再发起前台压缩。失败时只发出警告，轮次继续（真的超限时由超限后的重试
// 兜底）。
func (r *turnRunner) autoCompact(ctx context.Context, reason protocol.CompactionReason) {
	if err := r.compactDue(ctx, reason); err != nil && ctx.Err() == nil {
		r.emit(protocol.EventWarning, "", protocol.MessageData{Message: "context compaction failed: " + err.Error()})
	}
}

// compactDue 在必须压缩时调用：优先采用后台结果（进行中的就等它），没有
// 可用结果再前台压缩。采用的摘要后面接着快照之后新增的条目，可能仍然超过
// 阈值，这时再前台压缩一次，与 Codex 相同。
func (r *turnRunner) compactDue(ctx context.Context, reason protocol.CompactionReason) error {
	if r.adopt(ctx, reason, true) {
		if n, _ := r.tokens(); r.limit() <= 0 || n < r.limit() {
			return nil
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.compact(ctx, reason)
}

// postTurn 在模型给出最终回答后执行，对应 Codex run_turn 末尾的 turn-end
// compaction：
//   - 上下文达到 PostTurnCompactPercent 的水位时压缩（优先用后台结果），
//     让下一轮从较小的上下文开始，而不是等用户发下一句话时再压缩；
//   - 没达到水位但后台摘要已经完成时，也直接采用它；
//   - 都不满足时，按需发起后台压缩。
//
// PostTurnCompactPercent 为 0 时只做最后一步。失败只发出警告，回答照常保留。
func (r *turnRunner) postTurn(ctx context.Context) {
	if ctx.Err() != nil || r.limit() <= 0 || !r.canCompact() {
		return
	}
	if pct := r.env.PostTurnCompactPercent; pct > 0 {
		if n, _ := r.tokens(); n*100 >= r.limit()*pct {
			r.autoCompact(ctx, protocol.CompactPostTurn)
			return
		}
		if r.adopt(ctx, protocol.CompactPostTurn, false) {
			return
		}
	}
	r.maybeBackground()
}

// maybeBackground 在上下文越过后台水位（BackgroundCompactPercent）、但还没
// 到自动压缩阈值时，用当前模型历史发起后台压缩。只在模型历史完整配对的
// 时刻调用（工具执行完、或模型给出回答之后），快照因此可以直接截断。
func (r *turnRunner) maybeBackground() {
	b, pct := r.env.Background, r.env.BackgroundCompactPercent
	if b == nil || pct <= 0 || r.limit() <= 0 {
		return
	}
	if n, _ := r.tokens(); n*100 < r.limit()*pct || n >= r.limit() {
		return
	}
	b.start(r.env.Instructions, r.history)
}

// compact 让模型为当前历史写一份交接摘要，然后用"保留的用户消息 + 摘要"
// 取代模型历史，并把检查点（kind=compaction）持久化。对应 Codex 的
// run_compact_task_inner_impl（core/src/compact.rs）。
func (r *turnRunner) compact(ctx context.Context, reason protocol.CompactionReason) error {
	id := protocol.NewID("item")
	before, _ := r.tokens()
	info := protocol.Compaction{Reason: reason, ItemsBefore: len(r.history), TokensBefore: before}
	r.emit(protocol.EventItemStarted, id, protocol.ItemData{Kind: protocol.ItemCompaction, Status: protocol.ItemInProgress, Compaction: &protocol.Compaction{Reason: reason}})
	fail := func(err error) error {
		status := protocol.ItemFailed
		if ctx.Err() != nil {
			status = protocol.ItemInterrupted
		}
		r.emit(protocol.EventItemCompleted, id, protocol.ItemData{Kind: protocol.ItemCompaction, Status: status, Compaction: &info, Text: err.Error()})
		return err
	}

	summary, spent, err := summarize(ctx, r.env.Provider, r.env.Instructions, r.history, func(delta string) {
		r.emit(protocol.EventItemDelta, id, protocol.ItemData{Kind: protocol.ItemCompaction, Text: delta})
	})
	r.usage.Add(spent)
	if err != nil {
		return fail(err)
	}
	replacement := history.BuildCompacted(r.history, summary, history.UserMessageBudget(r.limit()), r.env.TurnID)
	if err := r.install(id, summary, replacement, info, spent); err != nil {
		return fail(err)
	}
	return nil
}

// adopt 采用后台压缩的结果。wait 为 true 时，如果当前这一代历史的后台请求
// 还在进行，就等它完成（对应 Codex 的 wait_for_pending），而不是另发一次；
// 否则只采用已经完成的结果（对应 take_ready）。没有可用结果时返回 false。
func (r *turnRunner) adopt(ctx context.Context, reason protocol.CompactionReason, wait bool) bool {
	b := r.env.Background
	if b == nil {
		return false
	}
	c := b.take(ctx, r.history, wait)
	if c == nil {
		return false
	}
	before, _ := r.tokens()
	info := protocol.Compaction{Reason: reason, Background: true, ItemsBefore: len(r.history), TokensBefore: before}
	// 摘要只覆盖快照；快照之后新增的条目原样接在后面。
	replacement := history.BuildCompacted(c.prefix, c.summary, history.UserMessageBudget(r.limit()), r.env.TurnID)
	replacement = append(replacement, slices.Clone(r.history[len(c.prefix):])...)
	replacement, _ = history.Normalize(replacement, history.MissingMessage)

	id := protocol.NewID("item")
	r.usage.Add(c.usage)
	r.emit(protocol.EventItemStarted, id, protocol.ItemData{Kind: protocol.ItemCompaction, Status: protocol.ItemInProgress, Compaction: &protocol.Compaction{Reason: reason, Background: true}})
	if err := r.install(id, c.summary, replacement, info, c.usage); err != nil {
		r.emit(protocol.EventItemCompleted, id, protocol.ItemData{Kind: protocol.ItemCompaction, Status: protocol.ItemFailed, Compaction: &info, Text: err.Error()})
		return false
	}
	return true
}

// install 持久化压缩检查点并用 replacement 取代模型历史。spent 是生成摘要
// 的用量，随压缩结束后的 token.usage 作为 last 发出，客户端累计会话用量时
// 才不会漏掉它。
func (r *turnRunner) install(id, summary string, replacement []protocol.Item, info protocol.Compaction, spent protocol.Usage) error {
	info.ItemsAfter = len(replacement)
	info.TokensAfter = r.base + history.EstimateItems(replacement)
	stored := info
	stored.Replacement = replacement
	if err := r.record(protocol.Item{ID: id, TurnID: r.env.TurnID, Kind: protocol.ItemCompaction, Text: summary, Compaction: &stored}); err != nil {
		return err
	}
	r.size = history.ContextSize{}
	r.compactedAt = len(r.history)
	if r.env.Background != nil {
		// 历史被替换，进入新的一代：之前的后台结果作废，可以重新尝试。
		r.env.Background.advance()
	}
	r.emit(protocol.EventItemCompleted, id, protocol.ItemData{Kind: protocol.ItemCompaction, Status: protocol.ItemCompleted, Text: summary, Compaction: &info})
	r.emitUsage(spent)
	return nil
}

// summarize 为 items 生成交接摘要：发出不带工具的请求，末尾附上压缩提示词，
// 返回模型的文本回答和所有请求的用量。请求本身超出上下文窗口时，逐条删掉
// 最旧的条目再试（对应 Codex 的 remove_first_item）。onDelta 接收流式片段，
// 可以为 nil。
func summarize(ctx context.Context, p llm.Provider, instructions string, items []protocol.Item, onDelta func(string)) (string, protocol.Usage, error) {
	req := history.CompactRequest(items)
	var spent protocol.Usage
	for {
		text, usage, err := summarizeOnce(ctx, p, instructions, req, onDelta)
		spent.Add(usage)
		if err == nil {
			return text, spent, nil
		}
		if ctx.Err() == nil && llm.IsContextOverflow(err) && len(req) > 2 {
			last := len(req) - 1
			req = append(history.DropOldest(req[:last]), req[last])
			continue
		}
		return "", spent, err
	}
}

func summarizeOnce(ctx context.Context, p llm.Provider, instructions string, items []protocol.Item, onDelta func(string)) (string, protocol.Usage, error) {
	var text strings.Builder
	var usage protocol.Usage
	for ev, err := range p.Stream(ctx, llm.Request{Instructions: instructions, Items: items}) {
		if err != nil {
			return "", usage, err
		}
		switch ev.Kind {
		case llm.EventTextDelta:
			text.WriteString(ev.Delta)
			if onDelta != nil {
				onDelta(ev.Delta)
			}
		case llm.EventCompleted:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		}
	}
	if ctx.Err() != nil {
		return "", usage, ctx.Err()
	}
	s := strings.TrimSpace(text.String())
	if s == "" {
		return "", usage, errEmptySummary
	}
	return s, usage, nil
}

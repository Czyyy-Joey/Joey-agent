package history

import (
	_ "embed"
	"slices"
	"strings"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// 压缩提示词和摘要前缀取自 Codex（Apache-2.0）：
// codex-rs/prompts/templates/compact/prompt.md、summary_prefix.md。
// compact.md 末尾加了一句"用用户的语言写摘要"。
var (
	//go:embed prompts/compact.md
	CompactPrompt string
	//go:embed prompts/summary_prefix.md
	SummaryPrefix string
)

// emptySummary 在模型没有给出摘要时使用，与 Codex 相同。
const emptySummary = "(no summary available)"

// IsSummary 报告 text 是否是压缩生成的摘要消息。
func IsSummary(text string) bool {
	return strings.HasPrefix(text, SummaryPrefix+"\n")
}

// CompactRequest 返回发给模型、用来生成摘要的条目：当前历史加上一条压缩
// 提示词。
func CompactRequest(items []protocol.Item) []protocol.Item {
	out := slices.Clip(slices.Clone(items))
	return append(out, protocol.Item{ID: protocol.NewID("item"), Kind: protocol.ItemUserMessage, Text: CompactPrompt})
}

// DropOldest 删掉最旧的一条，以及因此失去配对的工具输出。压缩请求本身
// 超出上下文窗口时用它逐条缩短请求，对应 Codex 的 remove_first_item。
func DropOldest(items []protocol.Item) []protocol.Item {
	if len(items) == 0 {
		return items
	}
	out, _ := Normalize(items[1:], MissingMessage)
	return out
}

// BuildCompacted 返回压缩后的模型历史：items 中真实的用户消息（不含以前
// 的摘要和中断标记），从最新的往前选，总量不超过 budget 个 token，超出
// 预算的那一条截断后保留并停止；最后是一条带 SummaryPrefix 的摘要。
// 对应 Codex 的 build_compacted_history。
func BuildCompacted(items []protocol.Item, summary string, budget int, turnID string) []protocol.Item {
	var selected []protocol.Item
	remaining := budget
	for i := len(items) - 1; i >= 0 && remaining > 0; i-- {
		it := items[i]
		if it.Kind != protocol.ItemUserMessage || IsSummary(it.Text) {
			continue
		}
		tokens := ApproxTokens(it.Text)
		if tokens > remaining {
			it.Text, _ = TruncateMiddle(it.Text, remaining)
			selected = append(selected, it)
			break
		}
		selected = append(selected, it)
		remaining -= tokens
	}
	slices.Reverse(selected)
	if strings.TrimSpace(summary) == "" {
		summary = emptySummary
	}
	return append(selected, protocol.Item{
		ID:     protocol.NewID("item"),
		TurnID: turnID,
		Kind:   protocol.ItemUserMessage,
		Text:   SummaryPrefix + "\n" + summary,
	})
}

// Replay 从完整的会话历史得到模型历史：从最后一个压缩检查点的替换历史
// 开始，接上检查点之后的条目。没有检查点时就是 items 本身。会话在内存中
// 和从数据库恢复时都用它，所以两者看到的模型历史一致。
func Replay(items []protocol.Item) []protocol.Item {
	for i := len(items) - 1; i >= 0; i-- {
		if it := items[i]; it.Kind == protocol.ItemCompaction && it.Compaction != nil {
			out := make([]protocol.Item, 0, len(it.Compaction.Replacement)+len(items)-i-1)
			out = append(out, it.Compaction.Replacement...)
			return append(out, items[i+1:]...)
		}
	}
	return items
}

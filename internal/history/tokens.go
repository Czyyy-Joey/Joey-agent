package history

import (
	"encoding/json"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

const (
	// BytesPerToken 是估算 token 数时使用的比例，与 Codex 的
	// approx_token_count 相同。不同 tokenizer 的实际值不同，所以阈值要留余量。
	BytesPerToken = 4
	// PromptOverhead 是每个请求在可见内容之外的固定开销。实测 gpt-6.1-sol
	// 经 OneAPI 时，"pong" 这样的请求 prompt_tokens 就有 390。
	PromptOverhead = 400
	// itemOverhead 是每条消息的角色、分隔符等格式开销。
	itemOverhead = 4

	// DefaultToolOutputTokens 是单个工具输出写入历史时保留的 token 数上限。
	DefaultToolOutputTokens = 10_000
	// CompactUserMessageMaxTokens 是压缩后保留的用户消息的 token 预算，
	// 对应 Codex 的 COMPACT_USER_MESSAGE_MAX_TOKENS。
	CompactUserMessageMaxTokens = 20_000
)

// ApproxTokens 按字节数估算 s 的 token 数（向上取整）。
func ApproxTokens(s string) int {
	return (len(s) + BytesPerToken - 1) / BytesPerToken
}

// EstimateItem 估算一条条目发给模型时占用的 token 数。推理内容和压缩
// 检查点不发给模型，算作 0。
func EstimateItem(it protocol.Item) int {
	switch it.Kind {
	case protocol.ItemUserMessage, protocol.ItemAgentMessage, protocol.ItemTurnAborted:
		return itemOverhead + ApproxTokens(it.Text)
	case protocol.ItemToolCall:
		if it.Call == nil {
			return 0
		}
		return itemOverhead + ApproxTokens(it.Call.CallID) + ApproxTokens(it.Call.Name) + ApproxTokens(it.Call.Arguments)
	case protocol.ItemToolOutput:
		if it.Output == nil {
			return 0
		}
		return itemOverhead + ApproxTokens(it.Output.CallID) + ApproxTokens(it.Output.Content)
	}
	return 0
}

// EstimateItems 是 items 中各条目估算值之和。
func EstimateItems(items []protocol.Item) int {
	n := 0
	for _, it := range items {
		n += EstimateItem(it)
	}
	return n
}

// EstimateBase 估算每个请求都有的部分：系统提示词、工具定义和固定开销。
// tools 是要发送的工具定义，任何可以序列化为 JSON 的值都行。
func EstimateBase(instructions string, tools any) int {
	n := PromptOverhead + ApproxTokens(instructions)
	if tools != nil {
		if b, err := json.Marshal(tools); err == nil {
			n += ApproxTokens(string(b))
		}
	}
	return n
}

// ContextSize 记录最近一次上游报告的上下文大小。Reported 是那次响应的
// input+output token 数，覆盖模型历史的前 Covered 条；之后追加的条目用
// 估算值补上。Reported 为 0 表示没有可用的报告（没有 usage、刚压缩过或
// 刚从数据库加载）。
type ContextSize struct {
	Reported int
	Covered  int
}

// Tokens 返回 items 作为模型历史时的上下文大小。base 是 EstimateBase 的
// 结果，只在没有可用报告时使用。estimated 为 true 表示结果完全是估算的。
func (c ContextSize) Tokens(items []protocol.Item, base int) (n int, estimated bool) {
	if c.Reported > 0 && c.Covered <= len(items) {
		return c.Reported + EstimateItems(items[c.Covered:]), false
	}
	return base + EstimateItems(items), true
}

// AutoCompactLimit 返回自动压缩阈值：上下文窗口的 90%，配置了更小的值时
// 用配置值，与 Codex 的 auto_compact_token_limit 相同。window 为 0（未知）
// 且没有配置时返回 0，表示不自动压缩。
func AutoCompactLimit(window, configured int) int {
	limit := window * 9 / 10
	if configured > 0 && (limit == 0 || configured < limit) {
		limit = configured
	}
	return limit
}

// ToolOutputLimit 返回单个工具输出保留的 token 数：配置值（0 表示默认值），
// 但不超过自动压缩阈值的四分之一，避免小窗口下一个输出就占满上下文。
func ToolOutputLimit(configured, limit int) int {
	n := configured
	if n <= 0 {
		n = DefaultToolOutputTokens
	}
	if limit > 0 {
		n = min(n, max(limit/4, 256))
	}
	return n
}

// UserMessageBudget 返回压缩后保留用户消息的 token 预算。
func UserMessageBudget(limit int) int {
	if limit > 0 {
		return min(CompactUserMessageMaxTokens, max(limit/4, 256))
	}
	return CompactUserMessageMaxTokens
}

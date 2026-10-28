package history

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

func TestApproxTokens(t *testing.T) {
	for s, want := range map[string]int{"": 0, "a": 1, "abcd": 1, "abcde": 2, "你好": 2} {
		if got := ApproxTokens(s); got != want {
			t.Errorf("ApproxTokens(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestEstimateItem(t *testing.T) {
	text := strings.Repeat("x", 400)
	tests := []struct {
		name string
		it   protocol.Item
		want int
	}{
		{"user", protocol.Item{Kind: protocol.ItemUserMessage, Text: text}, 104},
		{"agent", protocol.Item{Kind: protocol.ItemAgentMessage, Text: text}, 104},
		{"aborted marker", protocol.Item{Kind: protocol.ItemTurnAborted, Text: text}, 104},
		{"call", protocol.Item{Kind: protocol.ItemToolCall, Call: &protocol.ToolCall{CallID: "c1", Name: "read_file", Arguments: text}}, 4 + 1 + 3 + 100},
		{"output", protocol.Item{Kind: protocol.ItemToolOutput, Output: &protocol.ToolOutput{CallID: "c1", Content: text, Diff: text}}, 4 + 1 + 100},
		{"reasoning is not sent", protocol.Item{Kind: protocol.ItemReasoning, Text: text}, 0},
		{"checkpoint is not sent", protocol.Item{Kind: protocol.ItemCompaction, Text: text}, 0},
		{"nil call", protocol.Item{Kind: protocol.ItemToolCall}, 0},
	}
	for _, tt := range tests {
		if got := EstimateItem(tt.it); got != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestContextSize(t *testing.T) {
	items := []protocol.Item{user("aaaa"), agentMsg("bbbb"), user("cccc")}
	per := EstimateItem(items[0]) // 每条 5
	// 没有报告：全部估算。
	if n, est := (ContextSize{}).Tokens(items, 100); n != 100+3*per || !est {
		t.Errorf("estimated = %d %v", n, est)
	}
	// 报告覆盖前两条：报告值加上第三条的估算。
	if n, est := (ContextSize{Reported: 1000, Covered: 2}).Tokens(items, 100); n != 1000+per || est {
		t.Errorf("reported = %d %v", n, est)
	}
	// 覆盖范围超出历史（历史被替换过）：报告失效。
	if _, est := (ContextSize{Reported: 1000, Covered: 9}).Tokens(items, 100); !est {
		t.Error("stale report used")
	}
}

func TestLimits(t *testing.T) {
	for _, tt := range []struct{ window, configured, want int }{
		{32000, 0, 28800},
		{32000, 10000, 10000},
		{32000, 50000, 28800},
		{900000, 200000, 200000},
		{0, 0, 0},
		{0, 5000, 5000},
	} {
		if got := AutoCompactLimit(tt.window, tt.configured); got != tt.want {
			t.Errorf("AutoCompactLimit(%d, %d) = %d, want %d", tt.window, tt.configured, got, tt.want)
		}
	}
	if got := ToolOutputLimit(0, 0); got != DefaultToolOutputTokens {
		t.Errorf("default tool limit = %d", got)
	}
	if got := ToolOutputLimit(0, 8000); got != 2000 {
		t.Errorf("small window tool limit = %d", got)
	}
	if got := ToolOutputLimit(500, 200000); got != 500 {
		t.Errorf("configured tool limit = %d", got)
	}
	if got := UserMessageBudget(0); got != CompactUserMessageMaxTokens {
		t.Errorf("user budget = %d", got)
	}
	if got := UserMessageBudget(4000); got != 1000 {
		t.Errorf("small user budget = %d", got)
	}
}

func TestTruncateMiddle(t *testing.T) {
	s := strings.Repeat("a", 50) + strings.Repeat("b", 50)
	if got, cut := TruncateMiddle(s, 25); got != s || cut {
		t.Errorf("fits: %q %v", got, cut)
	}
	got, cut := TruncateMiddle(s, 10)
	if !cut || got != strings.Repeat("a", 20)+"…15 tokens truncated…"+strings.Repeat("b", 20) {
		t.Errorf("truncated = %q", got)
	}
	// 多字节字符：截断点不能落在字符中间。
	zh := strings.Repeat("你", 40) + strings.Repeat("好", 40) // 每个 3 字节
	got, cut = TruncateMiddle(zh, 10)
	if !cut || !utf8.ValidString(got) || !strings.HasPrefix(got, "你") || !strings.HasSuffix(got, "好") {
		t.Errorf("utf-8 = %q", got)
	}
	head, _, _ := strings.Cut(got, "…")
	if len(head) > 20 {
		t.Errorf("head is %d bytes, budget 20", len(head))
	}
	if got, cut := TruncateMiddle(s, 0); got != s || cut {
		t.Error("0 means no limit")
	}
}

//go:build smoke

package openaicompat

// Smoke 测试会调用真实网关。运行方式：
//
//	ONEAPI_API_KEY=... make smoke
//
// 没有设置 ONEAPI_API_KEY 时读取仓库根目录的 joey.toml。
// 可选：ONEAPI_BASE_URL（默认 OneAPI comate）、JOEY_SMOKE_MODEL（默认 gpt-6.1-sol）。

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/config"
	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

func smokeClient(t *testing.T) *Client {
	t.Helper()
	key := os.Getenv("ONEAPI_API_KEY")
	if key == "" {
		if cfg, err := config.Load("../../../joey.toml", os.Getenv); err == nil {
			key = cfg.Provider.APIKey
		}
	}
	if key == "" {
		t.Skip("ONEAPI_API_KEY not set and joey.toml has no api_key")
	}
	base := os.Getenv("ONEAPI_BASE_URL")
	if base == "" {
		base = "https://oneapi-comate.baidu-int.com/v1"
	}
	model := os.Getenv("JOEY_SMOKE_MODEL")
	if model == "" {
		model = "gpt-6.1-sol"
	}
	c, err := New(Config{BaseURL: base, APIKey: key, MaxRetries: 1, Model: llm.ModelInfo{
		Name: model, Streaming: true, ToolMode: llm.ToolsNative, MaxTokensParam: "max_completion_tokens",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSmokeToolRoundTrip(t *testing.T) {
	c := smokeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	tools := []llm.ToolSpec{{
		Name: "read_file", Description: "Read a text file",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}}
	items := []protocol.Item{{Kind: protocol.ItemUserMessage, Text: "Use read_file to read README.md, then tell me the project name."}}

	var call *protocol.ToolCall
	var usage *protocol.Usage
	for ev, err := range c.Stream(ctx, llm.Request{Items: items, Tools: tools}) {
		if err != nil {
			t.Fatalf("first request: %v", err)
		}
		if ev.Kind == llm.EventToolCall && call == nil {
			call = ev.Call
		}
		if ev.Kind == llm.EventCompleted {
			usage = ev.Usage
		}
	}
	if call == nil || call.Name != "read_file" {
		t.Fatalf("model did not call read_file (call=%v)", call)
	}
	t.Logf("capability: tool call id=%q args=%s usage_reported=%v", call.CallID, call.Arguments, usage != nil)

	items = append(items,
		protocol.Item{Kind: protocol.ItemToolCall, Call: call},
		protocol.Item{Kind: protocol.ItemToolOutput, Output: &protocol.ToolOutput{CallID: call.CallID, Content: "# Joey-agent\n"}},
	)
	var text strings.Builder
	for ev, err := range c.Stream(ctx, llm.Request{Items: items, Tools: tools}) {
		if err != nil {
			t.Fatalf("second request: %v", err)
		}
		if ev.Kind == llm.EventTextDelta {
			text.WriteString(ev.Delta)
		}
	}
	if !strings.Contains(strings.ToLower(text.String()), "joey") {
		t.Errorf("answer does not mention the project: %q", text.String())
	}
}

// TestSmokeContextOverflow 发送约 110 万 token 的输入，确认网关的上下文超限
// 错误能被 llm.IsContextOverflow 识别。请求很大，只在 JOEY_SMOKE_OVERFLOW=1
// 时运行。
func TestSmokeContextOverflow(t *testing.T) {
	if os.Getenv("JOEY_SMOKE_OVERFLOW") != "1" {
		t.Skip("set JOEY_SMOKE_OVERFLOW=1 to send a ~1.1M-token request")
	}
	c := smokeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n := 1_100_000
	if v, _ := strconv.Atoi(os.Getenv("JOEY_SMOKE_WINDOW_PROBE")); v > 0 {
		n = v
	}
	items := []protocol.Item{{Kind: protocol.ItemUserMessage, Text: strings.Repeat("hello ", n) + "\nReply with: ok"}}
	var got error
	for ev, err := range c.Stream(ctx, llm.Request{Items: items}) {
		if err != nil {
			got = err
			break
		}
		if ev.Kind == llm.EventCompleted && ev.Usage != nil {
			t.Logf("usage: %+v", *ev.Usage)
		}
	}
	t.Logf("error: %v", got)
	if n, _ := strconv.Atoi(os.Getenv("JOEY_SMOKE_WINDOW_PROBE")); n > 0 {
		return
	}
	if !llm.IsContextOverflow(got) {
		t.Errorf("IsContextOverflow(%v) = false", got)
	}
}

// TestSmokeSummaryWithoutTools 确认历史里有工具调用、请求里不带工具定义时
// 网关仍能返回文本。上下文压缩就是这样请求摘要的。
func TestSmokeSummaryWithoutTools(t *testing.T) {
	c := smokeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	call := &protocol.ToolCall{CallID: "call_1", Name: "read_file", Arguments: `{"path":"README.md"}`}
	items := []protocol.Item{
		{Kind: protocol.ItemUserMessage, Text: "Read README.md and tell me the project name."},
		{Kind: protocol.ItemToolCall, Call: call},
		{Kind: protocol.ItemToolOutput, Output: &protocol.ToolOutput{CallID: "call_1", Content: "# Joey-agent\nA Go agent harness."}},
		{Kind: protocol.ItemUserMessage, Text: "Summarize the conversation so far in one sentence."},
	}
	var text strings.Builder
	for ev, err := range c.Stream(ctx, llm.Request{Items: items}) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == llm.EventToolCall {
			t.Fatalf("model called a tool without tool definitions: %+v", ev.Call)
		}
		if ev.Kind == llm.EventTextDelta {
			text.WriteString(ev.Delta)
		}
	}
	t.Logf("summary: %s", text.String())
	if !strings.Contains(strings.ToLower(text.String()), "joey") {
		t.Errorf("summary does not mention the project: %q", text.String())
	}
}

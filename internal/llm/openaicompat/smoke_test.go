//go:build smoke

package openaicompat

// Smoke 测试会调用真实网关。运行方式：
//
//	ONEAPI_API_KEY=... make smoke
//
// 可选：ONEAPI_BASE_URL（默认 OneAPI comate）、JOEY_SMOKE_MODEL（默认 gpt-6.1-sol）。

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

func smokeClient(t *testing.T) *Client {
	t.Helper()
	key := os.Getenv("ONEAPI_API_KEY")
	if key == "" {
		t.Skip("ONEAPI_API_KEY not set")
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

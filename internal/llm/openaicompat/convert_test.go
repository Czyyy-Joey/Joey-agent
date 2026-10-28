package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

func TestToMessages(t *testing.T) {
	items := []protocol.Item{
		{Kind: protocol.ItemUserMessage, Text: "read two files"},
		{Kind: protocol.ItemReasoning, Text: "thinking is not sent back"},
		{Kind: protocol.ItemAgentMessage, Text: "Let me look."},
		{Kind: protocol.ItemToolCall, Call: &protocol.ToolCall{CallID: "c1", Name: "read_file", Arguments: `{"path":"a"}`}},
		{Kind: protocol.ItemToolCall, Call: &protocol.ToolCall{CallID: "c2", Name: "read_file", Arguments: `{"path":"b"}`}},
		{Kind: protocol.ItemToolOutput, Output: &protocol.ToolOutput{CallID: "c1", Content: "A", Diff: "--- a/a\n+++ b/a\n"}},
		{Kind: protocol.ItemToolOutput, Output: &protocol.ToolOutput{CallID: "c2", Content: "B", IsError: true}},
		{Kind: protocol.ItemAgentMessage, Text: "Done."},
		{Kind: protocol.ItemTurnAborted, Text: "[turn aborted]"},
		{Kind: protocol.ItemUserMessage, Text: "thanks"},
	}
	got, err := json.Marshal(toMessages("sys", items))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"role":"system","content":"sys"},` +
		`{"role":"user","content":"read two files"},` +
		`{"role":"assistant","content":"Let me look.","tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}},` +
		`{"id":"c2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"b\"}"}}]},` +
		`{"role":"tool","content":"A","tool_call_id":"c1"},` +
		`{"role":"tool","content":"B","tool_call_id":"c2"},` +
		`{"role":"assistant","content":"Done."},` +
		`{"role":"user","content":"[turn aborted]"},` +
		`{"role":"user","content":"thanks"}]`
	if string(got) != want {
		t.Errorf("messages mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestToMessagesToolCallWithoutText(t *testing.T) {
	items := []protocol.Item{
		{Kind: protocol.ItemUserMessage, Text: "hi"},
		{Kind: protocol.ItemToolCall, Call: &protocol.ToolCall{CallID: "c1", Name: "x", Arguments: "{}"}},
	}
	got, _ := json.Marshal(toMessages("", items))
	// 当 assistant 消息只包含工具调用时，content 必须是显式的 null。
	if !strings.Contains(string(got), `{"role":"assistant","content":null,"tool_calls"`) {
		t.Errorf("got %s", got)
	}
}

func TestSSEDataParsing(t *testing.T) {
	input := ": comment\n\nevent: message\nid: 7\ndata: first\ndata: second\n\ndata:nospace\n\ndata: tail-without-blank-line"
	var got []string
	for d, err := range sseData(strings.NewReader(input)) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(d))
	}
	want := []string{"first\nsecond", "nospace", "tail-without-blank-line"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

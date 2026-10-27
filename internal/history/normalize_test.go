package history

import (
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

func user(id string) protocol.Item {
	return protocol.Item{ID: id, TurnID: "t", Kind: protocol.ItemUserMessage, Text: id}
}

func agentMsg(id string) protocol.Item {
	return protocol.Item{ID: id, TurnID: "t", Kind: protocol.ItemAgentMessage, Text: id}
}

func call(id string) protocol.Item {
	return protocol.Item{ID: "ic_" + id, TurnID: "t", Kind: protocol.ItemToolCall, Call: &protocol.ToolCall{CallID: id, Name: "read_file"}}
}

func output(id string) protocol.Item {
	return protocol.Item{ID: "io_" + id, TurnID: "t", Kind: protocol.ItemToolOutput, Output: &protocol.ToolOutput{CallID: id, Content: "ok"}}
}

// shape 把条目渲染成紧凑形式："u1 c:a o:a"；补上的输出写作 "o:a!"。
func shape(items []protocol.Item) string {
	s := ""
	for i, it := range items {
		if i > 0 {
			s += " "
		}
		switch it.Kind {
		case protocol.ItemToolCall:
			s += "c:" + it.Call.CallID
		case protocol.ItemToolOutput:
			s += "o:" + it.Output.CallID
			if it.Output.Content == RestartedMessage {
				s += "!"
			}
		default:
			s += it.ID
		}
	}
	return s
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name      string
		in        []protocol.Item
		want      string
		wantAdded int
	}{
		{"empty", nil, "", 0},
		{"well formed", []protocol.Item{user("u1"), call("a"), call("b"), output("a"), output("b"), agentMsg("m1")}, "u1 c:a c:b o:a o:b m1", 0},
		{"missing output at end", []protocol.Item{user("u1"), call("a"), call("b"), output("a")}, "u1 c:a c:b o:a o:b!", 1},
		{"all outputs missing", []protocol.Item{user("u1"), agentMsg("m1"), call("a"), call("b")}, "u1 m1 c:a c:b o:a! o:b!", 2},
		{"missing before next user", []protocol.Item{user("u1"), call("a"), user("u2")}, "u1 c:a o:a! u2", 1},
		{"orphan output dropped", []protocol.Item{user("u1"), output("x"), agentMsg("m1")}, "u1 m1", 0},
		{"duplicate output dropped", []protocol.Item{call("a"), output("a"), output("a")}, "c:a o:a", 0},
		{"late output after flush dropped", []protocol.Item{call("a"), user("u2"), output("a")}, "c:a o:a! u2", 1},
		{"outputs out of order kept", []protocol.Item{call("a"), call("b"), output("b"), output("a")}, "c:a c:b o:b o:a", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := shape(tt.in)
			got, added := Normalize(tt.in, RestartedMessage)
			if s := shape(got); s != tt.want {
				t.Errorf("got %q, want %q", s, tt.want)
			}
			if len(added) != tt.wantAdded {
				t.Errorf("added %d, want %d", len(added), tt.wantAdded)
			}
			for _, it := range added {
				if it.ID == "" || it.TurnID != "t" || !it.Output.IsError {
					t.Errorf("synthetic item = %+v", it)
				}
			}
			if shape(tt.in) != before {
				t.Error("input modified")
			}
		})
	}
}

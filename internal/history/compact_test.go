package history

import (
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

func TestBuildCompacted(t *testing.T) {
	old := protocol.Item{ID: "s0", Kind: protocol.ItemUserMessage, Text: SummaryPrefix + "\nearlier summary"}
	items := []protocol.Item{
		old,
		user("goal"), call("a"), output("a"), agentMsg("m1"),
		{ID: "x", Kind: protocol.ItemTurnAborted, Text: "<turn_aborted>"},
		user("next"),
	}
	got := BuildCompacted(items, "the summary", 1000, "t9")
	if s := shape(got[:len(got)-1]); s != "goal next" {
		t.Errorf("kept = %q", s)
	}
	last := got[len(got)-1]
	if last.Kind != protocol.ItemUserMessage || last.TurnID != "t9" || !IsSummary(last.Text) || !strings.HasSuffix(last.Text, "\nthe summary") {
		t.Errorf("summary item = %+v", last)
	}
	if _, added := Normalize(got, MissingMessage); len(added) != 0 {
		t.Error("compacted history is not normalized")
	}

	// 预算：从最新的往前选，超出预算的那条截断后保留，更早的丢弃。
	long := protocol.Item{ID: "long", Kind: protocol.ItemUserMessage, Text: strings.Repeat("y", 400)}
	got = BuildCompacted([]protocol.Item{user("first"), long, user("last")}, "", 12, "t")
	if len(got) != 3 || got[0].ID != "long" || got[1].ID != "last" {
		t.Fatalf("budget = %q", shape(got))
	}
	if !strings.Contains(got[0].Text, "tokens truncated") || ApproxTokens(got[0].Text) > 30 {
		t.Errorf("truncated message = %q", got[0].Text)
	}
	if !strings.HasSuffix(got[2].Text, "(no summary available)") {
		t.Errorf("empty summary = %q", got[2].Text)
	}
}

func TestDropOldest(t *testing.T) {
	items := []protocol.Item{call("a"), output("a"), user("u1"), call("b"), output("b")}
	got := DropOldest(items)
	if s := shape(got); s != "u1 c:b o:b" {
		t.Errorf("drop call = %q", s)
	}
	if s := shape(DropOldest(got)); s != "c:b o:b" {
		t.Errorf("drop user = %q", s)
	}
	if len(DropOldest(nil)) != 0 {
		t.Error("empty")
	}
}

func TestReplay(t *testing.T) {
	items := []protocol.Item{user("u1"), agentMsg("m1")}
	if s := shape(Replay(items)); s != "u1 m1" {
		t.Errorf("no checkpoint = %q", s)
	}
	cp1 := protocol.Item{ID: "cp1", Kind: protocol.ItemCompaction, Compaction: &protocol.Compaction{Replacement: []protocol.Item{user("u1"), user("sum1")}}}
	cp2 := protocol.Item{ID: "cp2", Kind: protocol.ItemCompaction, Compaction: &protocol.Compaction{Replacement: []protocol.Item{user("u2"), user("sum2")}}}
	items = append(items, cp1, user("u2"), call("a"), output("a"), cp2, agentMsg("m2"))
	if s := shape(Replay(items)); s != "u2 sum2 m2" {
		t.Errorf("replay = %q", s)
	}
	if s := shape(Replay(items[:6])); s != "u1 sum1 u2 c:a o:a" {
		t.Errorf("replay first = %q", s)
	}
}

func TestCompactRequest(t *testing.T) {
	items := []protocol.Item{user("u1")}
	req := CompactRequest(items)
	if len(req) != 2 || req[1].Text != CompactPrompt || len(items) != 1 {
		t.Errorf("request = %+v", req)
	}
	if !strings.Contains(CompactPrompt, "CONTEXT CHECKPOINT COMPACTION") || !strings.HasPrefix(SummaryPrefix, "Another language model") {
		t.Error("prompts not embedded")
	}
}

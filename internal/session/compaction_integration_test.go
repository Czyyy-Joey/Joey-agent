package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/store"
)

// TestFiftyTurnConversation 跑 50 轮对话，每轮回答约 100 token。阈值设在
// 大约第 30 轮越过的位置：恰好压缩一次，压缩后的请求带着摘要和最初的目标，
// 之后的轮次继续使用压缩后的历史。
func TestFiftyTurnConversation(t *testing.T) {
	answer := strings.Repeat("detail ", 56) // ~100 tokens
	p := llmtest.NewScripted(llmtest.Step{Text: answer})
	p.Loop = true
	db, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Runtime{Provider: p, Store: db, AutoCompactLimit: 3500})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { m.Close(0); db.Close() }()
	s, _ := m.Create("")
	runTurn(t, s, "GOAL: keep a running design log")
	for i := 2; i <= 50; i++ {
		runTurn(t, s, fmt.Sprintf("turn %d", i))
	}

	info := s.Info()
	var cps []protocol.Item
	for _, it := range info.Items {
		if it.Kind == protocol.ItemCompaction {
			cps = append(cps, it)
		}
	}
	if len(cps) != 1 {
		t.Fatalf("%d compactions, want 1", len(cps))
	}
	c := cps[0].Compaction
	if c.Reason != protocol.CompactPreTurn || c.TokensBefore < 3500 || c.TokensAfter >= c.TokensBefore/2 {
		t.Errorf("compaction = %+v", *c)
	}

	// 找到压缩之后的第一个正常请求，检查它的开头。
	reqs := p.Requests()
	after := -1
	for i, r := range reqs {
		if n := len(r.Items); n > 0 && r.Items[n-1].Text == history.CompactPrompt {
			after = i + 1
		}
	}
	first := reqs[after].Items
	if first[0].Text != "GOAL: keep a running design log" {
		t.Errorf("first item after compaction = %q", first[0].Text)
	}
	var summaries int
	for _, it := range first {
		if history.IsSummary(it.Text) {
			summaries++
		}
		if it.Kind == protocol.ItemAgentMessage {
			t.Fatal("old answers survived compaction")
		}
	}
	if summaries != 1 {
		t.Errorf("%d summaries in the request", summaries)
	}
	// 之后的请求在压缩后的历史上继续增长。
	last := reqs[len(reqs)-1].Items
	if len(last) <= len(first) || last[0].Text != first[0].Text {
		t.Errorf("last request has %d items, first after compaction %d", len(last), len(first))
	}
	if est := history.EstimateItems(last); est >= 3500 {
		t.Errorf("context is %d tokens at the end, still above the limit", est)
	}
}

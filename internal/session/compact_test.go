package session

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Czyyy-Joey/joey-agent/internal/history"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/store"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// openCompacting 和 env.open 一样，但设置了自动压缩阈值。
func (e env) openCompacting(t *testing.T, p *llmtest.Scripted, limit int) *Manager {
	t.Helper()
	db, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Runtime{
		Provider: p, Store: db, Workspaces: e.ws, AutoCompactLimit: limit,
		Tools: func(e ToolEnv) (*tools.Router, error) { return tools.NewRouter(rootTool{e.Root}) },
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return m
}

// TestCompactionCheckpointSurvivesRestart：压缩检查点写进库里，重启后从
// 检查点重放，发给模型的历史和压缩后的内存状态一致。
func TestCompactionCheckpointSurvivesRestart(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("words ", 200)
	m := e.openCompacting(t, llmtest.NewScripted(
		llmtest.Step{Text: long},          // 第一轮
		llmtest.Step{Text: "summary one"}, // 第二轮开始前压缩
		llmtest.Step{Text: "short"},       // 第二轮
		llmtest.Step{Text: "probe"},       // 第三轮（用来看请求）
	), 500)
	s, err := m.Create("")
	if err != nil {
		t.Fatal(err)
	}
	runTurn(t, s, "goal: remember 42")
	evs := runTurn(t, s, "second question")
	var started, completed bool
	for _, ev := range evs {
		if d, ok := ev.Data.(protocol.ItemData); ok && d.Kind == protocol.ItemCompaction {
			started = started || ev.Type == protocol.EventItemStarted
			if ev.Type == protocol.EventItemCompleted && d.Compaction != nil && d.Compaction.Replacement == nil {
				completed = true
			}
		}
	}
	if !started || !completed {
		t.Fatalf("compaction events missing (started=%v completed=%v)", started, completed)
	}
	before := s.Info()
	inMemory := history.Replay(before.Items)
	if len(inMemory) != 4 || inMemory[0].Text != "goal: remember 42" || !history.IsSummary(inMemory[1].Text) {
		t.Fatalf("model history = %+v", inMemory)
	}
	e.close(m)

	p2 := llmtest.NewScripted(llmtest.Step{Text: "42"})
	m2 := e.openCompacting(t, p2, 0)
	defer e.close(m2)
	s2, err := m2.Get(s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, s2.Info()); diff != "" {
		t.Errorf("snapshot changed across restart:\n%s", diff)
	}
	runTurn(t, s2, "what was the goal?")
	sent := p2.Requests()[0].Items
	if diff := cmp.Diff(inMemory, sent[:len(sent)-1]); diff != "" {
		t.Errorf("history after restart differs from the compacted state:\n%s", diff)
	}
}

func TestManualCompactTurn(t *testing.T) {
	m, p := newManager(t, llmtest.Step{Text: "answer"}, llmtest.Step{Text: "the summary"}, llmtest.Step{Text: "after"})
	s, _ := m.Create("")
	runTurn(t, s, "goal")
	_, sub, _ := s.Bus().Subscribe(s.Bus().LastSeq())
	defer sub.Cancel()
	id, err := s.Compact()
	if err != nil {
		t.Fatal(err)
	}
	evs := waitTurn(t, sub, id)
	if turnStatus(evs) != protocol.TurnCompleted || evs[0].Data.(protocol.TurnData).Input != CompactCommand {
		t.Fatalf("compact turn = %s", turnStatus(evs))
	}
	runTurn(t, s, "next")
	last := p.Requests()[2].Items
	if len(last) != 3 || last[0].Text != "goal" || !strings.HasSuffix(last[1].Text, "the summary") || last[2].Text != "next" {
		t.Errorf("request after /compact = %+v", last)
	}
}

// TestBackgroundCompactionInSession：会话的后台压缩器跨轮次存在。第一轮结束
// 时在后台生成摘要，第二轮结束时采用；检查点带 background 标记，重启后照常
// 重放。
func TestBackgroundCompactionInSession(t *testing.T) {
	e := newEnv(t)
	db, err := store.Open(e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	p := llmtest.NewScripted(
		llmtest.Step{Text: strings.Repeat("long answer ", 200)},
		llmtest.Step{Text: "BG SUMMARY"},
		llmtest.Step{Text: "answer 2"},
	)
	m, err := NewManager(Runtime{
		Provider: p, Store: db, Workspaces: e.ws,
		AutoCompactLimit: 100_000, PostTurnCompactPercent: 50, BackgroundCompactPercent: 1,
		Tools: func(e ToolEnv) (*tools.Router, error) { return tools.NewRouter(rootTool{e.Root}) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := m.Create("")
	if err != nil {
		t.Fatal(err)
	}
	runTurn(t, s, "goal")
	s.compactor.Wait()
	if len(p.Requests()) != 2 || !isCompaction(p.Requests()[1].Items) {
		t.Fatal("second request is not the background compaction")
	}
	s.compactor.Wait()
	runTurn(t, s, "next")

	var cp *protocol.Compaction
	for _, it := range s.Info().Items {
		if it.Kind == protocol.ItemCompaction {
			cp = it.Compaction
		}
	}
	if cp == nil || cp.Reason != protocol.CompactPostTurn || !cp.Background {
		t.Fatalf("checkpoint = %+v", cp)
	}
	before := history.Replay(s.Info().Items)
	m.Close(time.Second)
	db.Close()

	m2 := e.openCompacting(t, llmtest.NewScripted(), 0)
	defer e.close(m2)
	s2, err := m2.Get(s.ID())
	if err != nil {
		t.Fatal(err)
	}
	after := history.Replay(s2.Info().Items)
	if diff := cmp.Diff(before, after); diff != "" || !strings.Contains(after[1].Text, "BG SUMMARY") {
		t.Errorf("history after restart:\n%s", diff)
	}
}

func isCompaction(items []protocol.Item) bool {
	return len(items) > 0 && items[len(items)-1].Text == history.CompactPrompt
}

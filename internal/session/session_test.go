package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

func newManager(t *testing.T, steps ...llmtest.Step) (*Manager, *llmtest.Scripted) {
	t.Helper()
	p := llmtest.NewScripted(steps...)
	r, err := tools.NewRouter()
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(Runtime{Provider: p, Tools: func(ToolEnv) (*tools.Router, error) { return r, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(time.Second) })
	return m, p
}

// waitTurn 收集 turnID 的事件，直到 turn.completed。
func waitTurn(t *testing.T, sub *Subscription, turnID string) []protocol.Event {
	t.Helper()
	var evs []protocol.Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatal("subscription closed")
			}
			if e.TurnID != turnID {
				continue
			}
			evs = append(evs, e)
			if e.Type == protocol.EventTurnCompleted {
				return evs
			}
		case <-timeout:
			t.Fatalf("turn %s did not complete; got %d events", turnID, len(evs))
		}
	}
}

func turnStatus(evs []protocol.Event) protocol.TurnStatus {
	return evs[len(evs)-1].Data.(protocol.TurnData).Status
}

func TestSubmitAndHistory(t *testing.T) {
	m, p := newManager(t, llmtest.Step{Text: "first"}, llmtest.Step{Text: "second"})
	s, _ := m.Create("")
	_, sub, _ := s.Bus().Subscribe(0)
	defer sub.Cancel()

	id1, err := s.Submit("one")
	if err != nil {
		t.Fatal(err)
	}
	evs := waitTurn(t, sub, id1)
	if evs[0].Type != protocol.EventTurnStarted || turnStatus(evs) != protocol.TurnCompleted {
		t.Fatalf("events = %+v", evs)
	}
	for i, e := range evs {
		if e.SessionID != s.ID() || e.AgentID != RootAgentID || e.TS.IsZero() {
			t.Errorf("event %d missing envelope fields: %+v", i, e)
		}
		if i > 0 && e.Seq != evs[i-1].Seq+1 {
			t.Errorf("seq gap at %d", i)
		}
	}

	id2, err := s.Submit("two")
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, sub, id2)
	if items := p.Requests()[1].Items; len(items) != 3 || items[1].Text != "first" {
		t.Errorf("second request history = %+v", items)
	}
	info := s.Info()
	if info.Status != "idle" || len(info.Items) != 4 || info.LastTurn != protocol.TurnCompleted || len(info.Turns) != 2 || info.Title != "one" {
		t.Errorf("info = %+v", info)
	}
}

func TestOneActiveTurn(t *testing.T) {
	m, _ := newManager(t, llmtest.Step{Text: "working", Hang: true}, llmtest.Step{Text: "after"})
	s, _ := m.Create("")
	_, sub, _ := s.Bus().Subscribe(0)
	defer sub.Cancel()

	id, err := s.Submit("long")
	if err != nil {
		t.Fatal(err)
	}
	// 等待轮次开始流式输出，此时它已消费了挂起的步骤。
	for e := range sub.C {
		if e.Type == protocol.EventItemDelta {
			break
		}
	}
	var wg sync.WaitGroup
	var conflicts int
	var mu sync.Mutex
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Submit("again"); errors.Is(err, ErrTurnActive) {
				mu.Lock()
				conflicts++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if conflicts != 10 {
		t.Errorf("conflicts = %d, want 10", conflicts)
	}
	if info := s.Info(); info.Status != "running" || info.ActiveTurn != id {
		t.Errorf("info = %+v", info)
	}

	start := time.Now()
	if !s.Interrupt(2 * time.Second) {
		t.Fatal("Interrupt returned false")
	}
	if time.Since(start) > time.Second {
		t.Error("interrupt took too long")
	}
	if got := turnStatus(waitTurn(t, sub, id)); got != protocol.TurnInterrupted {
		t.Errorf("status = %s", got)
	}

	id2, err := s.Submit("after interrupt")
	if err != nil {
		t.Fatalf("submit after interrupt: %v", err)
	}
	if got := turnStatus(waitTurn(t, sub, id2)); got != protocol.TurnCompleted {
		t.Errorf("status = %s", got)
	}
	if s.Interrupt(time.Second) {
		t.Error("Interrupt on idle session returned true")
	}
}

func TestInfoLastSeqDuringTurn(t *testing.T) {
	m, _ := newManager(t, llmtest.Step{Text: "a"}, llmtest.Step{Text: "partial", Hang: true})
	s, _ := m.Create("")
	_, sub, _ := s.Bus().Subscribe(0)
	defer sub.Cancel()
	id, _ := s.Submit("one")
	waitTurn(t, sub, id)
	idle := s.Info()
	if idle.LastSeq != s.Bus().LastSeq() {
		t.Errorf("idle LastSeq = %d, bus = %d", idle.LastSeq, s.Bus().LastSeq())
	}

	id2, _ := s.Submit("two")
	// 等待部分 delta，然后检查快照指向该轮次之前。
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-sub.C:
			if e.Type != protocol.EventItemDelta {
				continue
			}
			info := s.Info()
			backlog, sub2, _ := s.Bus().Subscribe(info.LastSeq)
			sub2.Cancel()
			if len(backlog) == 0 || backlog[0].Type != protocol.EventTurnStarted || backlog[0].TurnID != id2 {
				t.Fatalf("replay from LastSeq should start with turn.started of the running turn, got %+v", backlog)
			}
			return
		case <-deadline:
			t.Fatal("no delta")
		}
	}
}

func TestManagerClose(t *testing.T) {
	m, _ := newManager(t, llmtest.Step{Hang: true})
	s, _ := m.Create("")
	_, sub, _ := s.Bus().Subscribe(0)
	id, _ := s.Submit("x")
	m.Close(2 * time.Second)

	var last protocol.Event
	for e := range sub.C {
		if e.TurnID == id {
			last = e
		}
	}
	if last.Type != protocol.EventTurnCompleted || last.Data.(protocol.TurnData).Status != protocol.TurnInterrupted {
		t.Errorf("last event = %+v", last)
	}
	if _, err := s.Submit("y"); !errors.Is(err, ErrClosed) {
		t.Errorf("submit after close: %v", err)
	}
	if _, err := m.Create(""); !errors.Is(err, ErrClosed) {
		t.Errorf("create after close: %v", err)
	}
	if _, err := m.Get("nope"); !errors.Is(err, ErrClosed) {
		t.Errorf("get unknown after close: %v", err)
	}
	if got, err := m.Get(s.ID()); err != nil || got != s {
		t.Errorf("get loaded session after close: %v", err)
	}
}

func TestWait(t *testing.T) {
	m, _ := newManager(t, llmtest.Step{Text: "x", Delay: llmtest.Duration(20 * time.Millisecond)})
	s, _ := m.Create("")
	if err := s.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit("x"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Info().Status != "idle" {
		t.Error("still running after Wait")
	}
}

func TestDeleteSession(t *testing.T) {
	m, _ := newManager(t, llmtest.Step{Text: "answer"}, llmtest.Step{Text: "working", Hang: true})
	s, _ := m.Create("")
	_, sub, _ := s.Bus().Subscribe(0)
	id, _ := s.Submit("first")
	waitTurn(t, sub, id)

	// 运行中的会话不能删除。
	id, err := s.Submit("long")
	if err != nil {
		t.Fatal(err)
	}
	for e := range sub.C {
		if e.Type == protocol.EventItemDelta {
			break
		}
	}
	if err := m.Delete(s.ID()); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("delete running = %v", err)
	}
	s.Interrupt(2 * time.Second)
	waitTurn(t, sub, id)

	if err := m.Delete(s.ID()); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-sub.C; ok {
		// 订阅里可能还有缓冲的事件，读完后应当关闭。
		for range sub.C {
		}
	}
	if _, err := m.Get(s.ID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete = %v", err)
	}
	if _, err := s.Submit("again"); !errors.Is(err, ErrNotFound) {
		t.Errorf("submit on a deleted session = %v", err)
	}
	if list, _ := m.List(10); len(list) != 0 {
		t.Errorf("list = %+v", list)
	}
	if err := m.Delete(s.ID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v", err)
	}
}

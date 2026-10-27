package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

var ctx = context.Background()

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "joey.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newSession(t *testing.T, db *DB, id string, updated time.Time) {
	t.Helper()
	mustDo(t, db.CreateSession(ctx, SessionRecord{ID: id, Cwd: "/w/" + id, Model: "m", CreatedAt: updated, UpdatedAt: updated}))
}

func TestSessionsCRUD(t *testing.T) {
	db, _ := openTemp(t)
	t0 := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	newSession(t, db, "a", t0)
	newSession(t, db, "b", t0.Add(time.Minute))
	if err := db.CreateSession(ctx, SessionRecord{ID: "a", Cwd: "/x"}); err == nil {
		t.Error("duplicate id accepted")
	}

	mustDo(t, db.UpdateSession(ctx, "a", "hello", t0.Add(time.Hour)))
	got, err := db.GetSession(ctx, "a")
	mustDo(t, err)
	want := SessionRecord{ID: "a", Title: "hello", Cwd: "/w/a", Model: "m", CreatedAt: t0, UpdatedAt: t0.Add(time.Hour)}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("session (-want +got):\n%s", diff)
	}

	list, err := db.ListSessions(ctx, 10)
	mustDo(t, err)
	if len(list) != 2 || list[0].ID != "a" || list[1].ID != "b" {
		t.Errorf("list order = %+v", list)
	}
	if list, _ := db.ListSessions(ctx, 1); len(list) != 1 {
		t.Errorf("limit ignored: %d", len(list))
	}

	if _, err := db.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	if err := db.UpdateSession(ctx, "nope", "", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
}

func TestItemsRoundTrip(t *testing.T) {
	db, _ := openTemp(t)
	newSession(t, db, "s", time.Now())
	items := []protocol.Item{
		{ID: "i1", TurnID: "t1", Kind: protocol.ItemUserMessage, Text: "读 README"},
		{ID: "i2", TurnID: "t1", Kind: protocol.ItemToolCall, Call: &protocol.ToolCall{CallID: "c1", Name: "read_file", Arguments: `{"path":"README.md"}`}},
		{ID: "i3", TurnID: "t1", Kind: protocol.ItemToolOutput, Output: &protocol.ToolOutput{CallID: "c1", Content: "x", IsError: true}},
		{ID: "i4", TurnID: "t1", Kind: protocol.ItemAgentMessage, Text: "done"},
	}
	for _, it := range items {
		mustDo(t, db.AppendItem(ctx, "s", it))
	}
	got, err := db.LoadItems(ctx, "s")
	mustDo(t, err)
	if diff := cmp.Diff(items, got); diff != "" {
		t.Errorf("items (-want +got):\n%s", diff)
	}
	if err := db.AppendItem(ctx, "unknown", items[0]); err == nil {
		t.Error("item for unknown session accepted (foreign key)")
	}
}

func TestEvents(t *testing.T) {
	db, _ := openTemp(t)
	newSession(t, db, "s", time.Now())
	ts := time.Date(2026, 10, 27, 8, 0, 0, 123456000, time.UTC)
	for i := int64(1); i <= 5; i++ {
		mustDo(t, db.AppendEvent(ctx, protocol.Event{
			Seq: i, SessionID: "s", AgentID: "root", TurnID: "t1", ItemID: "it", Type: protocol.EventItemDelta, TS: ts,
			Data: protocol.ItemData{Kind: protocol.ItemAgentMessage, Text: strings.Repeat("x", int(i))},
		}))
	}
	if err := db.AppendEvent(ctx, protocol.Event{Seq: 3, SessionID: "s", Type: protocol.EventWarning}); err == nil {
		t.Error("duplicate seq accepted")
	}

	last, err := db.LastEventSeq(ctx, "s")
	mustDo(t, err)
	if last != 5 {
		t.Errorf("last = %d", last)
	}
	evs, err := db.EventsAfter(ctx, "s", 1, 4)
	mustDo(t, err)
	if len(evs) != 3 || evs[0].Seq != 2 || evs[2].Seq != 4 {
		t.Fatalf("events = %+v", evs)
	}
	e := evs[0]
	if e.SessionID != "s" || e.AgentID != "root" || e.TurnID != "t1" || e.ItemID != "it" || e.Type != protocol.EventItemDelta || !e.TS.Equal(ts) {
		t.Errorf("envelope = %+v", e)
	}
	var d protocol.ItemData
	mustDo(t, json.Unmarshal(e.Data.(json.RawMessage), &d))
	if d.Text != "xx" {
		t.Errorf("data = %+v", d)
	}
	if n, _ := db.LastEventSeq(ctx, "other"); n != 0 {
		t.Errorf("empty session last seq = %d", n)
	}
}

func TestTurns(t *testing.T) {
	db, _ := openTemp(t)
	t0 := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	newSession(t, db, "s", t0)
	mustDo(t, db.StartTurn(ctx, TurnRecord{ID: "t1", SessionID: "s", Status: protocol.TurnRunning, Input: "a", StartSeq: 1, StartedAt: t0}))
	mustDo(t, db.StartTurn(ctx, TurnRecord{ID: "t2", SessionID: "s", Status: protocol.TurnRunning, Input: "b", StartSeq: 9, StartedAt: t0.Add(time.Second)}))

	running, err := db.RunningTurns(ctx)
	mustDo(t, err)
	if len(running) != 2 {
		t.Fatalf("running = %+v", running)
	}

	usage := protocol.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
	mustDo(t, db.FinishTurn(ctx, TurnRecord{ID: "t1", Status: protocol.TurnCompleted, Usage: usage, EndedAt: t0.Add(time.Minute)}))
	turns, err := db.ListTurns(ctx, "s")
	mustDo(t, err)
	want := []TurnRecord{
		{ID: "t1", SessionID: "s", Status: protocol.TurnCompleted, Input: "a", StartSeq: 1, Usage: usage, StartedAt: t0, EndedAt: t0.Add(time.Minute)},
		{ID: "t2", SessionID: "s", Status: protocol.TurnRunning, Input: "b", StartSeq: 9, StartedAt: t0.Add(time.Second)},
	}
	if diff := cmp.Diff(want, turns); diff != "" {
		t.Errorf("turns (-want +got):\n%s", diff)
	}
	if running, _ := db.RunningTurns(ctx); len(running) != 1 || running[0].ID != "t2" {
		t.Errorf("running after finish = %+v", running)
	}
	if err := db.FinishTurn(ctx, TurnRecord{ID: "nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("finish missing: %v", err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	db, path := openTemp(t)
	newSession(t, db, "s", time.Now())
	mustDo(t, db.AppendItem(ctx, "s", protocol.Item{ID: "i1", Kind: protocol.ItemUserMessage, Text: "hi"}))
	mustDo(t, db.Close())

	db2, err := Open(path)
	mustDo(t, err)
	defer db2.Close()
	items, err := db2.LoadItems(ctx, "s")
	mustDo(t, err)
	if len(items) != 1 || items[0].Text != "hi" {
		t.Errorf("items after reopen = %+v", items)
	}
}

func TestSecondOpenRefused(t *testing.T) {
	_, path := openTemp(t)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("second open: %v", err)
	}
}

func TestInMemory(t *testing.T) {
	db, err := Open("")
	mustDo(t, err)
	defer db.Close()
	newSession(t, db, "s", time.Now())
	if _, err := db.GetSession(ctx, "s"); err != nil {
		t.Errorf("in-memory db lost data: %v", err)
	}
}

func TestConcurrentAppends(t *testing.T) {
	db, _ := openTemp(t)
	newSession(t, db, "s", time.Now())
	const n = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	var seq int64
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			seq++
			e := protocol.Event{Seq: seq, SessionID: "s", Type: protocol.EventWarning}
			err := db.AppendEvent(ctx, e)
			mu.Unlock()
			if err == nil {
				err = db.AppendItem(ctx, "s", protocol.Item{ID: protocol.NewID("item"), Kind: protocol.ItemUserMessage, Text: string(rune('a' + i%26))})
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		mustDo(t, err)
	}
	evs, _ := db.EventsAfter(ctx, "s", 0, n)
	items, _ := db.LoadItems(ctx, "s")
	if len(evs) != n || len(items) != n {
		t.Errorf("events=%d items=%d, want %d", len(evs), len(items), n)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("seq %d at %d", e.Seq, i)
		}
	}
}

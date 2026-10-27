package session

import (
	"context"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/store"
)

func TestBusReplayAndLive(t *testing.T) {
	b := NewBus("s", 0, nil, nil)
	for range 3 {
		b.Publish(protocol.Event{Type: protocol.EventWarning})
	}
	backlog, sub, gap := b.Subscribe(1)
	defer sub.Cancel()
	if gap || len(backlog) != 2 || backlog[0].Seq != 2 || backlog[1].Seq != 3 {
		t.Fatalf("backlog = %+v gap=%v", backlog, gap)
	}
	b.Publish(protocol.Event{Type: protocol.EventWarning})
	if e := <-sub.C; e.Seq != 4 {
		t.Errorf("live seq = %d", e.Seq)
	}
	if b.LastSeq() != 4 {
		t.Errorf("LastSeq = %d", b.LastSeq())
	}
}

func TestBusGapAfterEviction(t *testing.T) {
	b := NewBus("s", 0, nil, nil)
	for range replayBuffer + 10 {
		b.Publish(protocol.Event{})
	}
	backlog, sub, gap := b.Subscribe(0)
	sub.Cancel()
	if !gap || len(backlog) != replayBuffer || backlog[0].Seq != 11 {
		t.Errorf("gap=%v len=%d first=%d", gap, len(backlog), backlog[0].Seq)
	}
	if _, sub, gap := b.Subscribe(b.LastSeq()); gap {
		t.Error("no gap expected when up to date")
	} else {
		sub.Cancel()
	}
}

func TestBusSlowSubscriberIsDropped(t *testing.T) {
	b := NewBus("s", 0, nil, nil)
	_, slow, _ := b.Subscribe(0)
	for range cap(slow.C) + 1 {
		b.Publish(protocol.Event{})
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != cap(slow.C) {
		t.Errorf("received %d before close", n)
	}
	slow.Cancel() // 幂等
}

func TestBusClose(t *testing.T) {
	b := NewBus("s", 0, nil, nil)
	_, sub, _ := b.Subscribe(0)
	b.Close()
	if _, ok := <-sub.C; ok {
		t.Error("channel not closed")
	}
	_, late, _ := b.Subscribe(0)
	if _, ok := <-late.C; ok {
		t.Error("subscription after close should be closed")
	}
}

func TestBusReplaysEvictedEventsFromLog(t *testing.T) {
	db, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(context.Background(), store.SessionRecord{ID: "s", Cwd: "/"}); err != nil {
		t.Fatal(err)
	}
	b := NewBus("s", 0, db, nil)
	for range replayBuffer + 10 {
		b.Publish(protocol.Event{SessionID: "s", Type: protocol.EventWarning})
	}
	backlog, sub, gap := b.Subscribe(3)
	sub.Cancel()
	if gap || len(backlog) != replayBuffer+7 {
		t.Fatalf("gap=%v len=%d", gap, len(backlog))
	}
	for i, e := range backlog {
		if e.Seq != int64(i+4) {
			t.Fatalf("seq %d at %d", e.Seq, i)
		}
	}

	// 新总线（模拟重启）：缓冲区为空，全部从库中读取，编号继续。
	b2 := NewBus("s", b.LastSeq(), db, nil)
	backlog, sub, gap = b2.Subscribe(replayBuffer)
	sub.Cancel()
	if gap || len(backlog) != 10 || backlog[0].Seq != replayBuffer+1 {
		t.Errorf("after restart: gap=%v len=%d", gap, len(backlog))
	}
	if e := b2.Publish(protocol.Event{SessionID: "s"}); e.Seq != replayBuffer+11 {
		t.Errorf("next seq = %d", e.Seq)
	}
}

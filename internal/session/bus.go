package session

import (
	"context"
	"log/slog"
	"sync"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// replayBuffer 是内存中为 SSE 重连保留的最近事件数量；更早的事件从
// EventLog 读取。
const replayBuffer = 4096

// EventLog 持久化事件，并按 seq 区间读回。
type EventLog interface {
	AppendEvent(ctx context.Context, e protocol.Event) error
	EventsAfter(ctx context.Context, sessionID string, after, upTo int64) ([]protocol.Event, error)
}

// Bus 为会话的事件分配序号、写入 EventLog、维护一个有界的内存重放
// 缓冲区，并将事件分发给订阅者。
type Bus struct {
	sessionID string
	log       EventLog
	logger    *slog.Logger

	mu     sync.Mutex
	seq    int64
	buf    []protocol.Event
	subs   map[*Subscription]struct{}
	closed bool
}

// NewBus 返回一个从 startSeq 之后继续编号的总线。log 为 nil 时只在内存中
// 重放。
func NewBus(sessionID string, startSeq int64, log EventLog, logger *slog.Logger) *Bus {
	if logger == nil {
		logger = slog.Default()
	}
	return &Bus{sessionID: sessionID, seq: startSeq, log: log, logger: logger, subs: make(map[*Subscription]struct{})}
}

// Subscription 接收实时事件。当订阅者落后太多或订阅被取消时，C 会被
// 关闭；随后客户端用其见过的最后一个 seq 重新连接。
type Subscription struct {
	C      chan protocol.Event
	bus    *Bus
	closed bool
}

// Publish 为 e 分配下一个 seq，先写入 EventLog，再投递给订阅者。
// 写入失败只记录日志：事件仍会实时送达，但之后无法从库中重放。
func (b *Bus) Publish(e protocol.Event) protocol.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if b.log != nil {
		if err := b.log.AppendEvent(context.Background(), e); err != nil {
			b.logger.Error("persist event", "session", b.sessionID, "seq", e.Seq, "err", err)
		}
	}
	b.buf = append(b.buf, e)
	if len(b.buf) > replayBuffer {
		b.buf = append(b.buf[:0:0], b.buf[len(b.buf)-replayBuffer:]...)
	}
	for s := range b.subs {
		select {
		case s.C <- e:
		default:
			b.dropLocked(s)
		}
	}
	return e
}

// Subscribe 返回 seq > after 的已有事件，以及用于后续事件的订阅。
// 二者不会重叠，也不会留下空隙。内存缓冲区中没有的部分从 EventLog 读取；
// 读取失败（或没有 EventLog）时 gap 为 true，客户端应重新加载快照。
func (b *Bus) Subscribe(after int64) (backlog []protocol.Event, sub *Subscription, gap bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	oldest := b.seq + 1
	if len(b.buf) > 0 {
		oldest = b.buf[0].Seq
	}
	if after+1 < oldest && after < b.seq {
		if b.log == nil {
			gap = true
		} else if evs, err := b.log.EventsAfter(context.Background(), b.sessionID, after, oldest-1); err != nil {
			b.logger.Error("load events", "session", b.sessionID, "after", after, "err", err)
			gap = true
		} else {
			backlog = evs
		}
	}
	for _, e := range b.buf {
		if e.Seq > after {
			backlog = append(backlog, e)
		}
	}
	sub = &Subscription{C: make(chan protocol.Event, 256), bus: b}
	if b.closed {
		sub.closed = true
		close(sub.C)
		return backlog, sub, gap
	}
	b.subs[sub] = struct{}{}
	return backlog, sub, gap
}

// LastSeq 返回最近一个事件的 seq。
func (b *Bus) LastSeq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Cancel 停止向 s 投递事件并关闭 s.C。
func (s *Subscription) Cancel() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	s.bus.dropLocked(s)
}

func (b *Bus) dropLocked(s *Subscription) {
	if s.closed {
		return
	}
	s.closed = true
	delete(b.subs, s)
	close(s.C)
}

// Close 结束所有订阅。
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for s := range b.subs {
		b.dropLocked(s)
	}
}

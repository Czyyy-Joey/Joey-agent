package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// events 以 SSE 流式传输会话的事件。
//
// 客户端通过 ?after= 或在自动重连时通过 Last-Event-ID 请求头传入其见过的
// 最后一个 seq。该 seq 之后已缓冲的事件会先被重放，然后实时事件随之而来，
// 既无空隙也无重复。
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.lookup(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	// EventSource 重连会保留原始 URL，因此 ?after= 保持初始值而
	// Last-Event-ID 会推进；取两者中较大者。
	after := max(parseSeq(r.Header.Get("Last-Event-ID")), parseSeq(r.URL.Query().Get("after")))

	backlog, sub, gap := sess.Bus().Subscribe(after)
	defer sub.Cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if gap {
		// 客户端错过的事件已不在缓冲区中；它应重新加载
		// 会话快照。
		fmt.Fprint(w, "event: resync\ndata: {}\n\n")
	}
	for _, e := range backlog {
		if writeEvent(w, e) != nil {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(s.opts.Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-sub.C:
			if !ok {
				return
			}
			if writeEvent(w, e) != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeEvent(w http.ResponseWriter, e protocol.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, data)
	return err
}

func parseSeq(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

package llmtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Response 是 FakeServer 的一条预设 HTTP 响应。
type Response struct {
	Status int
	Body   string
	// Body 的各个 SSE 事件之间会等待 ChunkDelay，并会响应客户端断开连接。
	ChunkDelay time.Duration
	// Hang 会在响应体之后阻塞，直到客户端断开连接。
	Hang bool
}

// RecordedRequest 是 FakeServer 收到的请求。
type RecordedRequest struct {
	Path   string
	Header http.Header
	Body   map[string]any
}

// FakeServer 是一个 httptest 服务器，它按顺序回放预设响应，
// 以此模拟 OpenAI 兼容网关。最后一条响应会重复使用。
type FakeServer struct {
	*httptest.Server
	mu        sync.Mutex
	responses []Response
	requests  []RecordedRequest
}

// NewFakeServer 启动一个服务器；调用方必须 Close 它。
func NewFakeServer(responses ...Response) *FakeServer {
	f := &FakeServer{responses: responses}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

// URL 返回包含 /v1 前缀的 base URL。
func (f *FakeServer) URL() string { return f.Server.URL + "/v1" }

// Requests 返回目前已收到的请求。
func (f *FakeServer) Requests() []RecordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]RecordedRequest(nil), f.requests...)
}

func (f *FakeServer) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)

	f.mu.Lock()
	n := len(f.requests)
	f.requests = append(f.requests, RecordedRequest{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	resp := Response{Status: http.StatusInternalServerError, Body: `{"error":{"message":"no canned response"}}`}
	if len(f.responses) > 0 {
		resp = f.responses[min(n, len(f.responses)-1)]
	}
	f.mu.Unlock()

	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status == http.StatusOK && len(resp.Body) > 5 && resp.Body[:5] == "data:" {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	flusher, _ := w.(http.Flusher)

	if resp.ChunkDelay <= 0 {
		_, _ = io.WriteString(w, resp.Body)
	} else {
		for _, ev := range splitEvents(resp.Body) {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(resp.ChunkDelay):
			}
			_, _ = io.WriteString(w, ev)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	if resp.Hang {
		<-r.Context().Done()
	}
}

// splitEvents 将 SSE 响应体拆分为多个事件，并保留每个事件末尾的空行。
func splitEvents(body string) []string {
	var out []string
	start := 0
	for i := 0; i+1 < len(body); i++ {
		if body[i] == '\n' && body[i+1] == '\n' {
			out = append(out, body[start:i+2])
			start = i + 2
			i++
		}
	}
	if start < len(body) {
		out = append(out, body[start:])
	}
	return out
}

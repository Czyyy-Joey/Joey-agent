package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/llm/llmtest"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "sse", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func streamingModel() llm.ModelInfo {
	return llm.ModelInfo{Name: "gpt-6.1-sol", Streaming: true, ToolMode: llm.ToolsNative, MaxTokensParam: "max_completion_tokens"}
}

func newTestClient(t *testing.T, srv *llmtest.FakeServer, model llm.ModelInfo, retries int) *Client {
	t.Helper()
	c, err := New(Config{
		BaseURL: srv.URL(), APIKey: "test-key", Model: model, MaxRetries: retries,
		Backoff: func(int) time.Duration { return time.Millisecond },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// collected 是一次流式输出的扁平化结果。
type collected struct {
	Text      string
	Reasoning string
	Calls     []protocol.ToolCall
	Usage     *protocol.Usage
	Finish    string
	Completed int
}

func collect(t *testing.T, c *Client, ctx context.Context) (collected, error) {
	t.Helper()
	var got collected
	for ev, err := range c.Stream(ctx, llm.Request{Items: []protocol.Item{{Kind: protocol.ItemUserMessage, Text: "hi"}}}) {
		if err != nil {
			return got, err
		}
		switch ev.Kind {
		case llm.EventTextDelta:
			got.Text += ev.Delta
		case llm.EventReasoningDelta:
			got.Reasoning += ev.Delta
		case llm.EventToolCall:
			got.Calls = append(got.Calls, *ev.Call)
		case llm.EventCompleted:
			got.Usage, got.Finish = ev.Usage, ev.FinishReason
			got.Completed++
		}
	}
	return got, nil
}

func TestStreamFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    collected
	}{
		{
			fixture: "gpt61sol_text.txt",
			want: collected{Text: "1\n2\n3\n4\n5", Finish: "stop", Completed: 1,
				Usage: &protocol.Usage{InputTokens: 398, OutputTokens: 13, TotalTokens: 411}},
		},
		{
			fixture: "gpt61sol_tool_single.txt",
			want: collected{Finish: "tool_calls", Completed: 1,
				Calls: []protocol.ToolCall{{CallID: "call_tZSfanBDi4J9KsmplbsSfSYD", Name: "read_file", Arguments: `{"path":"README.md"}`}},
				Usage: &protocol.Usage{InputTokens: 247, OutputTokens: 19, TotalTokens: 266}},
		},
		{
			fixture: "gpt61sol_tool_parallel.txt",
			want: collected{Finish: "tool_calls", Completed: 1,
				Calls: []protocol.ToolCall{
					{CallID: "call_SzU1L42gMQc8JmFPledahaOp", Name: "read_file", Arguments: `{"path":"README.md"}`},
					{CallID: "call_1rL7u68JVqD5sZLdCDRsfiMs", Name: "list_directory", Arguments: `{"path":"docs"}`},
				},
				Usage: &protocol.Usage{InputTokens: 251, OutputTokens: 49, TotalTokens: 300}},
		},
		{
			fixture: "gpt61sol_after_tool.txt",
			want: collected{Text: "The project name is **Joey-agent**.", Finish: "stop", Completed: 1,
				Usage: &protocol.Usage{InputTokens: 278, OutputTokens: 14, TotalTokens: 292}},
		},
		{
			fixture: "openai_tool_fragmented.txt",
			want: collected{Reasoning: "Need both files.", Finish: "tool_calls", Completed: 1,
				Calls: []protocol.ToolCall{
					{CallID: "call_a", Name: "read_file", Arguments: `{"path":"README.md"}`},
					{CallID: "call_b", Name: "list_directory", Arguments: `{"path":"docs"}`},
				},
				Usage: &protocol.Usage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150}},
		},
		{
			fixture: "noindex_tool.txt",
			want: collected{Finish: "tool_calls", Completed: 1,
				Calls: []protocol.ToolCall{{CallID: "call_x", Name: "read_file", Arguments: `{"path":"go.mod"}`}}},
		},
		{
			// 没有 [DONE] 的 finish_reason 仍然是完整响应。
			fixture: "length_no_done.txt",
			want:    collected{Text: "Cut off", Finish: "length", Completed: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			srv := llmtest.NewFakeServer(llmtest.Response{Body: fixture(t, tt.fixture)})
			defer srv.Close()
			got, err := collect(t, newTestClient(t, srv, streamingModel(), 0), context.Background())
			if err != nil {
				t.Fatalf("stream error: %v", err)
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
			for _, c := range got.Calls {
				if !json.Valid([]byte(c.Arguments)) {
					t.Errorf("call %s has invalid JSON arguments %q", c.CallID, c.Arguments)
				}
			}
		})
	}
}

func TestStreamTruncatedIsError(t *testing.T) {
	srv := llmtest.NewFakeServer(llmtest.Response{Body: fixture(t, "truncated.txt")})
	defer srv.Close()
	got, err := collect(t, newTestClient(t, srv, streamingModel(), 0), context.Background())
	if !errors.Is(err, llm.ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", err)
	}
	if got.Text != "Partial ans" || got.Completed != 0 {
		t.Errorf("got %+v; want partial text and no completion", got)
	}
}

func TestStreamMidstreamError(t *testing.T) {
	srv := llmtest.NewFakeServer(llmtest.Response{Body: fixture(t, "midstream_error.txt")})
	defer srv.Close()
	_, err := collect(t, newTestClient(t, srv, streamingModel(), 3), context.Background())
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "overloaded" || apiErr.RequestID != "20261026000000000000000000001" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Errorf("requests = %d; errors after streaming started must not be retried", n)
	}
}

func TestRequestShape(t *testing.T) {
	srv := llmtest.NewFakeServer(llmtest.Response{Body: fixture(t, "gpt61sol_text.txt")})
	defer srv.Close()
	model := streamingModel()
	model.MaxOutputTokens = 256
	c := newTestClient(t, srv, model, 0)
	req := llm.Request{
		Instructions: "be brief",
		Items:        []protocol.Item{{Kind: protocol.ItemUserMessage, Text: "hi"}},
		Tools:        []llm.ToolSpec{{Name: "read_file", Description: "read", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	for _, err := range c.Stream(context.Background(), req) {
		if err != nil {
			t.Fatal(err)
		}
	}
	r := srv.Requests()[0]
	if r.Path != "/v1/chat/completions" {
		t.Errorf("path = %s", r.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q", got)
	}
	b := r.Body
	if b["model"] != "gpt-6.1-sol" || b["stream"] != true || b["tool_choice"] != "auto" {
		t.Errorf("body = %v", b)
	}
	if b["max_completion_tokens"] != float64(256) {
		t.Errorf("max_completion_tokens = %v", b["max_completion_tokens"])
	}
	for _, absent := range []string{"max_tokens", "stream_options", "parallel_tool_calls", "temperature"} {
		if _, ok := b[absent]; ok {
			t.Errorf("%s must not be sent unless enabled", absent)
		}
	}
	msgs := b["messages"].([]any)
	if first := msgs[0].(map[string]any); first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("first message = %v", first)
	}
}

func TestOptionalParamsWhenEnabled(t *testing.T) {
	model := streamingModel()
	model.StreamUsage = true
	model.ParallelToolCalls = true
	model.MaxTokensParam = "max_tokens"
	model.MaxOutputTokens = 10
	body := buildBody(model, llm.Request{Tools: []llm.ToolSpec{{Name: "x"}}})
	if _, ok := body["stream_options"]; !ok {
		t.Error("stream_options missing")
	}
	if body["parallel_tool_calls"] != true {
		t.Error("parallel_tool_calls missing")
	}
	if body["max_tokens"] != 10 {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}
	model.ToolMode = llm.ToolsNone
	if _, ok := buildBody(model, llm.Request{Tools: []llm.ToolSpec{{Name: "x"}}})["tools"]; ok {
		t.Error("tools sent with tool mode none")
	}
}

func TestRetries(t *testing.T) {
	ok := llmtest.Response{Body: fixture(t, "gpt61sol_text.txt")}
	bad503 := llmtest.Response{Status: http.StatusServiceUnavailable, Body: fixture(t, "oneapi_503_bad_status.json")}
	tests := []struct {
		name      string
		responses []llmtest.Response
		retries   int
		wantReqs  int
		wantErr   int // 预期错误的 HTTP status，成功时为 0
	}{
		{"429 then success", []llmtest.Response{{Status: 429, Body: `{"error":{"message":"slow down"}}`}, ok}, 3, 2, 0},
		{"500 exhausts retries", []llmtest.Response{{Status: 500, Body: `{"error":{"message":"boom"}}`}}, 2, 3, 500},
		{"401 is not retried", []llmtest.Response{{Status: 401, Body: fixture(t, "oneapi_401.json")}}, 3, 1, 401},
		{"400 is not retried", []llmtest.Response{{Status: 400, Body: `{"error":{"message":"bad"}}`}}, 3, 1, 400},
		{"oneapi wrapped 503 retries once", []llmtest.Response{bad503}, 3, 2, 503},
		{"non-JSON error body", []llmtest.Response{{Status: 502, Body: "<html>bad gateway</html>"}}, 0, 1, 502},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := llmtest.NewFakeServer(tt.responses...)
			defer srv.Close()
			_, err := collect(t, newTestClient(t, srv, streamingModel(), tt.retries), context.Background())
			if n := len(srv.Requests()); n != tt.wantReqs {
				t.Errorf("requests = %d, want %d", n, tt.wantReqs)
			}
			if tt.wantErr == 0 {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			var apiErr *llm.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.wantErr {
				t.Fatalf("err = %v, want APIError %d", err, tt.wantErr)
			}
		})
	}
}

func TestOneAPIErrorDecoding(t *testing.T) {
	e := decodeAPIError(503, []byte(fixture(t, "oneapi_503_bad_status.json")))
	if e.Code != llm.CodeBadUpstreamStatus || e.RequestID != "2026102618400437534729353089627" {
		t.Errorf("decoded = %+v", e)
	}
	if !strings.Contains(e.Error(), "HTTP 503") {
		t.Errorf("Error() = %q", e.Error())
	}
}

func TestCancelDuringStream(t *testing.T) {
	srv := llmtest.NewFakeServer(llmtest.Response{
		Body: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n", Hang: true,
	})
	defer srv.Close()
	c := newTestClient(t, srv, streamingModel(), 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	var gotErr error
	for ev, err := range c.Stream(ctx, llm.Request{}) {
		if err != nil {
			gotErr = err
			break
		}
		if ev.Kind == llm.EventTextDelta {
			cancel()
		}
	}
	if !errors.Is(gotErr, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", gotErr)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("cancellation took %s", d)
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	srv := llmtest.NewFakeServer(llmtest.Response{Status: 500, Body: `{"error":{"message":"x"}}`})
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL(), Model: streamingModel(), MaxRetries: 5,
		Backoff: func(int) time.Duration { return time.Hour }})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := collect(t, c, ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestNonStreaming(t *testing.T) {
	srv := llmtest.NewFakeServer(llmtest.Response{Body: `{"choices":[{"index":0,"message":{"role":"assistant","content":"pong","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`})
	defer srv.Close()
	model := streamingModel()
	model.Streaming = false
	got, err := collect(t, newTestClient(t, srv, model, 0), context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := collected{Text: "pong", Finish: "tool_calls", Completed: 1,
		Calls: []protocol.ToolCall{{CallID: "c1", Name: "read_file", Arguments: "{}"}},
		Usage: &protocol.Usage{InputTokens: 3, OutputTokens: 1, TotalTokens: 4}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Error(diff)
	}
	if _, ok := srv.Requests()[0].Body["stream"]; ok {
		t.Error("stream must not be sent for non-streaming models")
	}
}

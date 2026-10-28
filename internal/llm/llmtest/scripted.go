// Package llmtest 为测试和演示提供确定性的模型 Provider。
package llmtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"sync"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// Step 是一条脚本化的模型响应。
type Step struct {
	// Text 会以 delta 的形式流式发送，按 Chunk 个 rune 切分（默认：整段文本）。
	Text  string `json:"text,omitempty"`
	Chunk int    `json:"chunk,omitempty"`
	// Reasoning 在 Text 之前流式发送。
	Reasoning string     `json:"reasoning,omitempty"`
	Calls     []StepCall `json:"calls,omitempty"`
	// 每个 delta 之前都会等待 Delay，并且会响应取消。
	Delay Duration `json:"delay,omitempty"`
	// Hang 会在流式发送文本后阻塞，直到 context 被取消。
	Hang  bool   `json:"hang,omitempty"`
	Error string `json:"error,omitempty"`
	// ErrorStatus 不为 0 时，Error 以这个 HTTP 状态的 *llm.APIError 返回，
	// 用来模拟网关错误（例如 400 上下文超限）。
	ErrorStatus int             `json:"error_status,omitempty"`
	Usage       *protocol.Usage `json:"usage,omitempty"`
}

// StepCall 是一条脚本化的工具调用。
type StepCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Duration 从诸如 "200ms" 的 Go duration 字符串反序列化。
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// ErrScriptExhausted 在到达的请求多于脚本步骤时返回。
var ErrScriptExhausted = errors.New("llmtest: script has no more steps")

// Scripted 按顺序回放各步骤，并记录收到的请求。
// 它可安全地并发使用。
type Scripted struct {
	mu       sync.Mutex
	steps    []Step
	next     int
	requests []llm.Request
	// Loop 在脚本耗尽后从第一步重新开始。
	Loop bool
}

var _ llm.Provider = (*Scripted)(nil)

// NewScripted 返回一个回放各步骤的 Provider。
func NewScripted(steps ...Step) *Scripted { return &Scripted{steps: steps} }

// LoadScript 读取形如 {"loop": bool, "steps": [...]} 的 JSON 文件。
func LoadScript(path string) (*Scripted, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Loop  bool   `json:"loop"`
		Steps []Step `json:"steps"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("llmtest: parse %s: %w", path, err)
	}
	if len(f.Steps) == 0 {
		return nil, fmt.Errorf("llmtest: %s has no steps", path)
	}
	return &Scripted{steps: f.Steps, Loop: f.Loop}, nil
}

// Requests 返回目前已收到请求的副本。
func (s *Scripted) Requests() []llm.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Request, len(s.requests))
	for i, r := range s.requests {
		r.Items = append([]protocol.Item(nil), r.Items...)
		out[i] = r
	}
	return out
}

func (s *Scripted) take(req llm.Request) (Step, int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req.Items = append([]protocol.Item(nil), req.Items...)
	s.requests = append(s.requests, req)
	if s.next >= len(s.steps) {
		if !s.Loop || len(s.steps) == 0 {
			return Step{}, 0, false
		}
		s.next = 0
	}
	n := s.next
	s.next++
	return s.steps[n], n, true
}

// Stream 实现 llm.Provider。
func (s *Scripted) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		step, n, ok := s.take(req)
		if !ok {
			yield(llm.StreamEvent{}, ErrScriptExhausted)
			return
		}
		wait := func() bool {
			if step.Delay <= 0 {
				return ctx.Err() == nil
			}
			t := time.NewTimer(time.Duration(step.Delay))
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		}
		emit := func(kind llm.EventKind, text string) bool {
			for _, part := range split(text, step.Chunk) {
				if !wait() {
					yield(llm.StreamEvent{}, ctx.Err())
					return false
				}
				if !yield(llm.StreamEvent{Kind: kind, Delta: part}, nil) {
					return false
				}
			}
			return true
		}
		if !emit(llm.EventReasoningDelta, step.Reasoning) || !emit(llm.EventTextDelta, step.Text) {
			return
		}
		if step.Hang {
			<-ctx.Done()
			yield(llm.StreamEvent{}, ctx.Err())
			return
		}
		if step.Error != "" {
			var err error = errors.New(step.Error)
			if step.ErrorStatus != 0 {
				err = &llm.APIError{StatusCode: step.ErrorStatus, Message: step.Error}
			}
			yield(llm.StreamEvent{}, err)
			return
		}
		for i, c := range step.Calls {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("call_s%d_%d", n, i)
			}
			args := string(c.Arguments)
			if args == "" {
				args = "{}"
			}
			if !yield(llm.StreamEvent{Kind: llm.EventToolCall, Call: &protocol.ToolCall{CallID: id, Name: c.Name, Arguments: args}}, nil) {
				return
			}
		}
		finish := "stop"
		if len(step.Calls) > 0 {
			finish = "tool_calls"
		}
		yield(llm.StreamEvent{Kind: llm.EventCompleted, Usage: step.Usage, FinishReason: finish}, nil)
	}
}

func split(text string, size int) []string {
	if text == "" {
		return nil
	}
	r := []rune(text)
	if size <= 0 || size >= len(r) {
		return []string{text}
	}
	var out []string
	for i := 0; i < len(r); i += size {
		out = append(out, string(r[i:min(i+size, len(r))]))
	}
	return out
}

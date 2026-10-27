package openaicompat

import (
	"encoding/json"
	"strconv"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

// chat.completion 和 chat.completion.chunk 载荷的 wire 类型。

type wireChunk struct {
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
}

type wireChoice struct {
	Index        int        `json:"index"`
	Delta        wireDelta  `json:"delta"`
	Message      *wireDelta `json:"message"`
	FinishReason *string    `json:"finish_reason"`
}

type wireDelta struct {
	Role             string              `json:"role"`
	Content          *string             `json:"content"`
	ReasoningContent string              `json:"reasoning_content"`
	Reasoning        string              `json:"reasoning"`
	ToolCalls        []wireToolCallDelta `json:"tool_calls"`
}

type wireToolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func (u *wireUsage) toUsage() *protocol.Usage {
	if u == nil {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return &protocol.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, TotalTokens: total}
}

type partialCall struct {
	id, name string
	args     []byte
}

// assembler 将 chat.completion.chunk 载荷转换为规范化的流事件。
//
// OneAPI 背后的上游各不相同：有些会把参数拆分到多个 chunk，有些
// 在一个 chunk 中发送完整的调用，还有些会省略 index 或 id。因此调用会被
// 缓冲，直到流结束后才发出。
type assembler struct {
	calls        []*partialCall
	byIndex      map[int]*partialCall
	finishReason string
	usage        *protocol.Usage
}

func newAssembler() *assembler {
	return &assembler{byIndex: make(map[int]*partialCall)}
}

// feed 处理一个 chunk，并返回其中包含的 text/reasoning delta。
func (a *assembler) feed(c *wireChunk) []llm.StreamEvent {
	var out []llm.StreamEvent
	if c.Usage != nil {
		a.usage = c.Usage.toUsage()
	}
	for _, ch := range c.Choices {
		// 只使用第一个 choice；永远不会请求 n>1。
		if ch.Index != 0 {
			continue
		}
		d := ch.Delta
		if ch.Message != nil {
			d = *ch.Message
		}
		if r := d.ReasoningContent + d.Reasoning; r != "" {
			out = append(out, llm.StreamEvent{Kind: llm.EventReasoningDelta, Delta: r})
		}
		if d.Content != nil && *d.Content != "" {
			out = append(out, llm.StreamEvent{Kind: llm.EventTextDelta, Delta: *d.Content})
		}
		for _, tc := range d.ToolCalls {
			a.addToolCallDelta(tc)
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			a.finishReason = *ch.FinishReason
		}
	}
	return out
}

func (a *assembler) addToolCallDelta(tc wireToolCallDelta) {
	var p *partialCall
	switch {
	case tc.Index != nil:
		p = a.byIndex[*tc.Index]
		if p == nil {
			p = &partialCall{}
			a.byIndex[*tc.Index] = p
			a.calls = append(a.calls, p)
		}
	case tc.ID != "":
		for _, existing := range a.calls {
			if existing.id == tc.ID {
				p = existing
				break
			}
		}
		if p == nil {
			p = &partialCall{}
			a.calls = append(a.calls, p)
		}
	case len(a.calls) > 0:
		// 既没有 index 也没有 id：视为最近一次调用的延续。
		p = a.calls[len(a.calls)-1]
	default:
		p = &partialCall{}
		a.calls = append(a.calls, p)
	}
	if p.id == "" {
		p.id = tc.ID
	}
	if p.name == "" {
		p.name = tc.Function.Name
	}
	p.args = append(p.args, tc.Function.Arguments...)
}

// sawFinish 报告是否已收到 finish_reason。
func (a *assembler) sawFinish() bool { return a.finishReason != "" }

// finish 返回组装好的工具调用，随后是 completed 事件。
func (a *assembler) finish() []llm.StreamEvent {
	out := make([]llm.StreamEvent, 0, len(a.calls)+1)
	for i, p := range a.calls {
		id := p.id
		if id == "" {
			id = "call_joey_" + strconv.Itoa(i) + "_" + protocol.NewID("gen")
		}
		args := string(p.args)
		if args == "" {
			args = "{}"
		}
		out = append(out, llm.StreamEvent{
			Kind: llm.EventToolCall,
			Call: &protocol.ToolCall{CallID: id, Name: p.name, Arguments: args},
		})
	}
	out = append(out, llm.StreamEvent{Kind: llm.EventCompleted, Usage: a.usage, FinishReason: a.finishReason})
	return out
}

// parseChunk 解码一条 SSE data 载荷。
func parseChunk(data []byte) (*wireChunk, error) {
	var c wireChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

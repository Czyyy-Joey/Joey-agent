package openaicompat

import (
	"encoding/json"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
)

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string          `json:"type"`
	Function wireFunctionDef `json:"function"`
}

type wireFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

func ptr(s string) *string { return &s }

// toMessages 将历史条目转换为 Chat Completions 消息。
//
// 连续的 assistant 侧条目（文本和工具调用）会合并为一条
// assistant 消息，因为 API 要求每个 tool 消息都跟随在
// 声明该调用的 assistant 消息之后。Reasoning 条目仅用于展示，
// 不会回传。
func toMessages(instructions string, items []protocol.Item) []wireMessage {
	msgs := make([]wireMessage, 0, len(items)+1)
	if instructions != "" {
		msgs = append(msgs, wireMessage{Role: "system", Content: ptr(instructions)})
	}
	var assistant *wireMessage
	flush := func() {
		if assistant != nil {
			msgs = append(msgs, *assistant)
			assistant = nil
		}
	}
	for _, it := range items {
		switch it.Kind {
		case protocol.ItemUserMessage, protocol.ItemTurnAborted:
			flush()
			msgs = append(msgs, wireMessage{Role: "user", Content: ptr(it.Text)})
		case protocol.ItemAgentMessage:
			if assistant == nil {
				assistant = &wireMessage{Role: "assistant"}
			}
			if assistant.Content == nil {
				assistant.Content = ptr(it.Text)
			} else {
				*assistant.Content += "\n\n" + it.Text
			}
		case protocol.ItemToolCall:
			if it.Call == nil {
				continue
			}
			if assistant == nil {
				assistant = &wireMessage{Role: "assistant"}
			}
			assistant.ToolCalls = append(assistant.ToolCalls, wireToolCall{
				ID:       it.Call.CallID,
				Type:     "function",
				Function: wireFunctionCall{Name: it.Call.Name, Arguments: it.Call.Arguments},
			})
		case protocol.ItemToolOutput:
			if it.Output == nil {
				continue
			}
			flush()
			msgs = append(msgs, wireMessage{Role: "tool", ToolCallID: it.Output.CallID, Content: ptr(it.Output.Content)})
		}
	}
	flush()
	return msgs
}

func toWireTools(specs []llm.ToolSpec) []wireTool {
	out := make([]wireTool, 0, len(specs))
	for _, s := range specs {
		params := s.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, wireTool{Type: "function", Function: wireFunctionDef{Name: s.Name, Description: s.Description, Parameters: params}})
	}
	return out
}

// buildBody 生成请求体。可选参数仅在模型被配置为接受它们时才发送，
// 因为有些上游会以错误拒绝未知参数，而不是忽略它们。
func buildBody(model llm.ModelInfo, req llm.Request) map[string]any {
	body := map[string]any{
		"model":    model.Name,
		"messages": toMessages(req.Instructions, req.Items),
	}
	if model.Streaming {
		body["stream"] = true
		if model.StreamUsage {
			body["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	if model.ToolMode == llm.ToolsNative && len(req.Tools) > 0 {
		body["tools"] = toWireTools(req.Tools)
		body["tool_choice"] = "auto"
		if model.ParallelToolCalls {
			body["parallel_tool_calls"] = true
		}
	}
	if model.MaxOutputTokens > 0 {
		param := model.MaxTokensParam
		if param == "" {
			param = "max_completion_tokens"
		}
		body[param] = model.MaxOutputTokens
	}
	return body
}

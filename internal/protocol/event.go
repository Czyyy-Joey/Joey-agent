package protocol

import "time"

// EventType 命名发送给客户端的运行时事件。
type EventType string

const (
	EventTurnStarted   EventType = "turn.started"
	EventTurnCompleted EventType = "turn.completed"
	EventItemStarted   EventType = "item.started"
	EventItemDelta     EventType = "item.delta"
	EventItemCompleted EventType = "item.completed"
	EventTokenUsage    EventType = "token.usage"
	EventWarning       EventType = "warning"
	EventError         EventType = "error"
)

// Event 是通过 SSE 流式发送给前端的事件信封。
// Seq 由会话总线分配，并在每个会话内单调递增。
type Event struct {
	Seq       int64     `json:"seq"`
	SessionID string    `json:"session_id"`
	AgentID   string    `json:"agent_id"`
	TurnID    string    `json:"turn_id,omitempty"`
	ItemID    string    `json:"item_id,omitempty"`
	Type      EventType `json:"type"`
	TS        time.Time `json:"ts"`
	Data      any       `json:"data,omitempty"`
}

// TurnStatus 是一轮的生命周期状态。
type TurnStatus string

const (
	TurnRunning     TurnStatus = "running"
	TurnCompleted   TurnStatus = "completed"
	TurnInterrupted TurnStatus = "interrupted"
	TurnFailed      TurnStatus = "failed"
	TurnMaxSteps    TurnStatus = "max_steps"
)

// ItemStatus 是条目展示给客户端时的状态。
type ItemStatus string

const (
	ItemInProgress  ItemStatus = "in_progress"
	ItemCompleted   ItemStatus = "completed"
	ItemFailed      ItemStatus = "failed"
	ItemAborted     ItemStatus = "aborted"
	ItemInterrupted ItemStatus = "interrupted"
)

// ItemData 是 item.* 事件的负载。
type ItemData struct {
	Kind       ItemKind   `json:"kind"`
	Status     ItemStatus `json:"status,omitempty"`
	Text       string     `json:"text,omitempty"`
	CallID     string     `json:"call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	Arguments  string     `json:"arguments,omitempty"`
	Output     string     `json:"output,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
}

// TurnData 是 turn.* 事件的负载。
type TurnData struct {
	Status       TurnStatus `json:"status"`
	Input        string     `json:"input,omitempty"`
	FinalMessage string     `json:"final_message,omitempty"`
	Error        string     `json:"error,omitempty"`
	Usage        *Usage     `json:"usage,omitempty"`
}

// UsageData 是 token.usage 事件的负载。
type UsageData struct {
	Last          Usage `json:"last"`
	Turn          Usage `json:"turn"`
	ContextWindow int   `json:"context_window,omitempty"`
}

// MessageData 是 warning 和 error 事件的负载。
type MessageData struct {
	Message string `json:"message"`
}

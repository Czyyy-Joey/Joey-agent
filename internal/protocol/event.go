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
	// EventApprovalRequested：工具在执行前需要用户批准，轮次在此等待。
	EventApprovalRequested EventType = "approval.requested"
	// EventApprovalResolved：审批有了结果（用户决定、超时或轮次被中断）。
	EventApprovalResolved EventType = "approval.resolved"
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
	Kind      ItemKind   `json:"kind"`
	Status    ItemStatus `json:"status,omitempty"`
	Text      string     `json:"text,omitempty"`
	CallID    string     `json:"call_id,omitempty"`
	Name      string     `json:"name,omitempty"`
	Arguments string     `json:"arguments,omitempty"`
	// Output 在 item.completed 中是完整输出；在工具调用的 item.delta 中是
	// 一段实时输出，此时 Stream 标明来自 stdout 还是 stderr。
	Output     string `json:"output,omitempty"`
	Stream     string `json:"stream,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	// Diff 是工具对文件的改动，只出现在 write_file、apply_patch 的 item.completed 中。
	Diff string `json:"diff,omitempty"`
	// Compaction 出现在 kind=compaction 的 item.started（只有 Reason）和
	// item.completed 中，不含 Replacement。compaction 的 item.delta 的 Text
	// 是摘要的一段。
	Compaction *Compaction `json:"compaction,omitempty"`
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
	// ContextTokens 是下一次请求的上下文大小：上游报告的用量加上之后新增
	// 条目的估算值；Estimated 为 true 表示完全是估算。
	ContextTokens int  `json:"context_tokens,omitempty"`
	Estimated     bool `json:"estimated,omitempty"`
	// AutoCompactLimit 是自动压缩的阈值；0 表示不自动压缩。
	AutoCompactLimit int `json:"auto_compact_limit,omitempty"`
}

// MessageData 是 warning 和 error 事件的负载。
type MessageData struct {
	Message string `json:"message"`
}

// ReviewDecision 是用户对一次审批的决定，对应 Codex 的 ReviewDecision。
type ReviewDecision string

const (
	// DecisionApproved：只允许这一次。
	DecisionApproved ReviewDecision = "approved"
	// DecisionApprovedForSession：本会话内同样的操作不再询问。
	DecisionApprovedForSession ReviewDecision = "approved_for_session"
	// DecisionDenied：拒绝，模型会收到"用户拒绝"并可以换一种做法。
	DecisionDenied ReviewDecision = "denied"
	// DecisionAbort：拒绝并中断整个轮次。
	DecisionAbort ReviewDecision = "abort"
	// DecisionTimedOut：在审批超时之前没有人回应，按拒绝处理。
	DecisionTimedOut ReviewDecision = "timed_out"
	// DecisionCancelled：等待期间轮次被中断。
	DecisionCancelled ReviewDecision = "cancelled"
)

// Approves 报告 d 是否允许执行。
func (d ReviewDecision) Approves() bool {
	return d == DecisionApproved || d == DecisionApprovedForSession
}

// ApprovalKind 是被审批的操作类型。
type ApprovalKind string

const (
	// ApprovalExec：执行一条 shell 命令。
	ApprovalExec ApprovalKind = "exec"
	// ApprovalPatch：修改文件（write_file、apply_patch）。
	ApprovalPatch ApprovalKind = "patch"
)

// ApprovalData 是 approval.* 事件的负载。approval.requested 的 ItemID 是
// 对应工具调用的条目 id。
type ApprovalData struct {
	ApprovalID string       `json:"approval_id"`
	CallID     string       `json:"call_id"`
	Tool       string       `json:"tool"`
	Kind       ApprovalKind `json:"kind"`
	// Command、Cwd 用于 exec。
	Command string `json:"command,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	// Paths、Patch 用于 patch：要修改的文件和修改内容。
	Paths []string `json:"paths,omitempty"`
	Patch string   `json:"patch,omitempty"`
	// Reason 解释为什么需要批准。
	Reason string `json:"reason,omitempty"`
	// Unsandboxed 为 true 时命令会在沙箱之外执行。
	Unsandboxed bool `json:"unsandboxed,omitempty"`
	// Decision 只出现在 approval.resolved 中。
	Decision ReviewDecision `json:"decision,omitempty"`
}

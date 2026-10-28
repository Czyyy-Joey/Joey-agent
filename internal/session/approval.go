package session

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// DefaultApprovalTimeout 是等待用户审批的默认时长。超时按拒绝处理，模型会
// 收到"没有人及时批准"，轮次继续。
const DefaultApprovalTimeout = 10 * time.Minute

var (
	// ErrApprovalNotFound 在审批 id 未知或已经有了结果时返回。
	ErrApprovalNotFound = errors.New("session: approval not found or already resolved")
	// ErrInvalidDecision 在决定不是用户可以给出的值时返回。
	ErrInvalidDecision = errors.New("session: decision must be approved, approved_for_session, denied or abort")
)

// pendingApproval 是一次正在等待的审批。
type pendingApproval struct {
	turnID string
	keys   []string
	ch     chan protocol.ReviewDecision // 容量 1，只写一次
}

// approver 是会话实现的 tools.Approver。它把请求变成 approval.requested
// 事件，然后阻塞到用户通过 Resolve 给出决定、超时或轮次结束，对应 Codex
// 的 request_command_approval + ApprovalStore（"本会话都允许"的缓存）。
type approver struct{ s *Session }

func (a approver) Approve(ctx context.Context, inv tools.Invocation, req tools.ApprovalRequest) protocol.ReviewDecision {
	s := a.s
	data := req.Data
	data.CallID, data.Tool = inv.CallID, inv.Tool

	s.mu.Lock()
	if len(req.Keys) > 0 && !slices.ContainsFunc(req.Keys, func(k string) bool { return !s.allowed[k] }) {
		s.mu.Unlock()
		return protocol.DecisionApprovedForSession
	}
	id := protocol.NewID("appr")
	data.ApprovalID = id
	p := &pendingApproval{turnID: inv.TurnID, keys: req.Keys, ch: make(chan protocol.ReviewDecision, 1)}
	s.approvals[id] = p
	s.mu.Unlock()

	s.publish(protocol.Event{Type: protocol.EventApprovalRequested, TurnID: inv.TurnID, ItemID: inv.ItemID, Data: data})

	timeout := s.m.rt.ApprovalTimeout
	if timeout <= 0 {
		timeout = DefaultApprovalTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var d protocol.ReviewDecision
	select {
	case d = <-p.ch:
	case <-ctx.Done():
		d = protocol.DecisionCancelled
	case <-timer.C:
		d = protocol.DecisionTimedOut
	}

	s.mu.Lock()
	delete(s.approvals, id)
	if d == protocol.DecisionApprovedForSession {
		for _, k := range req.Keys {
			s.allowed[k] = true
		}
	}
	at := s.active
	s.mu.Unlock()
	if d == protocol.DecisionAbort && at != nil && at.id == inv.TurnID {
		at.cancel(errInterrupted)
	}
	data.Decision = d
	s.publish(protocol.Event{Type: protocol.EventApprovalResolved, TurnID: inv.TurnID, ItemID: inv.ItemID, Data: data})
	return d
}

// Resolve 记录用户对审批 approvalID 的决定。
func (s *Session) Resolve(approvalID string, d protocol.ReviewDecision) error {
	switch d {
	case protocol.DecisionApproved, protocol.DecisionApprovedForSession, protocol.DecisionDenied, protocol.DecisionAbort:
	default:
		return ErrInvalidDecision
	}
	s.mu.Lock()
	p, ok := s.approvals[approvalID]
	if ok {
		delete(s.approvals, approvalID)
	}
	s.mu.Unlock()
	if !ok {
		return ErrApprovalNotFound
	}
	p.ch <- d
	return nil
}

// PendingApprovals 返回正在等待的审批 id。
func (s *Session) PendingApprovals() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.approvals))
	for id := range s.approvals {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

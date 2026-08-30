package teams

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// 队友之间除了纯文本，还走几种结构化消息。它们都带一个 RequestID，
// 应答回来时原样带回，Lead 才能把应答和自己发出的那条请求对上号：
// 同时向三个队友发关闭请求时，三条应答不靠 ID 是分不清谁是谁的。
const (
	// MsgText 是普通文本消息，直接拼进队友下一轮的 prompt。
	MsgText = "text"
	// MsgShutdownRequest 由 Lead 发起，请队友收工。队友可以拒绝。
	MsgShutdownRequest = "shutdown_request"
	// MsgShutdownResponse 是队友对关闭请求的答复，Approve 为 false 表示还没干完。
	MsgShutdownResponse = "shutdown_response"
	// MsgPlanApprovalRequest 由队友发起，把计划交给 Lead 审批。
	MsgPlanApprovalRequest = "plan_approval_request"
	// MsgPlanApprovalResponse 是 Lead 的审批结果，驳回时 Text 里带修改意见。
	MsgPlanApprovalResponse = "plan_approval_response"
)

// NewRequestID 生成请求标识。用随机串而不是自增序号，因为请求可能由
// 不同进程里的队友发起，自增序号跨进程会撞。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-0"
	}
	return "req-" + hex.EncodeToString(b[:])
}

// NewShutdownRequest 构造关闭请求。Text 里放原因，队友要拿它判断该不该同意。
func NewShutdownRequest(from, reason string) FileMailMessage {
	if reason == "" {
		reason = "team is wrapping up"
	}
	return newTyped(from, MsgShutdownRequest, NewRequestID(), fmt.Sprintf("[shutdown] %s", reason))
}

// NewShutdownResponse 构造队友对关闭请求的答复。
func NewShutdownResponse(from, requestID string, approve bool, reason string) FileMailMessage {
	m := newTyped(from, MsgShutdownResponse, requestID, reason)
	m.Approve = &approve
	return m
}

// NewPlanApprovalRequest 构造计划审批请求，Text 是计划全文。
func NewPlanApprovalRequest(from, plan string) FileMailMessage {
	return newTyped(from, MsgPlanApprovalRequest, NewRequestID(), plan)
}

// NewPlanApprovalResponse 构造审批结果，驳回时 feedback 说明哪里要改。
func NewPlanApprovalResponse(from, requestID string, approve bool, feedback string) FileMailMessage {
	m := newTyped(from, MsgPlanApprovalResponse, requestID, feedback)
	m.Approve = &approve
	return m
}

func newTyped(from, msgType, requestID, text string) FileMailMessage {
	m := NewFileMailMessage(from, text)
	m.Type = msgType
	m.RequestID = requestID
	return m
}

// IsShutdownRequest 判断消息是不是关闭请求。
//
// 除了看 Type，还认 "[shutdown]" 文本前缀：窗格队友是独立进程，可能是旧版本
// 启动的；而且用户手动往信箱里塞一行也该管用。
func IsShutdownRequest(m FileMailMessage) bool {
	return m.Type == MsgShutdownRequest || strings.HasPrefix(strings.TrimSpace(m.Text), ShutdownPrefix)
}

// Approved 返回应答是否为同意。字段缺省时按不同意处理，
// 宁可让 Lead 多等一轮，也不能把没表态当成点头。
func (m FileMailMessage) Approved() bool {
	return m.Approve != nil && *m.Approve
}

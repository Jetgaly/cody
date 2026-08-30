package teams

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShutdownRequestRecognized(t *testing.T) {
	// 结构化的关闭请求
	req := NewShutdownRequest(LeadName, "收工")
	if req.Type != MsgShutdownRequest {
		t.Errorf("Type = %q，期望 %q", req.Type, MsgShutdownRequest)
	}
	if req.RequestID == "" {
		t.Error("关闭请求必须带 RequestID，否则应答对不上")
	}
	if !IsShutdownRequest(req) {
		t.Error("结构化关闭请求应被识别")
	}

	// 纯文本前缀同样要认，窗格队友可能是旧版本进程
	legacy := NewFileMailMessage(LeadName, "[shutdown] stop")
	if !IsShutdownRequest(legacy) {
		t.Error("[shutdown] 文本前缀应被识别")
	}

	// 普通消息不能被误判
	normal := NewFileMailMessage(LeadName, "继续改 auth 模块")
	if IsShutdownRequest(normal) {
		t.Error("普通消息被误判成关闭请求")
	}
}

func TestShutdownResponseCarriesDecision(t *testing.T) {
	req := NewShutdownRequest(LeadName, "收工")

	yes := NewShutdownResponse("alice", req.RequestID, true, "done")
	if !yes.Approved() {
		t.Error("同意的应答 Approved() 应为 true")
	}
	if yes.RequestID != req.RequestID {
		t.Errorf("应答的 RequestID = %q，应原样带回 %q", yes.RequestID, req.RequestID)
	}

	no := NewShutdownResponse("alice", req.RequestID, false, "还在跑测试")
	if no.Approved() {
		t.Error("拒绝的应答 Approved() 应为 false")
	}

	// 没表态时按不拒绝处理，不能当成点头
	silent := NewFileMailMessage("alice", "")
	if silent.Approved() {
		t.Error("没有 Approve 字段时不应视为同意")
	}
}

func TestPlanApprovalRoundTrip(t *testing.T) {
	req := NewPlanApprovalRequest("alice", "1. 先读 auth 包\n2. 抽出接口")
	if req.Type != MsgPlanApprovalRequest || req.RequestID == "" {
		t.Fatalf("计划请求构造有误：%+v", req)
	}
	if !strings.Contains(req.Text, "抽出接口") {
		t.Error("计划全文应放在 Text 里")
	}

	rej := NewPlanApprovalResponse(LeadName, req.RequestID, false, "别动 handler 层")
	if rej.Approved() {
		t.Error("驳回的批复不应为同意")
	}
	if rej.Text != "别动 handler 层" {
		t.Errorf("驳回意见应放在 Text 里，实际 %q", rej.Text)
	}
	if rej.RequestID != req.RequestID {
		t.Error("批复必须带回原请求的 RequestID")
	}
}

func TestTypedFieldsSurviveJSON(t *testing.T) {
	// 邮箱是落盘的，字段必须能原样穿过一次序列化
	req := NewShutdownRequest(LeadName, "收工")
	resp := NewShutdownResponse("alice", req.RequestID, false, "还没跑完")

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	var got FileMailMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if got.Type != MsgShutdownResponse || got.RequestID != req.RequestID {
		t.Errorf("类型或请求 ID 没穿过序列化：%+v", got)
	}
	if got.Approve == nil || *got.Approve {
		t.Errorf("Approve=false 没穿过序列化：%+v", got.Approve)
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewRequestID()
		if seen[id] {
			t.Fatalf("请求 ID 撞了：%s", id)
		}
		seen[id] = true
	}
}

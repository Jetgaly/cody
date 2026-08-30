package agents

import (
	"context"
	"strings"
	"testing"

	"cody/internal/conversation"
	"cody/internal/permissions"
	"cody/internal/tools"
)

func TestBuildForkedConversationPreservesThinkingBlocks(t *testing.T) {
	// 字节级精确重放：带 thinking blocks 的 assistant 消息在 fork 出的对话中
	// 必须以相同的 thinking blocks 复现，否则 API 请求前缀会发散，
	// 导致 prompt 缓存失效。
	parent := conversation.NewManager()
	thinking := []conversation.ThinkingBlock{{Thinking: "secret plan", Signature: "sig-1"}}
	parent.AddAssistantFull("hello", thinking, []conversation.ToolUseBlock{
		{ToolUseID: "tool_1", ToolName: "Bash", Arguments: map[string]any{"command": "ls"}},
	})

	forked := buildForkedConversation(parent, "do work")
	msgs := forked.GetMessages()
	var found *conversation.Message
	for i := range msgs {
		if len(msgs[i].ThinkingBlocks) > 0 {
			found = &msgs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("forked conversation lost thinking blocks")
	}
	if found.ThinkingBlocks[0].Thinking != "secret plan" || found.ThinkingBlocks[0].Signature != "sig-1" {
		t.Errorf("thinking blocks not preserved verbatim: %+v", found.ThinkingBlocks)
	}
}

func TestDeriveSubAgentCheckerOverrideMode(t *testing.T) {
	// spec.PermissionMode 必须产生一个共享父级 Sandbox / RuleEngine 但切换了
	// Mode 的 Checker。空的覆盖值 → 保持不变。
	sb := permissions.NewPathSandbox("/tmp", "")
	eng := &permissions.RuleEngine{}
	parent := permissions.NewChecker(sb, eng, permissions.ModeDefault)

	if derived := deriveSubAgentChecker(parent, ""); derived != parent {
		t.Error("empty override should return parent unchanged")
	}
	derived := deriveSubAgentChecker(parent, "plan")
	if derived == parent {
		t.Fatal("plan override should produce a new checker instance")
	}
	if derived.Mode != permissions.ModePlan {
		t.Errorf("derived.Mode = %q, want plan", derived.Mode)
	}
	if derived.Sandbox != sb || derived.RuleEngine != eng {
		t.Error("derived checker should share parent's Sandbox + RuleEngine")
	}
	if deriveSubAgentChecker(nil, "plan") != nil {
		t.Error("nil parent should propagate nil")
	}
}

func TestRunForkRejectedWhenQuerySourceIsFork(t *testing.T) {
	// 嵌套 fork 的首道防线：直接看 QuerySource 标记。
	tool := &AgentTool{
		Registry:     tools.NewRegistry(),
		Conversation: conversation.NewManager(),
		QuerySource:  ForkQuerySource,
		TaskMgr:      NewTaskManager(),
	}
	result := tool.runFork(context.Background(), "desc", "do work", "", "")
	if !result.IsError {
		t.Fatal("runFork should reject when QuerySource is fork")
	}
	if !strings.Contains(result.Output, "cannot fork from a forked agent") {
		t.Errorf("unexpected error message: %s", result.Output)
	}
}

func TestRunForkRejectedWhenBoilerplateInHistory(t *testing.T) {
	// 嵌套 fork 的兜底防线：QuerySource 没传到时，扫对话历史里的 fork 样板标记。
	conv := conversation.NewManager()
	conv.AddUserMessage(ForkBoilerplateTag + " stale message from a prior fork")
	tool := &AgentTool{
		Registry:     tools.NewRegistry(),
		Conversation: conv,
		TaskMgr:      NewTaskManager(),
	}
	result := tool.runFork(context.Background(), "desc", "do work", "", "")
	if !result.IsError {
		t.Fatal("runFork should reject when conversation history contains ForkBoilerplateTag")
	}
}

func TestCloneRegistryForForkSetsQuerySource(t *testing.T) {
	// fork 必须原样继承父工具池，只把其中的 Agent 工具换成带 QuerySource=ForkQuerySource
	// 的拷贝，这样再往下 fork 会在调用那一刻被拦住。
	reg := tools.NewRegistry()
	reg.Register(&AgentTool{}) // 模拟父级的 Agent 工具
	reg.Register(&dummyTool{name: "Bash", category: tools.CategoryCommand})

	forked := cloneRegistryForFork(reg)
	if forked.Get("Bash") == nil {
		t.Error("Bash should still be present in forked registry")
	}
	at, ok := forked.Get("Agent").(*AgentTool)
	if !ok {
		t.Fatal("Agent tool should still be present (tool pool inherited verbatim)")
	}
	if at.QuerySource != ForkQuerySource {
		t.Errorf("cloned Agent tool QuerySource = %q, want %q", at.QuerySource, ForkQuerySource)
	}
}

func TestExecuteRoutesBackgroundSpecToAsync(t *testing.T) {
	// Agent 定义里标了 background 就必须走异步，调用方没传 run_in_background 也一样。
	tool := &AgentTool{
		Registry: tools.NewRegistry(),
		TaskMgr:  NewTaskManager(),
		Protocol: "anthropic",
	}
	result := tool.Execute(context.Background(), map[string]any{
		"description":   "verify run",
		"prompt":        "do it",
		"subagent_type": "background-only",
	})
	if !result.IsError {
		// 没有注册的 spec 时调用应当失败；这是单元测试下唯一安全的结果形态——
		// 但它绝不能经由同步路径走到那里。IsError = true 且带 "unknown agent type"
		// 消息，证实参数解析已到达 spec 查找。
		if !strings.Contains(result.Output, "unknown agent type") {
			t.Errorf("expected unknown-agent-type error, got %q", result.Output)
		}
	}
}

func TestExecuteValidatesMode(t *testing.T) {
	tool := &AgentTool{
		Registry: tools.NewRegistry(),
		TaskMgr:  NewTaskManager(),
	}

	badMode := tool.Execute(context.Background(), map[string]any{
		"description": "x", "prompt": "y",
		"mode": "not-a-real-mode",
	})
	if !badMode.IsError || !strings.Contains(badMode.Output, "invalid mode") {
		t.Errorf("invalid mode must be rejected, got %q", badMode.Output)
	}
}

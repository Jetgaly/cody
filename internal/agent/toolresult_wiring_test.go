package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/toolresult"
	"cody/internal/tools"
)

// 这些测试驱动完整的 Agent 主循环，验证工具结果预算在入历史时的接线：
// 单条溢写、聚合溢写、回读豁免，以及进入对话历史的内容就是最终形态。

func findToolResultsMsg(t *testing.T, conv *conversation.Manager) conversation.Message {
	t.Helper()
	for _, m := range conv.GetMessages() {
		if len(m.ToolResults) > 0 {
			return m
		}
	}
	t.Fatal("no tool-results message in conversation")
	return conversation.Message{}
}

func TestIngestSpillsSingleOversizedResult(t *testing.T) {
	big := strings.Repeat("x", 60000)
	client := &mockClient{responses: [][]llm.StreamEvent{
		{
			llm.ToolCallStart{ToolName: "BigTool", ToolID: "t1"},
			llm.ToolCallComplete{ToolID: "t1", ToolName: "BigTool", Arguments: map[string]any{}},
			llm.StreamEnd{StopReason: "tool_use"},
		},
		{
			llm.TextDelta{Text: "done"},
			llm.StreamEnd{StopReason: "end_turn"},
		},
	}}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "BigTool", result: big})
	ag := New(client, reg, "anthropic")
	ag.WorkDir = t.TempDir()
	conv := conversation.NewManager()

	_, events := runConversationRound(ag, conv, "go")

	// 进历史的内容是预览，不是原文
	msg := findToolResultsMsg(t, conv)
	got := msg.ToolResults[0].Content
	if !strings.HasPrefix(got, "<persisted-output>") {
		t.Fatalf("history content should be a preview, got %d chars: %q...", len(got), got[:40])
	}
	// 溢写文件保存了完整原文
	fi, err := os.Stat(filepath.Join(toolresult.SpillDir(ag.WorkDir, ""), "t1.txt"))
	if err != nil {
		t.Fatalf("spill file missing: %v", err)
	}
	if fi.Size() != 60000 {
		t.Fatalf("spill file size = %d, want 60000", fi.Size())
	}
	// UI 事件携带原始输出
	trs := getToolResults(events)
	if len(trs) != 1 || len(trs[0].Output) != 60000 {
		t.Fatalf("UI event should carry raw output, got %d results", len(trs))
	}
}

func TestIngestReadbackExempt(t *testing.T) {
	ag0WorkDir := t.TempDir()
	readbackPath := filepath.Join(toolresult.SpillDir(ag0WorkDir, ""), "toolu_old.txt")
	big := strings.Repeat("y", 60000)

	client := &mockClient{responses: [][]llm.StreamEvent{
		{
			llm.ToolCallStart{ToolName: "ReadFile", ToolID: "t_rb"},
			llm.ToolCallComplete{ToolID: "t_rb", ToolName: "ReadFile", Arguments: map[string]any{"file_path": readbackPath}},
			llm.StreamEnd{StopReason: "tool_use"},
		},
		{
			llm.TextDelta{Text: "done"},
			llm.StreamEnd{StopReason: "end_turn"},
		},
	}}
	reg := tools.NewRegistry()
	reg.Register(&mockTool{name: "ReadFile", result: big})
	ag := New(client, reg, "anthropic")
	ag.WorkDir = ag0WorkDir
	conv := conversation.NewManager()

	runConversationRound(ag, conv, "read it back")

	// 回读结果豁免溢写：原文进历史
	msg := findToolResultsMsg(t, conv)
	if got := msg.ToolResults[0].Content; len(got) != 60000 {
		t.Fatalf("readback should stay raw, got %d chars", len(got))
	}
	// 没有为回读结果生成新的溢写文件
	if _, err := os.Stat(filepath.Join(toolresult.SpillDir(ag0WorkDir, ""), "t_rb.txt")); !os.IsNotExist(err) {
		t.Fatal("readback result must not be spilled")
	}
}

func TestIngestAggregateSpillsLargest(t *testing.T) {
	// 5 个并行工具各 ~45K，单条都不超限，合计 225K 超聚合线，
	// 只有最大的 t3 被溢写。
	sizes := map[string]int{"T1": 45000, "T2": 45000, "T3": 45001, "T4": 45000, "T5": 45000}
	var first []llm.StreamEvent
	reg := tools.NewRegistry()
	for _, name := range []string{"T1", "T2", "T3", "T4", "T5"} {
		id := "t" + strings.ToLower(name[1:])
		first = append(first,
			llm.ToolCallStart{ToolName: name, ToolID: id},
			llm.ToolCallComplete{ToolID: id, ToolName: name, Arguments: map[string]any{}},
		)
		reg.Register(&mockTool{name: name, result: strings.Repeat("z", sizes[name])})
	}
	first = append(first, llm.StreamEnd{StopReason: "tool_use"})

	client := &mockClient{responses: [][]llm.StreamEvent{
		first,
		{llm.TextDelta{Text: "done"}, llm.StreamEnd{StopReason: "end_turn"}},
	}}
	ag := New(client, reg, "anthropic")
	ag.WorkDir = t.TempDir()
	conv := conversation.NewManager()

	runConversationRound(ag, conv, "fan out")

	msg := findToolResultsMsg(t, conv)
	total := 0
	previews := 0
	var t3Content string
	for _, tr := range msg.ToolResults {
		total += len(tr.Content)
		if strings.HasPrefix(tr.Content, "<persisted-output>") {
			previews++
		}
		if tr.ToolUseID == "t3" {
			t3Content = tr.Content
		}
	}
	if total > 200000 {
		t.Fatalf("aggregate %d still over limit", total)
	}
	if previews != 1 {
		t.Fatalf("expected exactly 1 preview, got %d", previews)
	}
	if !strings.HasPrefix(t3Content, "<persisted-output>") {
		t.Fatal("largest result t3 should be the one spilled")
	}
}

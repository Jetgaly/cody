package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cody/internal/conversation"
)

func TestNewID(t *testing.T) {
	id := NewID()
	if len(id) != 20 { // 20060102-150405-xxxx
		t.Fatalf("unexpected ID format: %s (len=%d)", id, len(id))
	}
	// 同秒生成两个 ID 不应相同
	id2 := NewID()
	if id == id2 {
		t.Fatalf("two IDs generated in same second collided: %s", id)
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	sid := "test-session"

	SaveMessage(dir, sid, Message{Role: "user", Content: "hello", Ts: 1})
	SaveMessage(dir, sid, Message{Role: "assistant", Content: "hi", Ts: 2})

	msgs := LoadSession(dir, sid)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Fatalf("unexpected first message: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "hi" {
		t.Fatalf("unexpected second message: %+v", msgs[1])
	}
}

func TestLoadEmpty(t *testing.T) {
	dir := t.TempDir()
	msgs := LoadSession(dir, "nonexistent")
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages, got %d", len(msgs))
	}
}

func TestListSessions(t *testing.T) {
	dir := t.TempDir()

	SaveMessage(dir, "s1", Message{Role: "user", Content: "first session", Ts: 1})
	SaveMessage(dir, "s2", Message{Role: "user", Content: "second session", Ts: 2})
	SaveMessage(dir, "s2", Message{Role: "assistant", Content: "reply", Ts: 3})

	sessions := ListSessions(dir)
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	found := map[string]bool{}
	for _, s := range sessions {
		found[s.ID] = true
		if s.ID == "s2" && s.MessageCount != 2 {
			t.Fatalf("expected 2 messages in s2, got %d", s.MessageCount)
		}
	}
	if !found["s1"] || !found["s2"] {
		t.Fatalf("missing sessions: %v", sessions)
	}
}

func TestFileCreated(t *testing.T) {
	dir := t.TempDir()
	SaveMessage(dir, "test", Message{Role: "user", Content: "hi", Ts: 1})

	path := filepath.Join(dir, ".cody", "sessions", "test.jsonl")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("session file was not created")
	}
}

func TestFormatRelativeTime(t *testing.T) {
	now := time.Now()
	if got := FormatRelativeTime(now.Add(-30 * time.Second)); got != "just now" {
		t.Fatalf("expected 'just now', got %s", got)
	}
	if got := FormatRelativeTime(now.Add(-5 * time.Minute)); got != "5 minutes ago" {
		t.Fatalf("expected '5 minutes ago', got %s", got)
	}
	if got := FormatRelativeTime(now.Add(-3 * time.Hour)); got != "3 hours ago" {
		t.Fatalf("expected '3 hours ago', got %s", got)
	}
}

func TestFormatFileSize(t *testing.T) {
	if got := FormatFileSize(500); got != "500B" {
		t.Fatalf("expected '500B', got %s", got)
	}
	if got := FormatFileSize(53862); got != "52.6KB" {
		t.Fatalf("expected '52.6KB', got %s", got)
	}
}

// 包含 compact_boundary 的会话在恢复时必须重建到「已压缩」状态：
// 即边界的 summary + 内联的保留尾部 + 边界之后追加的任何普通消息 ——
// 而写在边界之前的原始压缩前前缀不会被重放。
func TestFindLastCompactBoundary_RebuildsCompactedState(t *testing.T) {
	dir := t.TempDir()
	sid := "compacted-session"

	// 原始压缩前前缀（边界之后绝不能重放）。
	SaveMessage(dir, sid, Message{Role: "user", Content: "ORIGINAL-PREFIX-1", Ts: 1})
	SaveMessage(dir, sid, Message{Role: "assistant", Content: "ORIGINAL-PREFIX-2", Ts: 2})
	SaveMessage(dir, sid, Message{Role: "user", Content: "ORIGINAL-PREFIX-3", Ts: 3})

	// 压缩触发：写入一个边界，内联 summary + 保留尾部。
	keep := []KeepMessage{
		{Role: "user", Content: "KEPT-TAIL-USER"},
		{Role: "assistant", Content: "KEPT-TAIL-ASSISTANT"},
	}
	SaveCompactBoundary(dir, sid, "THE-SUMMARY", keep)

	// 边界之后的延续消息（必须被重放）。
	SaveMessage(dir, sid, Message{Role: "user", Content: "AFTER-BOUNDARY-USER", Ts: 5})
	SaveMessage(dir, sid, Message{Role: "assistant", Content: "AFTER-BOUNDARY-ASSISTANT", Ts: 6})

	msgs := LoadSession(dir, sid)

	boundary, after, ok := FindLastCompactBoundary(msgs)
	if !ok {
		t.Fatalf("expected a compact boundary to be found")
	}
	if boundary.Summary != "THE-SUMMARY" {
		t.Fatalf("summary mismatch: got %q", boundary.Summary)
	}
	// 保留尾部（边界内联）必须带原始 role + content 原样往返。
	if len(boundary.Keep) != 2 ||
		boundary.Keep[0].Role != "user" || boundary.Keep[0].Content != "KEPT-TAIL-USER" ||
		boundary.Keep[1].Role != "assistant" || boundary.Keep[1].Content != "KEPT-TAIL-ASSISTANT" {
		t.Fatalf("kept tail not round-tripped: %+v", boundary.Keep)
	}
	// 边界之后的消息存在且有序；原始前缀不存在。
	if len(after) != 2 {
		t.Fatalf("expected 2 after-boundary messages, got %d: %+v", len(after), after)
	}
	if after[0].Content != "AFTER-BOUNDARY-USER" || after[1].Content != "AFTER-BOUNDARY-ASSISTANT" {
		t.Fatalf("after-boundary content mismatch: %+v", after)
	}
	for _, m := range after {
		if strings.Contains(m.Content, "ORIGINAL-PREFIX") {
			t.Fatalf("original pre-compaction prefix must not appear after the boundary: %q", m.Content)
		}
	}

	// 模拟 TUI 执行的恢复重建，并断言最终重建出的
	// 对话：[summary] + keep + after，不含原始前缀。
	var rebuilt []Message
	rebuilt = append(rebuilt, Message{Role: "user", Content: boundary.Summary})
	for _, k := range boundary.Keep {
		rebuilt = append(rebuilt, Message{Role: k.Role, Content: k.Content})
	}
	rebuilt = append(rebuilt, after...)

	wantOrder := []string{
		"THE-SUMMARY", "KEPT-TAIL-USER", "KEPT-TAIL-ASSISTANT",
		"AFTER-BOUNDARY-USER", "AFTER-BOUNDARY-ASSISTANT",
	}
	if len(rebuilt) != len(wantOrder) {
		t.Fatalf("rebuilt length %d != expected %d: %+v", len(rebuilt), len(wantOrder), rebuilt)
	}
	for i, want := range wantOrder {
		if rebuilt[i].Content != want {
			t.Fatalf("rebuilt[%d] = %q, want %q", i, rebuilt[i].Content, want)
		}
	}
	for _, m := range rebuilt {
		if strings.Contains(m.Content, "ORIGINAL-PREFIX") {
			t.Fatalf("original prefix leaked into rebuilt conversation: %q", m.Content)
		}
	}
}

// 最后一个边界胜出：压缩过两次的会话必须从最近一次
// 边界重建，两个边界之间的消息不得重放。
func TestFindLastCompactBoundary_UsesLastBoundary(t *testing.T) {
	dir := t.TempDir()
	sid := "twice-compacted"

	SaveMessage(dir, sid, Message{Role: "user", Content: "GEN0", Ts: 1})
	SaveCompactBoundary(dir, sid, "SUMMARY-1", []KeepMessage{{Role: "user", Content: "KEEP-1"}})
	SaveMessage(dir, sid, Message{Role: "assistant", Content: "BETWEEN-BOUNDARIES", Ts: 3})
	SaveCompactBoundary(dir, sid, "SUMMARY-2", []KeepMessage{{Role: "assistant", Content: "KEEP-2"}})
	SaveMessage(dir, sid, Message{Role: "user", Content: "NEWEST", Ts: 5})

	msgs := LoadSession(dir, sid)
	boundary, after, ok := FindLastCompactBoundary(msgs)
	if !ok {
		t.Fatalf("expected a boundary")
	}
	if boundary.Summary != "SUMMARY-2" {
		t.Fatalf("expected last boundary SUMMARY-2, got %q", boundary.Summary)
	}
	if len(boundary.Keep) != 1 || boundary.Keep[0].Content != "KEEP-2" {
		t.Fatalf("expected KEEP-2, got %+v", boundary.Keep)
	}
	if len(after) != 1 || after[0].Content != "NEWEST" {
		t.Fatalf("expected only NEWEST after last boundary, got %+v", after)
	}
}

// 向后兼容：没有任何边界的会话（旧格式）必须
// 返回 ok=false，让调用方逐字重放每条消息。
func TestFindLastCompactBoundary_NoBoundaryFullReplay(t *testing.T) {
	dir := t.TempDir()
	sid := "legacy-session"

	SaveMessage(dir, sid, Message{Role: "user", Content: "hello", Ts: 1})
	SaveMessage(dir, sid, Message{Role: "assistant", Content: "hi", Ts: 2})
	SaveMessage(dir, sid, Message{Role: "user", Content: "again", Ts: 3})

	msgs := LoadSession(dir, sid)
	_, _, ok := FindLastCompactBoundary(msgs)
	if ok {
		t.Fatalf("legacy session must report no boundary so caller does a full replay")
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages preserved for full replay, got %d", len(msgs))
	}
}

func TestMatchesSearch(t *testing.T) {
	s := SessionInfo{FirstMessage: "Hello World", ID: "test-123"}
	if !MatchesSearch(s, "hello") {
		t.Fatal("should match case-insensitive")
	}
	if !MatchesSearch(s, "") {
		t.Fatal("empty query should match all")
	}
	if MatchesSearch(s, "zzz") {
		t.Fatal("should not match unrelated query")
	}
}

// 工具块要能完整走一遍「落盘 → 读回 → 还原成对话消息」。
func TestToolBlocksRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id := "tools"

	assistant := conversation.Message{
		Role:    "assistant",
		Content: "先看看这个文件",
		ToolUses: []conversation.ToolUseBlock{{
			ToolUseID: "toolu_1",
			ToolName:  "ReadFile",
			Arguments: map[string]any{"file_path": "main.go"},
		}},
	}
	toolResult := conversation.Message{
		Role: "user",
		ToolResults: []conversation.ToolResultBlock{{
			ToolUseID: "toolu_1",
			Content:   "package main",
		}},
	}

	SaveMessage(dir, id, FromConversation(assistant))
	SaveMessage(dir, id, FromConversation(toolResult))

	loaded := LoadSession(dir, id)
	if len(loaded) != 2 {
		t.Fatalf("expected 2 records, got %d", len(loaded))
	}

	gotAssistant := loaded[0].ToConversation()
	if len(gotAssistant.ToolUses) != 1 {
		t.Fatalf("tool_use lost, got %+v", gotAssistant)
	}
	if gotAssistant.ToolUses[0].ToolName != "ReadFile" {
		t.Errorf("tool name = %q, want ReadFile", gotAssistant.ToolUses[0].ToolName)
	}
	if gotAssistant.ToolUses[0].Arguments["file_path"] != "main.go" {
		t.Errorf("arguments lost: %+v", gotAssistant.ToolUses[0].Arguments)
	}

	gotResult := loaded[1].ToConversation()
	if len(gotResult.ToolResults) != 1 {
		t.Fatalf("tool_result lost, got %+v", gotResult)
	}
	if gotResult.ToolResults[0].ToolUseID != "toolu_1" {
		t.Errorf("pairing id = %q, want toolu_1", gotResult.ToolResults[0].ToolUseID)
	}
}

// 只带工具结果的消息本身没有文本，不能被按空内容过滤掉。
func TestLoadKeepsEmptyContentToolResult(t *testing.T) {
	dir := t.TempDir()
	id := "empty-content"

	SaveMessage(dir, id, Message{
		Role:        "user",
		ToolResults: []ToolResultRecord{{ToolUseID: "toolu_9", Content: "ok"}},
	})

	loaded := LoadSession(dir, id)
	if len(loaded) != 1 {
		t.Fatalf("tool-result-only record was dropped, got %d records", len(loaded))
	}
}

// 不含工具字段的旧会话文件要照常读出。
func TestLoadLegacyRecordsWithoutToolFields(t *testing.T) {
	dir := t.TempDir()
	id := "legacy"
	path := SessionFilePath(dir, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"role":"user","content":"hi","ts":1}
{"role":"assistant","content":"hello","ts":2}
`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := LoadSession(dir, id)
	if len(loaded) != 2 {
		t.Fatalf("expected 2 legacy records, got %d", len(loaded))
	}
	if loaded[0].Content != "hi" || len(loaded[0].ToolUses) != 0 {
		t.Errorf("legacy record parsed wrong: %+v", loaded[0])
	}
}

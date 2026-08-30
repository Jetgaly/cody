package compact

import (
	"context"
	"strings"
	"testing"

	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/session"
)

// stubSummaryClient 实现 llm.Client 并流式返回一个固定的 <summary> 块，
// 使得 autoCompact 的摘要步骤在测试中是确定性的。它会记录被要求摘要的
// prompt，以便测试断言只有前缀被摘要——而不是保留的尾部。
type stubSummaryClient struct {
	summary      string
	lastPrompt   string
	streamCalled bool
}

func (c *stubSummaryClient) SetSystemPrompt(prompt string) {}

func (c *stubSummaryClient) Stream(ctx context.Context, conv *conversation.Manager, tools []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	c.streamCalled = true
	if msgs := conv.GetMessages(); len(msgs) > 0 {
		c.lastPrompt = msgs[len(msgs)-1].Content
	}
	ch := make(chan llm.StreamEvent, 4)
	errCh := make(chan error, 1)
	ch <- llm.TextDelta{Text: "<summary>" + c.summary + "</summary>"}
	ch <- llm.StreamEnd{StopReason: "end_turn"}
	close(ch)
	errCh <- nil
	close(errCh)
	return ch, errCh
}

// 第一层（offload + snip）的测试已迁移到 internal/toolresult/budget_test.go，
// 实现也移到了那里。compact 现在只负责第二层（autoCompact）
// 以及 formatCompactSummary 辅助函数，因此本文件覆盖这些内容。

func TestFormatCompactSummary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "both blocks present",
			in:   "<analysis>scratch thoughts</analysis>\n<summary>final text</summary>",
			want: "final text",
		},
		{
			name: "summary block unterminated",
			in:   "<analysis>scratch</analysis>\n<summary>tail with no close tag",
			want: "tail with no close tag",
		},
		{
			name: "only analysis block — drop it",
			in:   "prefix <analysis>scratch</analysis> suffix",
			want: "prefix  suffix",
		},
		{
			name: "neither block — return raw",
			in:   "plain text response",
			want: "plain text response",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCompactSummary(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// EstimateTokens 覆盖所有内容来源，且空输入时不会崩溃。
func TestEstimateTokensZeroAndPopulated(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Errorf("empty input should be 0 tokens, got %d", got)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage(strings.Repeat("x", 700))
	got := EstimateTokens(conv.GetMessages())
	if got < 150 || got > 250 {
		t.Errorf("700-char message should estimate ~200 tokens, got %d", got)
	}
}

// BaselineFromUsage 必须对四个真实 token 计数器求和，这样锚点才能反映
// 真实的 prompt+output 大小，即使在缓存命中占主导时也是如此（input 很小，
// cache_read 很大）。
func TestBaselineFromUsage(t *testing.T) {
	u := llm.UsageInfo{
		InputTokens:         100,
		OutputTokens:        40,
		CacheReadTokens:     5000,
		CacheCreationTokens: 200,
	}
	if got, want := BaselineFromUsage(u), 5340; got != want {
		t.Errorf("BaselineFromUsage = %d, want %d", got, want)
	}
	// 零 usage（兼容端点不报告任何数据）→ 零基线，这样
	// 调用方就知道不要把它当作锚点。
	if got := BaselineFromUsage(llm.UsageInfo{}); got != 0 {
		t.Errorf("empty usage baseline = %d, want 0", got)
	}
}

// ComputeUsedTokens：没有锚点（冷启动 / 第一轮）时必须回退到
// 对所有消息做完整的字符估算，与 EstimateTokens 一致。
func TestComputeUsedTokensColdStartFallback(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage(strings.Repeat("x", 700))
	conv.AddAssistantMessage(strings.Repeat("y", 700))
	msgs := conv.GetMessages()

	got := ComputeUsedTokens(msgs, UsageAnchor{}) // HasUsage == false
	want := EstimateTokens(msgs)
	if got != want {
		t.Errorf("cold-start ComputeUsedTokens = %d, want full estimate %d", got, want)
	}
}

// ComputeUsedTokens：有锚点时，它必须返回 baseline 加上对 anchorCount 之后
// 新增消息的估算——而不是对整个会话重新估算。这就是缓存命中的收益：
// 真实输入远小于锚定前缀的字符数。
func TestComputeUsedTokensWithAnchorIncremental(t *testing.T) {
	conv := conversation.NewManager()
	// 3 条大型锚定消息：它们的真实 token 成本由 baseline 记录，
	// 而不是由它们的字符数记录。
	conv.AddUserMessage(strings.Repeat("x", 7000))
	conv.AddAssistantMessage(strings.Repeat("y", 7000))
	conv.AddUserMessage(strings.Repeat("z", 7000))
	anchorCount := conv.Len()
	// 锚点之后追加一条小消息。
	conv.AddAssistantMessage(strings.Repeat("w", 350))
	msgs := conv.GetMessages()

	const baseline = 1500 // 假设真实 API 报告前缀花费了 1500 个 token
	anchor := UsageAnchor{BaselineTokens: baseline, AnchorCount: anchorCount, HasUsage: true}

	got := ComputeUsedTokens(msgs, anchor)
	wantIncrement := EstimateTokens(msgs[anchorCount:])
	if got != baseline+wantIncrement {
		t.Errorf("anchored ComputeUsedTokens = %d, want baseline+increment %d", got, baseline+wantIncrement)
	}
	// 合理性检查：增量结果必须远低于对整个（缓存密集的）会话的完整字符估算，
	// 证明我们没有重新估算前缀。
	if full := EstimateTokens(msgs); got >= full {
		t.Errorf("anchored result %d should be below full estimate %d", got, full)
	}
}

// ComputeUsedTokens：过期的锚点（AnchorCount 超过当前消息数，
// 例如压缩将会话回滚之后）不得 panic，并且应该回退到完整估算。
func TestComputeUsedTokensStaleAnchorClamp(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage("hi")
	msgs := conv.GetMessages()

	anchor := UsageAnchor{BaselineTokens: 9999, AnchorCount: 50, HasUsage: true}
	got := ComputeUsedTokens(msgs, anchor)
	if want := EstimateTokens(msgs); got != want {
		t.Errorf("stale-anchor ComputeUsedTokens = %d, want full estimate %d", got, want)
	}
}

// bigMsg 返回一条内容单独估算约为 `tokens` 个 token 的消息
// （recoveryCharsPerToken ≈ 3.5 字符/token），这样测试就能确定性地驱动
// keepRecentTokens 的预算遍历。
func bigMsg(tokens int) string {
	return strings.Repeat("x", tokens*4)
}

// containsMsg 报告 msgs 中是否有任何消息的内容等于 want。
func containsMsg(msgs []conversation.Message, want string) bool {
	for _, m := range msgs {
		if m.Content == want {
			return true
		}
	}
	return false
}

// autoCompact 必须原样保留最近的尾部，而不是用摘要替换它。
// 我们构建一个会话，其较旧的前缀足够大以越过 keepStart > 0，
// 且尾部带有独特的内容；压缩后尾部内容必须仍然存在（而不仅仅是摘要）。
func TestAutoCompactKeepsRecentVerbatim(t *testing.T) {
	conv := conversation.NewManager()
	// 较旧的前缀：若干应当被摘要掉的大消息。
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	// 最近的尾部：我们期望原样保留的独特小消息。
	recent := []string{"RECENT-A unique-marker-A", "RECENT-B unique-marker-B"}
	conv.AddUserMessage(recent[0])
	conv.AddAssistantMessage(recent[1])

	client := &stubSummaryClient{summary: "THE SUMMARY"}
	msg, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg == "" {
		t.Fatalf("expected a compaction message, got empty (degraded to no-op)")
	}
	out := conv.GetMessages()

	// 摘要必须存在。
	var sawSummary bool
	for _, m := range out {
		if strings.Contains(m.Content, "THE SUMMARY") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Errorf("summary not present after compaction")
	}
	// 最近的尾部必须原样保留，而不是被合并进摘要。
	for _, r := range recent {
		if !containsMsg(out, r) {
			t.Errorf("recent message %q not preserved verbatim after compaction; messages=%v", r, msgContents(out))
		}
	}
}

// 当提供了 sessionID + workDir 时，autoCompact 必须把一条 compact_boundary
// 记录持久化到会话日志中：内联的摘要加上保留的尾部（role+content）。
// 这是恢复流程中落盘的那一半——session.FindLastCompactBoundary 随后从它重建压缩后的状态。
func TestAutoCompactPersistsBoundary(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("RECENT-TAIL-USER unique-marker-A")
	conv.AddAssistantMessage("RECENT-TAIL-ASSISTANT unique-marker-B")

	workDir := t.TempDir()
	sid := "compact-roundtrip"
	client := &stubSummaryClient{summary: "PERSISTED-SUMMARY"}

	msg, err := autoCompact(context.Background(), conv, client, workDir, sid, 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg == "" {
		t.Fatalf("expected a compaction message, got empty (degraded to no-op)")
	}

	// 读回会话日志并断言 boundary 已写入，且摘要与保留的尾部已内联。
	msgs := session.LoadSession(workDir, sid)
	boundary, after, ok := session.FindLastCompactBoundary(msgs)
	if !ok {
		t.Fatalf("expected a compact_boundary record to be persisted")
	}
	if boundary.Summary != "PERSISTED-SUMMARY" {
		t.Fatalf("persisted summary mismatch: got %q", boundary.Summary)
	}
	if len(after) != 0 {
		t.Fatalf("no messages should follow a freshly written boundary, got %d", len(after))
	}
	// 保留的尾部必须原样内联到 boundary 中。
	var sawTailUser, sawTailAssistant bool
	for _, k := range boundary.Keep {
		if k.Content == "RECENT-TAIL-USER unique-marker-A" {
			sawTailUser = true
		}
		if k.Content == "RECENT-TAIL-ASSISTANT unique-marker-B" {
			sawTailAssistant = true
		}
	}
	if !sawTailUser || !sawTailAssistant {
		t.Fatalf("kept tail not inlined into boundary: %+v", boundary.Keep)
	}
	// boundary 的保留尾部必须与 autoCompact 原样保留的会话尾部完全一致
	// （相同的 role+content，且顺序一致）。磁盘上存储的摘要是纯摘要文本，
	// 内存重建之后会话为 [summary user msg] + [continuation ack] + keep，
	// 因此保留的尾部位于重建后会话的末尾。
	rebuilt := conv.GetMessages()
	tail := rebuilt[len(rebuilt)-len(boundary.Keep):]
	for i, k := range boundary.Keep {
		if tail[i].Role != k.Role || tail[i].Content != k.Content {
			t.Fatalf("boundary keep[%d]=%+v does not match in-memory tail %+v", i, k, tail[i])
		}
	}
}

// 没有 sessionID/workDir 时，autoCompact 不得触碰任何会话日志
// （一次性调用方、测试、子 agent）——行为与之前保持一致。
func TestAutoCompactNoSessionNoBoundary(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("RECENT-A")
	conv.AddAssistantMessage("RECENT-B")

	workDir := t.TempDir()
	client := &stubSummaryClient{summary: "S"}
	// 空的 sessionID → 不进行持久化。
	if _, err := autoCompact(context.Background(), conv, client, workDir, "", 200000, nil, nil); err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	// 任何 id 下都不应创建会话文件。
	msgs := session.LoadSession(workDir, "anything")
	if len(msgs) != 0 {
		t.Fatalf("expected no session log written when sessionID empty, got %d", len(msgs))
	}
}

// autoCompact 必须只摘要 messages[:keepStart]；保留的尾部不得
// 出现在交给摘要器的 prompt 中。
func TestAutoCompactSummaryOnlyCoversPrefix(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("PREFIX-ONLY-CONTENT " + bigMsg(3000))
		conv.AddAssistantMessage("PREFIX-ONLY-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("TAIL-ONLY-MARKER")
	conv.AddAssistantMessage("TAIL-ONLY-REPLY")

	client := &stubSummaryClient{summary: "S"}
	if _, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil); err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if !client.streamCalled {
		t.Fatalf("summarizer was never called")
	}
	if strings.Contains(client.lastPrompt, "TAIL-ONLY-MARKER") {
		t.Errorf("summary prompt must not include the kept tail, but it did")
	}
	if !strings.Contains(client.lastPrompt, "PREFIX-ONLY-CONTENT") {
		t.Errorf("summary prompt must include the summarized prefix, but it did not")
	}
}

// computeKeepStartIndex 绝不能拆开 tool_use ↔ tool_result 配对：如果预算
// 边界落在携带 tool_results 的用户消息上，就必须向后移动，以包含产生它们的
// assistant tool_use 消息。
func TestComputeKeepStartIndexDoesNotSplitToolPair(t *testing.T) {
	conv := conversation.NewManager()
	// 大前缀，使 keepStart > 0。
	for i := 0; i < 8; i++ {
		conv.AddUserMessage(bigMsg(3000))
		conv.AddAssistantMessage(bigMsg(3000))
	}
	// 靠近尾部的一对 tool_use / tool_result。tool_result 是条大消息，
	// 因此预算边界很可能正好落在它上面。
	conv.AddToolUseMessage("calling tool", "tu-1", "ReadFile", map[string]any{"path": "/x"})
	conv.AddToolResultMessage("tu-1", bigMsg(9000), false)
	msgs := conv.GetMessages()

	keepStart := computeKeepStartIndex(msgs)
	if keepStart <= 0 || keepStart >= len(msgs) {
		t.Fatalf("keepStart=%d out of expected range (0, %d)", keepStart, len(msgs))
	}
	// 边界消息不能是孤立的 tool_result，否则它匹配的 tool_use 会留在被摘要的前缀里。
	if hasToolResults(msgs[keepStart]) {
		t.Fatalf("keepStart landed on a tool_result message (orphaned); keepStart=%d", keepStart)
	}
	// 验证配对在保留的尾部内是完整的：遍历它并确保每个
	// tool_result 在保留的切片内都有一个前置的 tool_use。
	keep := msgs[keepStart:]
	openUses := map[string]bool{}
	for _, m := range keep {
		for _, tu := range m.ToolUses {
			openUses[tu.ToolUseID] = true
		}
		for _, tr := range m.ToolResults {
			if !openUses[tr.ToolUseID] {
				t.Errorf("tool_result %s in kept tail has no matching tool_use in tail (pair split)", tr.ToolUseID)
			}
		}
	}
}

// 当会话太短而没有可摘要的前缀（keepStart <= 0）时，
// autoCompact 必须退化为空操作：不进行摘要，会话保持不变。
func TestAutoCompactDegradesWhenTooFewMessages(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage("just one")
	conv.AddAssistantMessage("two")
	before := conv.GetMessages()

	client := &stubSummaryClient{summary: "S"}
	msg, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg != "" {
		t.Errorf("expected no-op (empty message) for too-few messages, got %q", msg)
	}
	if client.streamCalled {
		t.Errorf("summarizer should not be called when degrading to no-op")
	}
	after := conv.GetMessages()
	if len(before) != len(after) {
		t.Errorf("conversation changed during no-op: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].Content != after[i].Content {
			t.Errorf("message %d mutated during no-op", i)
		}
	}
}

func msgContents(msgs []conversation.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		c := m.Content
		if len(c) > 40 {
			c = c[:40] + "..."
		}
		out[i] = c
	}
	return out
}

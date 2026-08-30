// Package compact 实现 Cody 的 Layer 2 上下文管理：
// 由 LLM 驱动的整段对话摘要，以 token 比例（默认超过上下文窗口的 80%）为门槛。
// 将整段对话替换为一条摘要消息加一个延续确认。也可通过 ForceCompact
// （/compact 斜杠命令）触发。
//
// Layer 1（工具结果预算）在 package toolresult：单条超限和单消息聚合
// 超限都在结果进入对话历史那一刻处理完，消息一进历史就是终态，这里
// 拿到的消息大小即最终大小。
package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/session"
)

const (
	// maxPTLRetries 限制在遇到 prompt-too-long（提示词过长）错误后，通过丢弃
	// 最老的 API 轮次分组来重试摘要请求的次数。
	maxPTLRetries = 3
	// ptlRetryMarker 在丢弃旧分组后前置，确保摘要请求仍以 user 角色的消息开头。
	ptlRetryMarker = "[earlier conversation truncated for compaction retry]"

	// autoCompactThreshold 是旧的比率阈值（仅供参考）。
	// 当前的实际决策使用下面的绝对 token 公式：
	// 当已用 token 接近上下文窗口上限时触发压缩，为下一轮留出余量。
	autoCompactThreshold = 0.80

	// summaryOutputReserve 为摘要响应本身预留空间，因此有效窗口为
	// contextWindow − min(model maxOutput, summaryOutputReserve)。
	summaryOutputReserve = 20000
	// autoCompactSafetyMargin 在有效窗口之下设置软性自动压缩触发线。
	autoCompactSafetyMargin = 13000
	// manualCompactSafetyMargin 设置硬性阻断线：一旦已用 token 越过
	// effectiveWindow − manualCompactSafetyMargin，就强制压缩，
	// 而不是依赖软性触发。
	manualCompactSafetyMargin = 3000
)

// 压缩时对最近消息的保留预算：不是对整段对话做摘要并丢弃所有原始消息，
// 而是原样保留最近消息的尾部，只对更早的前缀做摘要。
const (
	// keepRecentTokens 是 token 预算的下限：从尾部往回走，逐条累加消息的 token，
	// 直到保留的数量达到该值。
	keepRecentTokens = 10000
	// minKeepMessages 是不论 token 数量都要保留的最近消息条数下限。
	// 只要 keepRecentTokens 或 minKeepMessages 任一满足就停止往回走（以先到者为准）。
	minKeepMessages = 5
	// keepMaxTokens 限制保留的尾部：一旦累计 token 将超过该值，即使未达到下限
	// 也停止往回走，这样不会保留过多内容导致摘要毫无节省。
	keepMaxTokens = 40000
)

// computeCompactThreshold 返回 Layer 2 应触发的绝对已用 token 阈值。
// effectiveWindow = contextWindow − min(maxOutput, summaryOutputReserve)；
// 阈值即 effectiveWindow 减去安全余量（硬性阻断线用 manual 余量，软性触发用 auto 余量）。
func computeCompactThreshold(contextWindow, maxOutput int, manual bool) int {
	reserve := summaryOutputReserve
	if maxOutput > 0 && maxOutput < reserve {
		reserve = maxOutput
	}
	effectiveWindow := contextWindow - reserve
	margin := autoCompactSafetyMargin
	if manual {
		margin = manualCompactSafetyMargin
	}
	return effectiveWindow - margin
}

// MaxConsecutiveAutoCompactFailures 在上下文不可恢复地超出上限（例如 prompt_too_long）时
// 停止自动压缩重试，避免 agent 在每次迭代都用注定失败的重试请求轰炸 API。
const MaxConsecutiveAutoCompactFailures = 3

// AutoCompactTrackingState 在 agent 循环迭代间传递熔断器状态。调用方持有该结构体；
// ManageContext 会就地修改它。
type AutoCompactTrackingState struct {
	// ConsecutiveFailures 统计自上次成功以来返回错误的自动压缩尝试次数。
	// 成功时重置为 0。
	ConsecutiveFailures int
}

// summarySystemPrompt 指示模型生成两段式响应：先是 <analysis> 草稿区，
// 后跟 <summary> 块。formatCompactSummary 会在结果写回对话前剥离 analysis 块，
// 只留下结构化的摘要。
const summarySystemPrompt = `Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions.
This summary should be thorough in capturing technical details, code patterns, and architectural decisions that would be essential for continuing development work without losing context.

Before providing your final summary, wrap your analysis in <analysis> tags to organize your thoughts and ensure you've covered all necessary points. In your analysis process:

1. Chronologically analyze each message and section of the conversation. For each section thoroughly identify:
   - The user's explicit requests and intents
   - Your approach to addressing the user's requests
   - Key decisions, technical concepts and code patterns
   - Specific details like:
     - file names
     - full code snippets
     - function signatures
     - file edits
   - Errors that you ran into and how you fixed them
   - Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
2. Double-check for technical accuracy and completeness, addressing each required element thoroughly.

After your analysis, output your final summary wrapped in <summary> tags. Your summary should include the following sections:

1. Primary Request and Intent: Capture all of the user's explicit requests and intents in detail
2. Key Technical Concepts: List all important technical concepts, technologies, and frameworks discussed.
3. Files and Code Sections: Enumerate specific files and code sections examined, modified, or created. Pay special attention to the most recent messages and include full code snippets where applicable and include a summary of why this file read or edit is important.
4. Errors and fixes: List all errors that you ran into, and how you fixed them. Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
5. Problem Solving: Document problems solved and any ongoing troubleshooting efforts.
6. All user messages: List ALL user messages that are not tool results. These are critical for understanding the users' feedback and changing intent.
7. Pending Tasks: Outline any pending tasks that you have explicitly been asked to work on.
8. Current Work: Describe in detail precisely what was being worked on immediately before this summary request, paying special attention to the most recent messages from both user and assistant. Include file names and code snippets where applicable.
9. Optional Next Step: List the next step that you will take that is related to the most recent work you were doing. IMPORTANT: ensure that this step is DIRECTLY in line with the user's most recent explicit requests, and the task you were working on immediately before this summary request. If your last task was concluded, then only list next steps if they are explicitly in line with the users request.
   If there is a next step, include direct quotes from the most recent conversation showing exactly what task you were working on and where you left off. This should be verbatim to ensure there's no drift in task interpretation.

Output structure:

<analysis>
[Your thought process, ensuring all points are covered thoroughly and accurately]
</analysis>

<summary>
1. Primary Request and Intent:
   [Detailed description]

2. Key Technical Concepts:
   - [Concept 1]
   - [Concept 2]

3. Files and Code Sections:
   - [File Name 1]
      - [Summary and important code snippet]

4. Errors and fixes:
   - [Error and fix description]

5. Problem Solving:
   [Description]

6. All user messages:
   - [User message 1]
   - [User message 2]

7. Pending Tasks:
   - [Task 1]

8. Current Work:
   [Precise description]

9. Optional Next Step:
   [Next step if applicable]
</summary>`

// EstimateTokens 对 content、tool args、tool results 和 thinking blocks 使用
// 约每 3.5 个字符一个 token 的估算。
func EstimateTokens(messages []conversation.Message) int {
	total := 0
	for _, m := range messages {
		total += int(float64(len(m.Content))/3.5) + 4
		for _, tu := range m.ToolUses {
			argsJSON, _ := json.Marshal(tu.Arguments)
			total += 50 + int(float64(len(argsJSON))/3.5)
		}
		for _, tr := range m.ToolResults {
			total += int(float64(len(tr.Content))/3.5) + 10
		}
		for _, tb := range m.ThinkingBlocks {
			total += int(float64(len(tb.Thinking)) / 3.5)
		}
	}
	return total
}

// UsageAnchor 记录最近一次真实的 API 用量及该用量上报时的对话长度。
// baselineTokens 是那一轮的 prompt+output 真实总大小
// （input + cache_read + cache_creation + output）；anchorCount 是
// assistant 那一轮落定后立即取到的 conv.Len()。任何在 anchorCount 之后追加的内容
// （工具结果、下一条用户消息、系统提醒）都还没有真实用量，
// 因此会在 baseline 之上做增量估算。
//
// 零值 anchor（HasUsage 为 false）表示尚未观测到真实用量——即第一轮——
// 此时调用方退回到整段字符数估算。
type UsageAnchor struct {
	BaselineTokens int
	AnchorCount    int
	HasUsage       bool
}

// BaselineFromUsage 把一次 API 用量报告折叠成用作锚点基线的单个
// “已真正上线的 token 数”。Anthropic 将 cache_read / cache_creation 与
// input_tokens 分开报告，因此真实 prompt 大小是全部四个计数器的总和。
func BaselineFromUsage(u llm.UsageInfo) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}

// ComputeUsedTokens 返回与压缩阈值比较的当前“已用 token”数值。
// 当存在真实用量锚点时，返回 baselineTokens 加上对锚点之后追加消息的估算
// （增量估算）。没有锚点时（冷启动、第一轮）则回退为对所有消息做
// 整段字符数估算，与原先行为一致，保证在首次用量报告到来前 agent 仍可用。
func ComputeUsedTokens(messages []conversation.Message, anchor UsageAnchor) int {
	if !anchor.HasUsage {
		return EstimateTokens(messages)
	}
	// 防御性钳制：如果对话被回退到锚点之前（例如压缩之后），锚点已失效，
	// 因此估算全部内容，而不是越界取索引。
	if anchor.AnchorCount < 0 || anchor.AnchorCount > len(messages) {
		return EstimateTokens(messages)
	}
	return anchor.BaselineTokens + EstimateTokens(messages[anchor.AnchorCount:])
}

// ComputeUsedTokensFromConv 从 ConversationManager 读取锚点状态来计算
// 当前 token 用量。这是 ComputeUsedTokens 的便捷封装，避免调用方
// 手动传递 UsageAnchor 参数。
func ComputeUsedTokensFromConv(conv *conversation.Manager) int {
	baseline, count, has := conv.UsageAnchorState()
	return ComputeUsedTokens(conv.GetMessages(), UsageAnchor{
		BaselineTokens: baseline,
		AnchorCount:    count,
		HasUsage:       has,
	})
}

// ManageContext 在已用 token 达到自动压缩阈值（effectiveWindow − auto 余量）时执行
// Layer 2（autoCompact）；一旦越过硬性阻断线（effectiveWindow − manual 余量）则强制压缩。
// 参见 computeCompactThreshold。Layer 1（工具结果预算）在摄取时执行——
// 工具结果进入历史时已是最终大小，因此这里的估算无需再做裁剪。
//
// tracking 在迭代间传递熔断器状态。为 nil 时熔断器被禁用
// （用于测试和一次性调用方）。
//
// anchor 携带最近一次真实 API 用量（baseline 以及上报时的对话长度）。
// 存在时，已用 token 数值为 baselineTokens 加上对之后追加消息的增量估算；
// 不存在时（第一轮）回退为整段字符数估算。这只会改变“当前已用 token”
// 的计算方式——阈值公式（effectiveWindow − margin）保持不变。
//
// `workDir` + `sessionID` 定位磁盘上的会话日志；两者都非空时，
// 成功的压缩会在那里追加一条 compact_boundary 记录，以便之后恢复时
// 重建压缩后的状态，而不是重放压缩前的完整记录。任一为空时，
// 跳过边界持久化（测试、一次性调用方），行为不变。
func ManageContext(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	maxOutput int,
	tracking *AutoCompactTrackingState,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	// 历史里的工具结果在入历史时已按预算处理为终态，conv 自身消息
	// 就是实际发送量，直接用它估算。
	baseline, count, has := conv.UsageAnchorState()
	anchor := UsageAnchor{BaselineTokens: baseline, AnchorCount: count, HasUsage: has}
	tokens := ComputeUsedTokens(conv.GetMessages(), anchor)
	// 软性自动压缩触发：已用 token >= effectiveWindow − auto 余量。
	if tokens < computeCompactThreshold(contextWindow, maxOutput, false) {
		return "", nil
	}

	// 硬性阻断线：一旦已用 token 越过 effectiveWindow − manual 余量，
	// 就强制压缩（绕过熔断器），而不是走软性自动路径，
	// 因为上下文已太接近上限，不值得冒险跳过。
	if tokens >= computeCompactThreshold(contextWindow, maxOutput, true) {
		return ForceCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
	}

	// 熔断器：连续失败 N 次后停止重试。没有它，上下文不可恢复地超限的会话
	// 会在每次迭代都用注定失败的压缩尝试轰炸 API。
	if tracking != nil && tracking.ConsecutiveFailures >= MaxConsecutiveAutoCompactFailures {
		return "", nil
	}

	msg, err := autoCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
	if err != nil {
		if tracking != nil {
			tracking.ConsecutiveFailures++
		}
		return "", err
	}
	if tracking != nil {
		tracking.ConsecutiveFailures = 0
	}
	return msg, nil
}

// ForceCompact 是手动 /compact 入口。无论当前 token 比例如何，总是执行 Layer 2
// （完整摘要）。Layer 1 被跳过，因为完整摘要无论如何都取代了工具结果预算。
func ForceCompact(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	return autoCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
}

// hasToolResults 报告一条消息是否携带 tool_result 块（由工具执行器产生的
// user 角色消息）。这样的消息不能与它所回应的 assistant tool_use 分离，
// 否则 API 会因存在孤立的 tool_result 而拒绝该对话。
func hasToolResults(m conversation.Message) bool {
	return len(m.ToolResults) > 0
}

// hasToolUses 报告一条消息是否携带 tool_use 块（调用过工具的 assistant 消息）。
func hasToolUses(m conversation.Message) bool {
	return len(m.ToolUses) > 0
}

// computeKeepStartIndex 在被压缩的前缀（messages[:keepStart]）和原样保留的
// 最近消息尾部（messages[keepStart:]）之间选择边界。
//
// 从尾部往回走，逐条累加消息的 token（单条消息的 EstimateTokens）。
// 只要 token 下限（keepRecentTokens）或消息条数下限（minKeepMessages）
// 任一满足即停止——以先到者为准。但绝不让累计的尾部超过 keepMaxTokens：
// 如果再加一条就会越过该上限，就在那之前停下。
//
// 在预算遍历之后，把边界往回吸，使其绝不会拆开 tool_use ↔ tool_result 配对：
// 如果 keepStart 落在携带 tool_results 的消息上，就把它往回移，越过它所回应的
// assistant tool_use 消息，让配对在保留的尾部中保持完整
// （保留完整配对，而不是孤立的 tool_result）。
//
// 返回该索引；调用方把 keepStart <= 0（或前缀过小）视为“可压缩内容太少”，
// 回退到原先的完整摘要行为。
// 获取保留的最近消息的起始索引，前面的消息会被压缩为摘要，后面的消息会原样保留。
func computeKeepStartIndex(messages []conversation.Message) int {
	n := len(messages)
	if n == 0 {
		return 0
	}

	keptTokens := 0
	keptCount := 0
	keepStart := n
	for i := n - 1; i >= 0; i-- {
		msgTokens := EstimateTokens(messages[i : i+1])
		// 上限：如果加入这条消息会超过上限，就停下，把它留在被压缩的前缀里
		// （不保留它）。
		if keptCount > 0 && keptTokens+msgTokens > keepMaxTokens {
			break
		}
		keptTokens += msgTokens
		keptCount++
		keepStart = i
		// 下限：任一满足即结束。
		if keptTokens >= keepRecentTokens || keptCount >= minKeepMessages {
			break
		}
	}

	// 不拆开 tool_use ↔ tool_result 配对：如果边界消息携带 tool_results，
	// 就把它往回移，越过它所回应的 assistant tool_use 消息，使配对保持完整。
	for keepStart > 0 && hasToolResults(messages[keepStart]) {
		prev := keepStart - 1
		if hasToolUses(messages[prev]) {
			keepStart = prev
			continue
		}
		break
	}

	return keepStart
}

// groupMessagesByAPIRound 在 API 往返边界处把消息切分成组。
// 每个紧跟 tool_result 的 assistant 消息（即新的一轮 LLM）开启一组。
// 这样让 tool_use/tool_result 配对保持在一起，丢弃一组时绝不会产生孤立的 tool_result。
func groupMessagesByAPIRound(messages []conversation.Message) [][]conversation.Message {
	var groups [][]conversation.Message
	var current []conversation.Message
	prevHadToolResult := false

	for _, m := range messages {
		if m.Role == "assistant" && prevHadToolResult && len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, m)
		prevHadToolResult = len(m.ToolResults) > 0
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// truncateHeadForPTL 从前缀消息中丢弃最老的 API 轮次分组，直到估算的
// token 数至少下降 tokenGap。如果已经没有值得摘要的内容，返回 nil。
func truncateHeadForPTL(prefix []conversation.Message, tokenGap int) []conversation.Message {
	groups := groupMessagesByAPIRound(prefix)
	if len(groups) < 2 {
		return nil
	}

	dropCount := 0
	if tokenGap > 0 {
		acc := 0
		for _, g := range groups {
			acc += EstimateTokens(g)
			dropCount++
			if acc >= tokenGap {
				break
			}
		}
	} else {
		dropCount = max(1, len(groups)/5)
	}

	dropCount = min(dropCount, len(groups)-1)
	if dropCount < 1 {
		return nil
	}

	var result []conversation.Message
	for _, g := range groups[dropCount:] {
		result = append(result, g...)
	}
	if len(result) > 0 && result[0].Role != "user" {
		marker := conversation.Message{Role: "user", Content: ptlRetryMarker}
		result = append([]conversation.Message{marker}, result...)
	}
	return result
}

// buildPrefixText 把前缀消息序列化成文本块，供摘要 LLM 调用使用。
func buildPrefixText(prefix []conversation.Message) string {
	var sb strings.Builder
	for _, m := range prefix {
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
		for _, tu := range m.ToolUses {
			sb.WriteString(fmt.Sprintf("[tool_use %s]: %s\n", tu.ToolName, tu.ToolUseID))
		}
		for _, tr := range m.ToolResults {
			content := tr.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			sb.WriteString(fmt.Sprintf("[tool_result]: %s\n", content))
		}
	}
	return sb.String()
}

// autoCompact 是 Layer 2：对较旧前缀做 LLM 摘要，只把 messages[:keepStart] 替换为
// 一条摘要消息，而最近的尾部（messages[keepStart:]）原样保留。摘要落定后，
// 会在摘要消息上追加一个 recovery 块，使模型仍然拥有它刚读过的文件的快照、
// 它调用过的技能的 SOP，以及当前的工具列表。当可摘要的前缀太少时，
// 退化为原先的完整摘要行为。
func autoCompact(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	messages := conv.GetMessages()
	beforeTokens := EstimateTokens(messages)

	// 选择要原样保留的最近尾部；只有 keepStart 之前的前缀会被摘要。
	// 如果边界之后几乎没有（或完全没有）可摘要的内容，压缩就没有意义——
	// 保持对话不变。
	keepStart := computeKeepStartIndex(messages)
	if keepStart <= 0 {
		// 全部内容都在保留的尾部内（对话太短）——退化为空操作，
		// 免得“白白压缩”。
		return "", nil
	}
	prefix := messages[:keepStart]
	keep := messages[keepStart:]

	// 调用 LLM 生成摘要，带 PTL 重试：如果摘要请求本身超出上下文窗口，
	// 从最老的 API 轮次开始丢弃，最多重试 maxPTLRetries 次。
	finalSummary, err := callSummaryWithPTLRetry(ctx, client, prefix, toolSchemas)
	if err != nil {
		return "", err
	}

	// 持久化 compact_boundary 记录，以便之后恢复时重建这个压缩后的状态
	// （摘要 + 保留的尾部），而不是重放压缩前的完整记录。只追加：
	// 原始前缀消息仍在会话文件中，但不会越过该边界重放。我们把保留的尾部
	// 连同其工具块一起内联进来，因此恢复后的会话保留那些最近轮次的完整
	// 工具调用链。边界存储的是纯摘要文本，而不是 recovery 附件，
	// 因为 recovery 快照是内存中的重建辅助，恢复时并不可用。
	// sessionID/workDir 为空时跳过（测试、一次性调用方）。
	if sessionID != "" && workDir != "" {
		keepRecords := make([]session.KeepMessage, 0, len(keep))
		for _, m := range keep {
			keepRecords = append(keepRecords, session.FromConversationKeep(m))
		}
		// 保存 compact_boundary 记录到会话文件中，便于后续恢复时直接加载压缩后的状态。
		session.SaveCompactBoundary(workDir, sessionID, finalSummary, keepRecords)
	}

	// 对话重建
	content := "本次会话延续自之前的对话，因上下文空间不足进行了压缩。以下是早期对话的摘要：\n\n" + finalSummary
	if len(keep) > 0 {
		content += "\n\n近期消息已原样保留。"
	}
	if sessionID != "" && workDir != "" {
		content += fmt.Sprintf("\n\n如果你需要压缩前的具体细节（代码片段、报错信息等），请用 ReadFile 读取完整会话记录：%s", session.SessionFilePath(workDir, sessionID))
	}
	// 附加恢复快照：最近读过的文件、技能定义、工具列表，以及关于不要从摘要中猜测的结尾说明。
	if attachment := BuildRecoveryAttachment(recovery, toolSchemas); attachment != "" {
		content += "\n\n---\n\n" + attachment
	}

	compacted := conversation.NewManager()
	compacted.AddUserMessage(content)
	compacted.AppendMessages(keep)

	*conv = *compacted// 覆盖原来的对话历史，保留原来的锚点状态
	afterTokens := EstimateTokens(conv.GetMessages())
	return fmt.Sprintf("Compacted: %d → %d estimated tokens", beforeTokens, afterTokens), nil
}

// callSummaryWithPTLRetry 把前缀发送给 LLM 做摘要。如果请求返回
// ContextTooLongError，它会从前缀中丢弃最老的 API 轮次分组并重试，
// 最多 maxPTLRetries 次。
func callSummaryWithPTLRetry(
	ctx context.Context,
	client llm.Client,
	prefix []conversation.Message,
	toolSchemas []map[string]any,
) (string, error) {
	currentPrefix := prefix
	for attempt := 0; ; attempt++ {
		text := buildPrefixText(currentPrefix)
		summaryConv := conversation.NewManager()
		summaryConv.AddUserMessage(summarySystemPrompt + "\n\n" + text)

		// 调用llm生成摘要
		events, errs := client.Stream(ctx, summaryConv, toolSchemas)
		var summary strings.Builder
		for ev := range events {
			if td, ok := ev.(llm.TextDelta); ok {
				summary.WriteString(td.Text)
			}
		}
		var streamErr error
		select {
		case streamErr = <-errs:
		default:
		}

		if streamErr == nil {
			return formatCompactSummary(summary.String()), nil
		}

		var ptlErr *llm.ContextTooLongError
		if !errors.As(streamErr, &ptlErr) || attempt >= maxPTLRetries {
			return "", streamErr
		}

		// 计算要释放的 tokenGap（约为当前前缀估算 token 的 1/5），
		// 调用 truncateHeadForPTL 从最老开始丢整组，
		// 直到释放足够 token 或只剩下很少可删内容
		tokenGap := EstimateTokens(currentPrefix) / 5
		truncated := truncateHeadForPTL(currentPrefix, tokenGap)
		if truncated == nil {
			return "", streamErr
		}
		currentPrefix = truncated
	}
}

// formatCompactSummary 从模型的两段式响应中剥离 <analysis> 草稿区，
// 只返回 <summary> 块的内容。当两个标签都不存在时（模型未遵守提示结构），
// 回退到原始文本，确保摘要永远不会整体丢失。
func formatCompactSummary(raw string) string {
	if start := strings.Index(raw, "<summary>"); start >= 0 {
		body := raw[start+len("<summary>"):]
		if end := strings.Index(body, "</summary>"); end >= 0 {
			return strings.TrimSpace(body[:end])
		}
		return strings.TrimSpace(body)
	}
	// 没有 <summary> 块——若存在 <analysis>...</analysis> 块则丢弃它，返回剩余内容。
	if start := strings.Index(raw, "<analysis>"); start >= 0 {
		if end := strings.Index(raw, "</analysis>"); end > start {
			return strings.TrimSpace(raw[:start] + raw[end+len("</analysis>"):])
		}
	}
	return strings.TrimSpace(raw)
}

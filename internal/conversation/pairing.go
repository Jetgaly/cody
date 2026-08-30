package conversation

// Anthropic 要求每个 tool_use 都有配对的 tool_result，缺一个整条请求就会被拒。
// 会话历史出现不配对的情况有几种来路：用户中断在工具执行途中、进程退出后从磁盘
// 恢复会话、并发写入交错。这些文案是补位用的，模型看到后应当明白该工具没有产出。
const (
	// InterruptedToolResult 用于补上没有结果的工具调用。工具可能压根没启动，
	// 也可能跑到一半被打断，所以措辞上不能断言它没有产生任何副作用。
	InterruptedToolResult = "Tool execution was interrupted. The tool may or may not have completed; verify before relying on its effects."
	// RejectedToolResult 用于用户明确拒绝授权的工具调用。这种情况可以断言什么都没改，
	// 必须讲清楚，否则模型会以为修改已经生效并继续往下走。
	RejectedToolResult = "The user rejected this tool use. Nothing was changed (for file edits, the new content was NOT written)."
)

// EnsureToolPairing 返回一份修好配对关系的消息副本，输入不会被修改。
//
// 做两件事：
//   - 给没有结果的 tool_use 补一条标记为错误的 tool_result，接在紧随其后的位置
//   - 丢掉找不到对应 tool_use 的孤儿 tool_result
//
// 调用点在发请求之前，这样中断、恢复会话、并发交错这几种来路只需要这一处兜底，
// 不必在每个前端各写一遍。补出来的内容不写回对话历史：历史应当如实记录发生过什么，
// 而补位只是为了让这一次请求合法。
func EnsureToolPairing(messages []Message) []Message {
	resolved := make(map[string]struct{})
	issued := make(map[string]struct{})
	for _, m := range messages {
		for _, tr := range m.ToolResults {
			resolved[tr.ToolUseID] = struct{}{}
		}
		for _, tu := range m.ToolUses {
			issued[tu.ToolUseID] = struct{}{}
		}
	}

	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		// 孤儿工具结果：对应的调用不在历史里，留着只会让请求被拒
		if len(m.ToolResults) > 0 {
			kept := make([]ToolResultBlock, 0, len(m.ToolResults))
			for _, tr := range m.ToolResults {
				if _, ok := issued[tr.ToolUseID]; ok {
					kept = append(kept, tr)
				}
			}
			if len(kept) == 0 && m.Content == "" && len(m.ToolUses) == 0 {
				continue // 整条消息只剩空壳，直接丢掉以免破坏角色交替
			}
			m.ToolResults = kept
		}

		out = append(out, m)

		// 悬空工具调用：补一条错误结果紧跟其后，保持 tool_use 与 tool_result 相邻
		var missing []ToolResultBlock
		for _, tu := range m.ToolUses {
			if _, ok := resolved[tu.ToolUseID]; ok {
				continue
			}
			missing = append(missing, ToolResultBlock{
				ToolUseID: tu.ToolUseID,
				Content:   InterruptedToolResult,
				IsError:   true,
			})
			resolved[tu.ToolUseID] = struct{}{}
		}
		if len(missing) > 0 {
			out = append(out, Message{Role: "user", ToolResults: missing})
		}
	}
	return out
}

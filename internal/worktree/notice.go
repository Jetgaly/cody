package worktree

import "fmt"

// BuildWorktreeNotice 返回在子 agent 运行于隔离 worktree 时注入其 prompt 的提示文本。
// 它告诉子 agent 转换继承上下文中的路径，并重新读取文件。
func BuildWorktreeNotice(parentCwd, worktreeCwd string) string {
	return fmt.Sprintf(
		"You've inherited the conversation context above from a parent agent working in %s. "+
			"You are operating in an isolated git worktree at %s — same repository, same relative "+

			"file structure, separate working copy. Paths in the inherited context refer to the "+
			"parent's working directory; translate them to your worktree root. Re-read files before "+
			"editing if the parent may have modified them since they appear in the context. Your "+
			"changes stay in this worktree and will not affect the parent's files.",

		parentCwd, worktreeCwd,
	)
}


package worktree

import (
	"context"
	"strconv"
	"strings"
)

// ChangeSummary 保存 countWorktreeChanges 的结果。
type ChangeSummary struct {
	ChangedFiles int
	Commits      int
}

// HasWorktreeChanges 返回 true 表示 worktree 自 headCommit 以来有未提交的改动或新提交。
// git 失败时也返回 true（fail-closed）。
func HasWorktreeChanges(ctx context.Context, worktreePath, headCommit string) bool {
	stdout, _, code := runGit(ctx, worktreePath, "status", "--porcelain")
	if code != 0 {
		return true // fail-closed（失败即关闭）
	}
	if strings.TrimSpace(stdout) != "" {
		return true
	}

	stdout, _, code = runGit(ctx, worktreePath, "rev-list", "--count", headCommit+"..HEAD")
	if code != 0 {
		return true // fail-closed（失败即关闭）
	}
	n, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		return true // fail-closed（失败即关闭）
	}
	return n > 0
}

// CountWorktreeChanges 返回详细的改动摘要；当状态无法可靠确定时返回 nil。
// 把它当作安全闸门使用的调用方必须把 nil 视为“未知，按不安全处理”
// （fail-closed）。
func CountWorktreeChanges(ctx context.Context, worktreePath, originalHeadCommit string) *ChangeSummary {
	stdout, _, code := runGit(ctx, worktreePath, "status", "--porcelain")
	if code != 0 {
		return nil // fail-closed（失败即关闭）
	}
	changedFiles := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			changedFiles++
		}
	}

	if originalHeadCommit == "" {
		// 没有基线提交就无法统计提交数。按 fail-closed 处理。
		return nil
	}

	stdout, _, code = runGit(ctx, worktreePath, "rev-list", "--count", originalHeadCommit+"..HEAD")
	if code != 0 {
		return nil // fail-closed（失败即关闭）
	}
	commits, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		return nil
	}

	return &ChangeSummary{
		ChangedFiles: changedFiles,
		Commits:      commits,
	}
}


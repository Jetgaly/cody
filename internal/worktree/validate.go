package worktree

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxWorktreeSlugLength 将 worktree 名称限制在 64 个字符以内。
const MaxWorktreeSlugLength = 64

// validWorktreeSlugSegment 是在将 slug 按 '/' 拆分之后应用的逐段白名单。
var validWorktreeSlugSegment = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ValidateWorktreeSlug 验证 worktree slug，以防止路径遍历和目录逃逸。
// slug 会通过 filepath.Join 拼接到 `.cody/worktrees/<slug>` 中，而 filepath.Join
// 会规范化 `.` 段——因此 `./././target` 会逃出 worktrees 目录。同样，绝对路径
// （以 `/` 或 `C:\` 开头）会完全丢弃前缀。
//
// 允许正斜杠用于嵌套（例如 `asm/feature-foo`）；每个段独立对照白名单验证，
// 因此 `.` / `.` 段以及盘符字符仍会被拒绝。
//
// 同步返回——调用方依赖此函数在任何副作用（git 命令、hook 执行、chdir）之前运行。
func ValidateWorktreeSlug(slug string) error {
	if len(slug) > MaxWorktreeSlugLength {
		return fmt.Errorf(
			"Invalid worktree name: must be %d characters or fewer (got %d)",
			MaxWorktreeSlugLength, len(slug),
		)
	}
	// 开头或结尾的 `/` 会让 filepath.Join 产生绝对路径或悬空段。
	// 拆分并逐一验证每个段即可同时拒绝这两种情况（空段无法通过正则），
	// 同时仍允许 `user/feature` 这样的嵌套。
	for _, segment := range strings.Split(slug, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf(
				`Invalid worktree name %q: must not contain "." or ".." path segments`,
				slug,
			)
		}
		if !validWorktreeSlugSegment.MatchString(segment) {
			return fmt.Errorf(
				`Invalid worktree name %q: each "/"-separated segment must be non-empty and contain only letters, digits, dots, underscores, and dashes`,
				slug,
			)
		}
	}
	return nil
}

// FlattenSlug 将嵌套的 slug（`user/feature` → `user+feature`）展平，
// 用于分支名和目录路径。在任一位置嵌套都是不安全的：
// git refs：`worktree-user`（文件）与 `worktree-user/feature`（需要目录）
// 是 git 会拒绝的 D/F 冲突。
// 目录：`.cody/worktrees/user/feature/` 位于 `user` 工作区内部；
// 对父目录执行 `git worktree remove` 会删除带有未提交内容的子目录。
//
// `+` 在 git 分支名和文件系统路径中都是合法的，但不在 slug 段白名单
// （[a-zA-Z0-9._-]）内，因此该映射是单射的。
func FlattenSlug(slug string) string {
	return strings.ReplaceAll(slug, "/", "+")
}

// WorktreeBranchName 返回与 slug 关联的工作区对应的 git 分支名。格式：
// "worktree-<flattenedSlug>"。
func WorktreeBranchName(slug string) string {
	return "worktree-" + FlattenSlug(slug)
}


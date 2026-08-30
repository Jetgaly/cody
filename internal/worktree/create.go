package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// WorktreesDir 是所有由 Cody 管理的工作区所在的位置：仓库根目录下一个
// 已被 .gitignore 忽略的独立目录。
func WorktreesDir(repoRoot string) string {
	return filepath.Join(repoRoot, ".cody", "worktrees")
}

// WorktreePathFor 返回 slug 对应的工作区目录路径，嵌套的 slug 会被展平
// （`team/alice` → `team+alice`）。
func WorktreePathFor(repoRoot, slug string) string {
	return filepath.Join(WorktreesDir(repoRoot), FlattenSlug(slug))
}

// CreateResult 是 getOrCreateWorktree 的结果——Existed=true 表示快速恢复了
// 已有的工作区（跳过了 `git worktree add` 和 performPostCreationSetup）；Existed=false
// 表示刚刚创建，调用方仍需执行创建后的初始化设置。
type CreateResult struct {
	WorktreePath   string
	WorktreeBranch string
	HeadCommit     string
	BaseBranch     string // Existed=true 时为空（恢复路径不计算 baseBranch）
	Existed        bool
}

// getOrCreateWorktree 在 <repoRoot>/.cody/worktrees/ 下为给定的 slug 创建一个新的
// git 工作区，如果已存在则恢复它。
//
// 快速恢复路径：ReadWorktreeHeadSha 直接读取 .git 指针文件（不启动子进程，也不向上遍历）。
// 在 1600 万个对象的仓库上，这能省下每次恢复都会执行的约 6-8 秒 `git fetch` commit-graph 扫描。
//
// 创建路径：仅通过文件系统读取来解析默认分支（当 origin/<default> 已在本地已知时，
// 不执行 `git fetch`），然后运行 `git worktree add -B worktree-<flat> <path> <baseBranch>`。
//
// `-B`（大写，不是 `-b`）：重置被删除的工作区目录遗留的孤儿分支。
// 每次创建省掉一次 `git branch -D` 子进程。
func getOrCreateWorktree(ctx context.Context, repoRoot, slug string) (*CreateResult, error) {
	worktreePath := WorktreePathFor(repoRoot, slug)
	worktreeBranch := WorktreeBranchName(slug)

	// 快速恢复路径：已存在的工作区 → 跳过 fetch 和 add。
	//复用时只刷新 mtime。后台清理循环看的就是目录的 mtime，刷新一下就不会被误清。
	existingHead, err := ReadWorktreeHeadSha(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("read worktree HEAD: %w", err)
	}
	if existingHead != "" {
		return &CreateResult{
			WorktreePath:   worktreePath,
			WorktreeBranch: worktreeBranch,
			HeadCommit:     existingHead,
			Existed:        true,
		}, nil
	}

	if err := os.MkdirAll(WorktreesDir(repoRoot), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktrees dir: %w", err)
	}

	// 解析 baseBranch + baseSha。如果 origin/<default> 在本地已存在，跳过 fetch——在大仓库中，
	// fetch 在碰到网络之前就会消耗约 6-8 秒做本地的 commit-graph 扫描。基准略旧没关系；
	// 用户想要最新内容可以在工作区内自行 pull。
	defaultBranch, err := GetDefaultBranch(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("get default branch: %w", err)
	}
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve git dir: %w", err)
	}
	var baseBranch, baseSha string
	if gitDir != "" {
		baseSha, _ = ResolveRef(gitDir, "refs/remotes/origin/"+defaultBranch)
	}
	if baseSha != "" {
		baseBranch = "origin/" + defaultBranch
	} else {
		// origin/<default> 本地未知——尝试 `git fetch origin <default>`。如果失败
		// （离线、无远程），回退到 HEAD，这样至少能从工作树当前提交拉出分支，而不是报错退出。
		_, _, fetchCode := runGit(ctx, repoRoot, "fetch", "origin", defaultBranch)
		if fetchCode == 0 {
			baseBranch = "origin/" + defaultBranch
		} else {
			baseBranch = "HEAD"
		}
		// 通过子进程解析所选 baseBranch 的 SHA——resolveRef 无法在 worktree 共享的
		// commonDir 中找到 FETCH_HEAD 或 HEAD，除非再做更多处理。
		stdout, _, shaCode := runGit(ctx, repoRoot, "rev-parse", baseBranch)
		if shaCode != 0 {
			return nil, fmt.Errorf(`failed to resolve base branch %q: git rev-parse failed`, baseBranch)
		}
		baseSha = trimNewline(stdout)
	}

	// `-B`（大写）会重置被删除的工作区目录遗留的孤儿分支；`-b` 则会直接报错。
	/*
	-B （大写）的选择很关键：如果上次创建后 worktree 目录被手动删了但分支还在， -b （小写）会报错「分支已存在」， -B 直接覆盖。省掉一次 git branch -D 子进程。
	*/
	_, stderr, code := runGit(ctx, repoRoot, "worktree", "add", "-B", worktreeBranch, worktreePath, baseBranch)
	if code != 0 {
		return nil, fmt.Errorf("failed to create worktree: %s", stderr)
	}

	return &CreateResult{
		WorktreePath:   worktreePath,
		WorktreeBranch: worktreeBranch,
		HeadCommit:     baseSha,
		BaseBranch:     baseBranch,
		Existed:        false,
	}, nil
}

// trimNewline 去除末尾的 CR/LF；等价于对单行命令 stdout 做 .trim。保留在本地，
// 避免仅仅为此导入 strings。
func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

package worktree

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// performPostCreationSetup 把主仓库中的设置、钩子、符号链接和 gitignored 文件
// 传播到新建的 worktree。
func performPostCreationSetup(ctx context.Context, repoRoot, worktreePath string) {
	// A. 复制 settings.local.json。
	copySettingsLocal(repoRoot, worktreePath)

	// B. 配置 git hooks 路径。
	configureHooksPath(ctx, repoRoot, worktreePath)

	// C. 符号链接大目录（通过配置选择启用）。
	symlinkDirectories(repoRoot, worktreePath, getSymlinkDirectories())

	// D. 从 .worktreeinclude 复制 gitignored 文件。
	CopyWorktreeIncludeFiles(ctx, repoRoot, worktreePath)
}

// copySettingsLocal 将 .cody/settings.local.json 从主仓库复制到 worktree。这会
// 传播本地设置（其中可能包含机密）。
func copySettingsLocal(repoRoot, worktreePath string) {
	relPath := filepath.Join(".cody", "settings.local.json")
	src := filepath.Join(repoRoot, relPath)
	dst := filepath.Join(worktreePath, relPath)

	srcData, err := os.ReadFile(src)
	if err != nil {
		return // ENOENT 没问题——没有本地设置需要复制
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(dst, srcData, 0o644)
}

// configureHooksPath 在 worktree 中设置 core.hooksPath，使主仓库的 git hooks
// 被共享。优先使用 .husky/，其次 .git/hooks/。
func configureHooksPath(ctx context.Context, repoRoot, worktreePath string) {
	candidates := []string{
		filepath.Join(repoRoot, ".husky"),
		filepath.Join(repoRoot, ".git", "hooks"),
	}
	var hooksPath string
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err == nil && info.IsDir() {
			hooksPath = c
			break
		}
	}
	if hooksPath == "" {
		return
	}
	_, _, code := runGit(ctx, worktreePath, "config", "core.hooksPath", hooksPath)
	if code != 0 {
		// 尽力而为——不要因为失败而中断整个设置。
		return
	}
}

// symlinkDirectories 从 repoRoot 的目录在 worktreePath 中创建符号链接，避免磁盘膨胀
// （例如 node_modules、vendor）。
func symlinkDirectories(repoRoot, worktreePath string, dirs []string) {
	for _, dir := range dirs {
		if strings.Contains(dir, "..") {
			continue // 路径穿越防护
		}
		src := filepath.Join(repoRoot, dir)
		dst := filepath.Join(worktreePath, dir)
		// 符号链接是尽力而为：源目录可能不存在，目标目录可能已存在。
		_ = os.Symlink(src, dst)
	}
}

// getSymlinkDirectories 返回配置的需要符号链接的目录列表。通过
// settings.worktree.symlinkDirectories 配置。可用时从 config 读取；默认空。
func getSymlinkDirectories() []string {
	return worktreeConfig.SymlinkDirectories
}

// CopyWorktreeIncludeFiles 将 .worktreeinclude 中指定的 gitignored 文件从基础仓库
// 复制到 worktree。使用 gitignore 语法模式。
func CopyWorktreeIncludeFiles(ctx context.Context, repoRoot, worktreePath string) ([]string, error) {
	includeFile := filepath.Join(repoRoot, ".worktreeinclude")
	data, err := os.ReadFile(includeFile)
	if err != nil {
		return nil, nil // 没有 .worktreeinclude → 无需复制
	}

	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if len(patterns) == 0 {
		return nil, nil
	}

	// 使用 git ls-files 列出 gitignored 文件。
	stdout, _, code := runGit(ctx, repoRoot,
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory")
	if code != 0 || strings.TrimSpace(stdout) == "" {
		return nil, nil
	}

	entries := strings.Split(strings.TrimSpace(stdout), "\n")

	// 简单模式匹配：对每个 gitignored 文件，检查是否有 .worktreeinclude 模式匹配
	// 它。对基本 glob 匹配使用 filepath.Match，对目录模式使用前缀匹配。
	var toCopy []string
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		// 跳过折叠的目录（以 / 结尾）。
		if strings.HasSuffix(entry, "/") {
			continue
		}
		if matchesWorktreeInclude(entry, patterns) {
			toCopy = append(toCopy, entry)
		}
	}

	var copied []string
	for _, rel := range toCopy {
		src := filepath.Join(repoRoot, rel)
		dst := filepath.Join(worktreePath, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		if err := copyFileContents(src, dst); err != nil {
			continue
		}
		copied = append(copied, rel)
	}
	return copied, nil
}

// matchesWorktreeInclude 检查文件路径是否匹配任何 .worktreeinclude 模式。
// 支持精确匹配、基名匹配和基本 glob 模式。
func matchesWorktreeInclude(path string, patterns []string) bool {
	base := filepath.Base(path)
	for _, p := range patterns {
		p = strings.TrimPrefix(p, "/")
		// 精确匹配。
		if p == path || p == base {
			return true
		}
		// 对完整路径做 glob 匹配。
		if matched, _ := filepath.Match(p, path); matched {
			return true
		}
		// 对基名做 glob 匹配。
		if matched, _ := filepath.Match(p, base); matched {
			return true
		}
		// 目录模式的前缀匹配。
		if strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// WorktreeConfig 保存与 worktree 相关的配置。从 config.yaml 或默认值填充。
var worktreeConfig = struct {
	SymlinkDirectories    []string
	StaleCleanupInterval  int // 秒；0 = 禁用
	StaleCutoffHours      int // 小时；默认 720（30 天）
}{
	StaleCutoffHours: 720,
}

// SetWorktreeConfig 允许 TUI/CLI 启动时注入配置值。
func SetWorktreeConfig(symlinkDirs []string, cleanupIntervalSec, cutoffHours int) {
	worktreeConfig.SymlinkDirectories = symlinkDirs
	worktreeConfig.StaleCleanupInterval = cleanupIntervalSec
	if cutoffHours > 0 {
		worktreeConfig.StaleCutoffHours = cutoffHours
	}
}

// GetStaleCutoffHours 返回配置的截止时间（小时）。
func GetStaleCutoffHours() int {
	return worktreeConfig.StaleCutoffHours
}

// GetStaleCleanupInterval 返回配置的清理间隔（秒）。
func GetStaleCleanupInterval() int {
	return worktreeConfig.StaleCleanupInterval
}

// FindCanonicalGitRoot 通过 worktree 解析出主仓库根目录。从 worktree 内部
// 调用时，沿 .git 指针找到 commondir。
func FindCanonicalGitRoot(startDir string) string {
	gitDir, err := ResolveGitDir(startDir)
	if err != nil || gitDir == "" {
		return ""
	}
	// 如果 gitDir 包含 commondir 指针，沿它找到主仓库。
	commonDir, err := GetCommonDir(gitDir)
	if err != nil || commonDir == "" {
		// gitDir 是主 .git 目录；仓库根目录是它的父目录。
		return filepath.Dir(gitDir)
	}
	// commonDir 指向主仓库的 .git；仓库根目录是它的父目录。
	return filepath.Dir(commonDir)
}


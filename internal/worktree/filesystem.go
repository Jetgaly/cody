// Package worktree 提供文件系统辅助函数：不启动 git 子进程即可读取 git 状态。
//
// 覆盖范围：解析 .git 目录（包括 worktree/submodules）、解析 HEAD、通过松散文件和
// packed-refs 解析 ref。
//
// 正确性说明（对照 git 源码验证）：
// HEAD：`ref: refs/heads/<branch>\n` 或原始 SHA（refs/files-backend.c）
// Packed-refs：`<sha> <refname>\n`，跳过 `#` 和 `^` 开头的行（packed-backend.c）
// git 文件（worktree）：`gitdir: <path>\n`，路径可为相对路径（setup.c）
package worktree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// safeRefName 允许 ASCII 字母数字以及 '/', '.', '_', '+', '-', '@'。用于校验从 .git/ 读取的
// ref/branch 名称，防止被篡改的 HEAD 或 ref 文件注入路径穿越、参数前缀或 shell 元字符。
var safeRefName = regexp.MustCompile(`^[a-zA-Z0-9/._+@-]+$`)

// IsSafeRefName 校验 ref/branch 名称是否可安全用于路径拼接、git 位置参数以及插值进 shell 命令。
func IsSafeRefName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	// 拒绝单独的点和空路径分量。
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == "" {
			return false
		}
	}
	return safeRefName.MatchString(name)
}

var sha1Pattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsValidGitSha 报告 s 是否为完整长度的 git 对象 ID：SHA-1（40 位十六进制）或 SHA-256（64 位十六进制）。
// git 从不向 HEAD 或 ref 文件写入缩写形式的 SHA。
func IsValidGitSha(s string) bool {
	return sha1Pattern.MatchString(s) || sha256Pattern.MatchString(s)
}

// ResolveGitDir 解析以 root 为根目录的仓库的实际 .git 目录。处理 .git 是包含 `gitdir: <path>`
// 的文件的情况（worktree/submodules）。当 root 没有 .git 项（不是仓库）时返回 ("", nil)——
// 调用方把空值视为“不是 git 仓库”。错误仅保留给调用方关心的 IO 失败（此处：除 ENOENT 外的
// 文件系统错误）。
//
// （去掉了记忆化：Go 调用方会在更高层自行缓存。）
func ResolveGitDir(root string) (string, error) {
	gitPath := filepath.Join(root, ".git")
	st, err := os.Stat(gitPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if !st.IsDir() {
		// Worktree 或 submodule：.git 是包含 `gitdir: <path>` 的文件。git 通过
		// strbuf_rtrim（setup.c read_gitfile_gently）去除末尾空白；strings.TrimSpace 与其等价。
		raw, err := os.ReadFile(gitPath)
		if err != nil {
			return "", err
		}
		content := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(content, "gitdir:") {
			return "", nil
		}
		rel := strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
		// 相对于 root（.git 指针文件所在位置）解析相对路径。
		if filepath.IsAbs(rel) {
			return rel, nil
		}
		return filepath.Clean(filepath.Join(root, rel)), nil
	}
	return gitPath, nil
}

// GetCommonDir 读取 worktree 的 gitDir 内的 `commondir` 文件，以找到共享的 git 目录。
// 在 worktree 中，该文件指向主仓库的 .git 目录。若不存在 commondir 文件（普通仓库）则返回 ("", nil)。
func GetCommonDir(gitDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	content := strings.TrimSpace(string(raw))
	if filepath.IsAbs(content) {
		return content, nil
	}
	return filepath.Clean(filepath.Join(gitDir, content)), nil
}

// gitHead 是 <gitDir>/HEAD 的解析结果。
type gitHead struct {
	// branch 在 HEAD 指向某个分支时非空。
	branch string
	// sha 在 HEAD 处于分离状态（原始 SHA）或解析了不常见的 symref 时非空。
	sha string
}

// readGitHead 解析 <gitDir>/HEAD 以确定当前分支或分离的 SHA。当 HEAD 不存在或格式异常时返回
// (nil, nil)——调用方将其视为“不是 worktree”/“不是仓库”。除 ENOENT 外的 IO 错误会继续向上传播。
//
// HEAD 格式（依据 refs/files-backend.c）：
// `ref: refs/heads/<branch>\n` —— 位于某个分支上
// `ref: <other-ref>\n` —— 不常见的 symref（例如 bisect 期间）
// `<hex-sha>\n` —— 分离的 HEAD（例如 rebase 期间）
func readGitHead(gitDir string) (*gitHead, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	content := strings.TrimSpace(string(raw))
	if strings.HasPrefix(content, "ref:") {
		ref := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
		if strings.HasPrefix(ref, "refs/heads/") {
			name := strings.TrimPrefix(ref, "refs/heads/")
			if !IsSafeRefName(name) {
				return nil, nil
			}
			return &gitHead{branch: name}, nil
		}
		// 不常见的 symref（非本地分支）——解析为 SHA。
		if !IsSafeRefName(ref) {
			return nil, nil
		}
		sha, err := ResolveRef(gitDir, ref)
		if err != nil {
			return nil, err
		}
		return &gitHead{sha: sha}, nil
	}
	// 原始 SHA（分离的 HEAD）。进行校验，防止被篡改的 HEAD 将 shell 元字符带入下游上下文。
	if !IsValidGitSha(content) {
		return nil, nil
	}
	return &gitHead{sha: content}, nil
}

// ResolveRef 将 git ref（例如 `refs/heads/main`）解析为提交 SHA。先检查松散 ref 文件，
// 再回退到 packed-refs。会跟随 symref（例如 `ref: refs/remotes/origin/main`）。
//
// 对于 worktree，ref 位于公共 gitdir（由 `commondir` 文件指向），而不是 worktree 专属的 gitdir。
// 我们优先检查 worktree 的 gitdir，然后回退到公共目录。
func ResolveRef(gitDir, ref string) (string, error) {
	sha, err := resolveRefInDir(gitDir, ref)
	if err != nil {
		return "", err
	}
	if sha != "" {
		return sha, nil
	}
	commonDir, err := GetCommonDir(gitDir)
	if err != nil {
		return "", err
	}
	if commonDir != "" && commonDir != gitDir {
		return resolveRefInDir(commonDir, ref)
	}
	return "", nil
}

// resolveRefInDir 在单个 git 目录内解析 ref（不做 commonDir 回退）。
func resolveRefInDir(dir, ref string) (string, error) {
	// 先尝试松散 ref 文件。
	raw, err := os.ReadFile(filepath.Join(dir, ref))
	if err == nil {
		content := strings.TrimSpace(string(raw))
		if strings.HasPrefix(content, "ref:") {
			target := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
			if !IsSafeRefName(target) {
				return "", nil
			}
			// 递归以跟随 symref 链。传入 `dir`（而非 gitDir），使 resolveRef 的 commonDir 回退
			// 从同一出发点生效。
			return ResolveRef(dir, target)
		}
		if !IsValidGitSha(content) {
			return "", nil
		}
		return content, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	// 回退到 packed-refs。
	packed, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		spaceIdx := strings.IndexByte(line, ' ')
		if spaceIdx == -1 {
			continue
		}
		if line[spaceIdx+1:] == ref {
			sha := line[:spaceIdx]
			if !IsValidGitSha(sha) {
				return "", nil
			}
			return sha, nil
		}
	}
	return "", nil
}

// ReadRawSymref 读取原始 symref 文件，并在已知前缀之后提取分支名称。若 ref 不存在、不是
// symref 或不匹配前缀，则返回 ("", nil)。仅检查松散文件——packed-refs 不存储 symref。
func ReadRawSymref(gitDir, refPath, branchPrefix string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, refPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	content := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(content, "ref:") {
		return "", nil
	}
	target := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
	if !strings.HasPrefix(target, branchPrefix) {
		return "", nil
	}
	name := strings.TrimPrefix(target, branchPrefix)
	if !IsSafeRefName(name) {
		return "", nil
	}
	return name, nil
}

// GetDefaultBranch 通过从公共 gitdir 读取 refs/remotes/origin/HEAD（一个 symref）来确定仓库的
// 默认分支；若失败则依次尝试 `main`、`master`，最后回退返回 "main"。
//
// 纯文件系统读取——不启动 git 子进程，也不访问网络。
func GetDefaultBranch(repoRoot string) (string, error) {
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil {
		return "main", err
	}
	if gitDir == "" {
		return "main", nil
	}
	// refs/remotes/ 位于 commonDir，而不是每个 worktree 各自的 gitDir。
	commonDir, err := GetCommonDir(gitDir)
	if err != nil {
		return "main", err
	}
	if commonDir == "" {
		commonDir = gitDir
	}
	branch, err := ReadRawSymref(commonDir, "refs/remotes/origin/HEAD", "refs/remotes/origin/")
	if err != nil {
		return "main", err
	}
	if branch != "" {
		return branch, nil
	}
	for _, candidate := range []string{"main", "master"} {
		sha, err := ResolveRef(commonDir, "refs/remotes/origin/"+candidate)
		if err != nil {
			return "main", err
		}
		if sha != "" {
			return candidate, nil
		}
	}
	return "main", nil
}

// GetCurrentBranch 读取 <repoRoot>/.git/HEAD 并返回当前分支名；当 HEAD 处于分离状态时返回 ""。
// 纯文件系统读取；（通过空字符串而非哨兵值 "HEAD" 来区分分离的 HEAD）。
func GetCurrentBranch(repoRoot string) (string, error) {
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil || gitDir == "" {
		return "", err
	}
	head, err := readGitHead(gitDir)
	if err != nil || head == nil {
		return "", err
	}
	return head.branch, nil
}

// ReadWorktreeHeadSha 读取 git worktree 目录（而非主仓库）的 HEAD SHA。与 ResolveGitDir+readGitHead
// 串联不同，这里直接把 `<worktreePath>/.git` 当作 `gitdir:` 指针文件读取，不做向上遍历。
// 当 worktree 不存在（`.git` 指针 ENOENT）或格式异常时返回 ("", nil)；调用方把空值视为
// “不是有效的 worktree”。
//
// 性能目标：≤10ms（纯文件系统读取，无子进程）。在一个 1600 万对象的仓库里，仅启动 `git
// rev-parse HEAD` 的开销就约 15ms。
func ReadWorktreeHeadSha(worktreePath string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(worktreePath, ".git"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	ptr := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(ptr, "gitdir:") {
		return "", nil
	}
	rel := strings.TrimSpace(strings.TrimPrefix(ptr, "gitdir:"))
	var gitDir string
	if filepath.IsAbs(rel) {
		gitDir = rel
	} else {
		gitDir = filepath.Clean(filepath.Join(worktreePath, rel))
	}
	head, err := readGitHead(gitDir)
	if err != nil {
		return "", err
	}
	if head == nil {
		return "", nil
	}
	if head.branch != "" {
		return ResolveRef(gitDir, "refs/heads/"+head.branch)
	}
	return head.sha, nil
}

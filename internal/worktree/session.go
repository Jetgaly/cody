package worktree

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// WorktreeSession 跟踪活动 worktree 会话的状态。
type WorktreeSession struct {
	OriginalCwd        string `json:"original_cwd"`
	WorktreePath       string `json:"worktree_path"`
	WorktreeName       string `json:"worktree_name"`
	WorktreeBranch     string `json:"worktree_branch,omitempty"`
	OriginalBranch     string `json:"original_branch,omitempty"`
	OriginalHeadCommit string `json:"original_head_commit,omitempty"`
	SessionID          string `json:"session_id"`
	HookBased          bool   `json:"hook_based,omitempty"`
	CreationDurationMs int64  `json:"creation_duration_ms,omitempty"`
}

// 模块级单例 + 互斥锁。
var (
	currentWorktreeSession *WorktreeSession
	sessionMu              sync.RWMutex
)

// GetCurrentWorktreeSession 返回当前活动的 worktree 会话，没有则返回 nil。
func GetCurrentWorktreeSession() *WorktreeSession {
	sessionMu.RLock()
	defer sessionMu.RUnlock()
	return currentWorktreeSession
}

// RestoreWorktreeSession 在 --resume 时恢复会话。调用方必须已经确认目录存在
// 并设置了引导状态。
func RestoreWorktreeSession(session *WorktreeSession) {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	currentWorktreeSession = session
}

// sessionFilePath 返回会话持久化文件的路径。
func sessionFilePath(repoRoot string) string {
	return filepath.Join(repoRoot, ".cody", "worktree_session.json")
}

// SaveWorktreeSession 将会话状态持久化到磁盘。传入 nil 表示清除。
func SaveWorktreeSession(repoRoot string, session *WorktreeSession) error {
	path := sessionFilePath(repoRoot)
	if session == nil {
		_ = os.Remove(path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadWorktreeSession 从磁盘读取先前持久化的会话。如果文件不存在，返回 (nil, nil)。
func LoadWorktreeSession(repoRoot string) (*WorktreeSession, error) {
	path := sessionFilePath(repoRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var session WorktreeSession
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

// CreateWorktreeForSession 创建或恢复一个 worktree，并设置全局会话单例。
func CreateWorktreeForSession(ctx context.Context, sessionID, slug, repoRoot string) (*WorktreeSession, error) {
	if err := ValidateWorktreeSlug(slug); err != nil {
		return nil, err
	}

	if repoRoot == "" {
		return nil, errorf("cannot create a worktree: not in a git repository")
	}

	originalCwd, _ := os.Getwd()
	originalBranch, _ := GetCurrentBranch(repoRoot)

	start := time.Now()
	result, err := getOrCreateWorktree(ctx, repoRoot, slug)
	if err != nil {
		return nil, err
	}

	var creationDurationMs int64
	if !result.Existed {
		performPostCreationSetup(ctx, repoRoot, result.WorktreePath)
		creationDurationMs = time.Since(start).Milliseconds()
	}

	session := &WorktreeSession{
		OriginalCwd:        originalCwd,
		WorktreePath:       result.WorktreePath,
		WorktreeName:       slug,
		WorktreeBranch:     result.WorktreeBranch,
		OriginalBranch:     originalBranch,
		OriginalHeadCommit: result.HeadCommit,
		SessionID:          sessionID,
		CreationDurationMs: creationDurationMs,
	}

	sessionMu.Lock()
	currentWorktreeSession = session
	sessionMu.Unlock()

	_ = SaveWorktreeSession(repoRoot, session)
	return session, nil
}

// KeepWorktree 保留磁盘上的 worktree，并清除会话状态。
func KeepWorktree(repoRoot string) error {
	sessionMu.Lock()
	session := currentWorktreeSession
	currentWorktreeSession = nil
	sessionMu.Unlock()

	if session == nil {
		return nil
	}

	if err := os.Chdir(session.OriginalCwd); err != nil {
		return err
	}

	_ = SaveWorktreeSession(repoRoot, nil)
	return nil
}

// CleanupWorktree 删除 worktree 及其临时分支，然后清除会话状态。
func CleanupWorktree(ctx context.Context, repoRoot string) error {
	sessionMu.Lock()
	session := currentWorktreeSession
	currentWorktreeSession = nil
	sessionMu.Unlock()

	if session == nil {
		return nil
	}

	if err := os.Chdir(session.OriginalCwd); err != nil {
		return err
	}

	// 通过 git 删除 worktree 目录。
	_, _, code := runGit(ctx, session.OriginalCwd,
		"worktree", "remove", "--force", session.WorktreePath)
	if code != 0 {
		// 尽力而为：继续执行分支清理。
	}

	// 删除临时分支。
	if session.WorktreeBranch != "" {
		// 等待 git 锁文件释放。
		time.Sleep(100 * time.Millisecond)
		runGit(ctx, session.OriginalCwd, "branch", "-D", session.WorktreeBranch)
	}

	_ = SaveWorktreeSession(repoRoot, nil)
	return nil
}

func errorf(format string, args ...any) error {
	return &worktreeError{msg: format}
}

type worktreeError struct {
	msg string
}

func (e *worktreeError) Error() string { return e.msg }

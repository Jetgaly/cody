package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCreateAgentWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	// CreateAgentWorktree 需要在 git 仓库内调用
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	result, err := CreateAgentWorktree(context.Background(), "agent-a1234567")
	if err != nil {
		t.Fatalf("CreateAgentWorktree failed: %v", err)
	}

	expectedPath := filepath.Join(repo, ".cody", "worktrees", "agent-a1234567")
	if result.WorktreePath != expectedPath {
		t.Fatalf("expected path %q, got %q", expectedPath, result.WorktreePath)
	}
	if result.GitRoot != repo {
		t.Fatalf("expected git root %q, got %q", repo, result.GitRoot)
	}
	if result.HeadCommit == "" {
		t.Fatal("expected non-empty head commit")
	}

	// 目录应该存在
	if _, err := os.Stat(result.WorktreePath); err != nil {
		t.Fatalf("worktree directory not created: %v", err)
	}

	// 不应设置会话单例（agent worktree 无会话）
	if s := GetCurrentWorktreeSession(); s != nil {
		t.Fatal("CreateAgentWorktree should not touch global session")
	}
}

func TestCreateAgentWorktree_Resume(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	// 第一次调用负责创建
	r1, err := CreateAgentWorktree(context.Background(), "agent-a7777777")
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	// 第二次调用应恢复（mtime 被刷新）
	r2, err := CreateAgentWorktree(context.Background(), "agent-a7777777")
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if r2.WorktreePath != r1.WorktreePath {
		t.Fatal("resume should return same path")
	}
}

func TestRemoveAgentWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	result, err := CreateAgentWorktree(context.Background(), "agent-aabcdef0")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	ok := RemoveAgentWorktree(context.Background(), result.WorktreePath, result.WorktreeBranch, result.GitRoot)
	if !ok {
		t.Fatal("RemoveAgentWorktree returned false")
	}

	// 目录应该已不存在
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("worktree directory should be removed")
	}
}

func TestRemoveAgentWorktree_NoGitRoot(t *testing.T) {
	ok := RemoveAgentWorktree(context.Background(), "/tmp/nonexistent", "branch", "")
	if ok {
		t.Fatal("expected false when gitRoot is empty")
	}
}

package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHasWorktreeChanges_Clean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	// 获取 HEAD 提交
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	head := trimNewline(string(out))

	// 干净的仓库应返回 false
	if HasWorktreeChanges(context.Background(), repo, head) {
		t.Fatal("expected no changes in clean repo")
	}
}

func TestHasWorktreeChanges_Dirty(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, _ := cmd.Output()
	head := trimNewline(string(out))

	// 创建一个未提交的文件
	os.WriteFile(filepath.Join(repo, "dirty.txt"), []byte("dirty"), 0o644)

	if !HasWorktreeChanges(context.Background(), repo, head) {
		t.Fatal("expected changes with uncommitted file")
	}
}

func TestHasWorktreeChanges_NewCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, _ := cmd.Output()
	head := trimNewline(string(out))

	// 新增一个提交
	os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new"), 0o644)
	exec.Command("git", "-C", repo, "add", ".").Run()
	exec.Command("git", "-C", repo, "commit", "-m", "new").Run()

	if !HasWorktreeChanges(context.Background(), repo, head) {
		t.Fatal("expected changes with new commit")
	}
}

func TestHasWorktreeChanges_FailClosed(t *testing.T) {
	// 不存在的路径应返回 true（故障关闭）
	if !HasWorktreeChanges(context.Background(), "/nonexistent-path-xyz", "abc123") {
		t.Fatal("expected true for non-existent path (fail-closed)")
	}
}

func TestCountWorktreeChanges_Clean(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, _ := cmd.Output()
	head := trimNewline(string(out))

	summary := CountWorktreeChanges(context.Background(), repo, head)
	if summary == nil {
		t.Fatal("expected non-nil summary for clean repo")
	}
	if summary.ChangedFiles != 0 || summary.Commits != 0 {
		t.Fatalf("expected 0/0, got %d/%d", summary.ChangedFiles, summary.Commits)
	}
}

func TestCountWorktreeChanges_EmptyHeadCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	// 空的 originalHeadCommit 应返回 nil（故障关闭）
	summary := CountWorktreeChanges(context.Background(), repo, "")
	if summary != nil {
		t.Fatal("expected nil with empty head commit (fail-closed)")
	}
}

func TestCountWorktreeChanges_FailClosed(t *testing.T) {
	summary := CountWorktreeChanges(context.Background(), "/nonexistent-path-xyz", "abc123")
	if summary != nil {
		t.Fatal("expected nil for non-existent path (fail-closed)")
	}
}

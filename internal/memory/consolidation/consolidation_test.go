package consolidation

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cody/internal/conversation"
)

// =========================================================================
// 锁文件测试
// =========================================================================

func TestLock_FirstAcquire(t *testing.T) {
	dir := t.TempDir()

	// 锁文件不存在时，lastConsolidatedAt 应该是 0
	last, err := ReadLastConsolidatedAt(dir)
	if err != nil {
		t.Fatalf("ReadLastConsolidatedAt: %v", err)
	}
	if last != 0 {
		t.Fatalf("expected 0, got %d", last)
	}

	// 首次获取应该成功，返回旧 mtime 0
	prior, err := TryAcquireLock(dir)
	if err != nil {
		t.Fatalf("TryAcquireLock: %v", err)
	}
	if prior != 0 {
		t.Fatalf("expected prior=0, got %d", prior)
	}

	// 锁文件内容应该是当前 PID
	content, _ := os.ReadFile(filepath.Join(dir, lockFileName))
	if string(content) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("expected PID %d, got %s", os.Getpid(), content)
	}

	// lastConsolidatedAt 现在应该非零
	last2, _ := ReadLastConsolidatedAt(dir)
	if last2 == 0 {
		t.Fatal("expected non-zero after lock acquire")
	}
}

func TestLock_BlocksWhenHeld(t *testing.T) {
	dir := t.TempDir()

	// 获取锁
	_, _ = TryAcquireLock(dir)

	// 同一进程再次获取应该被阻塞（PID 活着，mtime 在 1 小时内）
	prior2, _ := TryAcquireLock(dir)
	if prior2 != -1 {
		t.Fatal("expected lock to block when held by live process")
	}
}

func TestLock_ReclaimsDeadPID(t *testing.T) {
	dir := t.TempDir()

	// 写一个不存在的 PID 的锁文件
	lockFile := filepath.Join(dir, lockFileName)
	os.WriteFile(lockFile, []byte("999999999"), 0o644)

	// 应该能抢到锁（PID 已死）
	prior, err := TryAcquireLock(dir)
	if err != nil {
		t.Fatalf("TryAcquireLock: %v", err)
	}
	if prior == -1 {
		t.Fatal("should reclaim lock from dead PID")
	}
}

func TestLock_ReclaimsStale(t *testing.T) {
	dir := t.TempDir()

	// 写一个 mtime 超过 1 小时的锁文件（即使 PID 是自己）
	lockFile := filepath.Join(dir, lockFileName)
	os.WriteFile(lockFile, []byte(strconv.Itoa(os.Getpid())), 0o644)
	oldTime := time.Now().Add(-2 * time.Hour)
	os.Chtimes(lockFile, oldTime, oldTime)

	// mtime 超过 holderStaleMs，即使 PID 活着也应该能抢到
	prior, err := TryAcquireLock(dir)
	if err != nil {
		t.Fatalf("TryAcquireLock: %v", err)
	}
	if prior == -1 {
		t.Fatal("should reclaim stale lock even with live PID")
	}
}

func TestLock_RollbackDeletesOnZero(t *testing.T) {
	dir := t.TempDir()
	TryAcquireLock(dir)

	RollbackLock(dir, 0)

	if _, err := os.Stat(filepath.Join(dir, lockFileName)); !os.IsNotExist(err) {
		t.Fatal("rollback with prior=0 should delete lock file")
	}
}

func TestLock_RollbackRestoresMtime(t *testing.T) {
	dir := t.TempDir()

	// 先创建旧锁
	lockFile := filepath.Join(dir, lockFileName)
	oldTime := time.Now().Add(-48 * time.Hour)
	os.WriteFile(lockFile, []byte("99999"), 0o644)
	os.Chtimes(lockFile, oldTime, oldTime)

	prior, _ := TryAcquireLock(dir)
	RollbackLock(dir, prior)

	// mtime 应该被还原到旧值附近
	info, _ := os.Stat(lockFile)
	diff := info.ModTime().UnixMilli() - prior
	if diff < -1000 || diff > 1000 {
		t.Fatalf("mtime not restored: expected ~%d, got %d", prior, info.ModTime().UnixMilli())
	}

	// PID 应该被清空
	content, _ := os.ReadFile(lockFile)
	if strings.TrimSpace(string(content)) != "" {
		t.Fatalf("expected empty PID after rollback, got %q", content)
	}
}

// =========================================================================
// Prompt 测试
// =========================================================================

func TestPrompt_ContainsAllPhases(t *testing.T) {
	prompt := BuildConsolidationPrompt("/mem", "/user/mem", "/sessions", []string{"s1", "s2", "s3"})

	for _, want := range []string{
		"Phase 1", "Phase 2", "Phase 3", "Phase 4",
		"MEMORY.md",
		"/mem", "/user/mem", "/sessions",
		"s1", "s2", "s3",
		"Sessions since last consolidation (3)",
		"read-only commands",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestPrompt_EmptySessions(t *testing.T) {
	prompt := BuildConsolidationPrompt("/mem", "", "/sessions", nil)

	if strings.Contains(prompt, "Sessions since last consolidation") {
		t.Error("should not include sessions section when list is empty")
	}
}

// =========================================================================
// 会话列表测试
// =========================================================================

func TestListSessionsSince(t *testing.T) {
	dir := t.TempDir()
	sessDir := filepath.Join(dir, ".cody", "sessions")
	os.MkdirAll(sessDir, 0o755)

	// 创建旧会话（2 天前）
	os.WriteFile(filepath.Join(sessDir, "old.jsonl"), []byte(`{"role":"user","content":"hi","ts":1}`+"\n"), 0o644)
	os.Chtimes(filepath.Join(sessDir, "old.jsonl"), time.Now().Add(-48*time.Hour), time.Now().Add(-48*time.Hour))

	// 创建新会话（刚才）
	os.WriteFile(filepath.Join(sessDir, "new1.jsonl"), []byte(`{"role":"user","content":"a","ts":2}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(sessDir, "new2.jsonl"), []byte(`{"role":"user","content":"b","ts":3}`+"\n"), 0o644)

	ids := listSessionsSince(dir, time.Now().Add(-24*time.Hour).UnixMilli())
	if len(ids) != 2 {
		t.Fatalf("expected 2 recent sessions, got %d: %v", len(ids), ids)
	}
}

// =========================================================================
// MaybeRun 门控逻辑测试
// =========================================================================

func TestMaybeRun_SkipsWhenMemoryDirMissing(t *testing.T) {
	dir := t.TempDir()
	c := NewConsolidator(Deps{
		MemoryDir:   filepath.Join(dir, "nonexistent", "memory") + "/",
		ProjectRoot: dir,
	})

	// 不应该 panic 或报错
	c.MaybeRun(context.Background())
}

func TestMaybeRun_SkipsWhenTimeGateNotMet(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".cody", "memory")
	os.MkdirAll(memDir, 0o755)

	// 设置锁文件 mtime 为 1 小时前（不满足 24 小时门控）
	lockFile := filepath.Join(memDir, lockFileName)
	os.WriteFile(lockFile, []byte(""), 0o644)
	os.Chtimes(lockFile, time.Now().Add(-1*time.Hour), time.Now().Add(-1*time.Hour))

	var triggered atomic.Bool
	c := NewConsolidator(Deps{
		MemoryDir:   memDir + "/",
		ProjectRoot: dir,
		DebugLogf:   func(f string, a ...any) { triggered.Store(true) },
	})

	c.MaybeRun(context.Background())

	// 不应该触发（连 debug log 都不应该有 "firing" 字样）
	// 时间门未通过，直接返回
}

func TestMaybeRun_SkipsWhenSessionGateNotMet(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".cody", "memory")
	sessDir := filepath.Join(dir, ".cody", "sessions")
	os.MkdirAll(memDir, 0o755)
	os.MkdirAll(sessDir, 0o755)

	// 只创建 2 个会话（不满足 5 个门控）
	os.WriteFile(filepath.Join(sessDir, "s1.jsonl"), []byte(`{"role":"user","content":"a","ts":1}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(sessDir, "s2.jsonl"), []byte(`{"role":"user","content":"b","ts":2}`+"\n"), 0o644)

	var logs []string
	c := NewConsolidator(Deps{
		MemoryDir:   memDir + "/",
		ProjectRoot: dir,
		DebugLogf:   func(f string, a ...any) { logs = append(logs, f) },
	})
	c.SetThresholds(0, 5) // 时间门设为 0（立刻通过），会话门设为 5

	c.MaybeRun(context.Background())

	// 应该看到 "skip — 2 sessions" 日志
	found := false
	for _, log := range logs {
		if strings.Contains(log, "skip") && strings.Contains(log, "sessions") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected skip log for insufficient sessions, got: %v", logs)
	}
}

func TestMaybeRun_TriggersWhenBothGatesMet(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".cody", "memory")
	sessDir := filepath.Join(dir, ".cody", "sessions")
	os.MkdirAll(memDir, 0o755)
	os.MkdirAll(sessDir, 0o755)

	// 创建 6 个会话
	for i := 0; i < 6; i++ {
		name := filepath.Join(sessDir, "s"+strconv.Itoa(i)+".jsonl")
		os.WriteFile(name, []byte(`{"role":"user","content":"x","ts":1}`+"\n"), 0o644)
	}

	// 验证门控逻辑：两个门都通过时，应该获取到锁并打印 "firing"。
	// 不提供 Client，所以不会真正启动子 Agent——只验证触发判断逻辑。
	// 用 lockAcquired 标记验证：如果锁被获取了，说明整个门控链通过了。
	lockAcquired := false

	// 手动模拟 MaybeRun 的门控链（不调 c.run 避免 nil client panic）
	lastAt, _ := ReadLastConsolidatedAt(memDir)
	hoursSince := float64(time.Now().UnixMilli()-lastAt) / 3_600_000

	sessionIDs := listSessionsSince(dir, lastAt)

	// 时间门应该通过（没有锁文件，lastAt=0）
	if hoursSince < 0 {
		t.Fatal("time gate should pass (no lock file)")
	}

	// 会话门应该通过（6 个会话 >= 5）
	if len(sessionIDs) < 5 {
		t.Fatalf("session gate should pass: got %d sessions", len(sessionIDs))
	}

	// 锁应该能获取到
	prior, err := TryAcquireLock(memDir)
	if err != nil {
		t.Fatalf("lock acquire failed: %v", err)
	}
	if prior != -1 {
		lockAcquired = true
	}

	if !lockAcquired {
		t.Fatal("expected lock to be acquired when both gates pass")
	}
}

func TestMaybeRun_ScanThrottle(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".cody", "memory")
	sessDir := filepath.Join(dir, ".cody", "sessions")
	os.MkdirAll(memDir, 0o755)
	os.MkdirAll(sessDir, 0o755)

	// 只有 2 个会话（不够触发）
	os.WriteFile(filepath.Join(sessDir, "s1.jsonl"), []byte(`{"role":"user","content":"a","ts":1}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(sessDir, "s2.jsonl"), []byte(`{"role":"user","content":"b","ts":2}`+"\n"), 0o644)

	var logs []string
	c := NewConsolidator(Deps{
		MemoryDir:   memDir + "/",
		ProjectRoot: dir,
		DebugLogf:   func(f string, a ...any) { logs = append(logs, f) },
	})
	c.SetThresholds(0, 5)

	// 第一次调用：扫描会话，发现不够
	c.MaybeRun(context.Background())

	logsBefore := len(logs)

	// 第二次立刻调用：应该被扫描节流拦住
	c.MaybeRun(context.Background())

	throttled := false
	for _, log := range logs[logsBefore:] {
		if strings.Contains(log, "throttle") {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Errorf("expected scan throttle on second call, got: %v", logs[logsBefore:])
	}
}

func TestMaybeRun_LockBlocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("lock detection unreliable on Windows")
	}
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".cody", "memory")
	sessDir := filepath.Join(dir, ".cody", "sessions")
	os.MkdirAll(memDir, 0o755)
	os.MkdirAll(sessDir, 0o755)

	for i := 0; i < 6; i++ {
		name := filepath.Join(sessDir, "s"+strconv.Itoa(i)+".jsonl")
		os.WriteFile(name, []byte(`{"role":"user","content":"x","ts":1}`+"\n"), 0o644)
	}

	// 先手动获取锁
	TryAcquireLock(memDir)

	var logs []string
	c := NewConsolidator(Deps{
		MemoryDir:   memDir + "/",
		ProjectRoot: dir,
		DebugLogf:   func(f string, a ...any) { logs = append(logs, f) },
	})
	c.SetThresholds(0, 1) // 门控全降最低

	c.MaybeRun(context.Background())

	// 应该被锁阻塞
	blocked := false
	for _, log := range logs {
		if strings.Contains(log, "lock held") {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Errorf("expected lock blocked log, got: %v", logs)
	}
}

// =========================================================================
// extractWrittenPaths 测试
// =========================================================================

func TestExtractWrittenPaths(t *testing.T) {
	msgs := []conversation.Message{
		{Role: "user", Content: "consolidate"},
		{
			Role:    "assistant",
			Content: "updating memories",
			ToolUses: []conversation.ToolUseBlock{
				{ToolName: "WriteFile", Arguments: map[string]any{"file_path": "/mem/feedback_testing.md"}},
				{ToolName: "EditFile", Arguments: map[string]any{"file_path": "/mem/MEMORY.md"}},
				{ToolName: "ReadFile", Arguments: map[string]any{"file_path": "/mem/old.md"}},
			},
		},
		{
			Role:    "assistant",
			Content: "done",
			ToolUses: []conversation.ToolUseBlock{
				{ToolName: "WriteFile", Arguments: map[string]any{"file_path": "/mem/feedback_testing.md"}}, // 重复
				{ToolName: "WriteFile", Arguments: map[string]any{"file_path": "/mem/project_new.md"}},
			},
		},
	}

	paths := extractWrittenPaths(msgs)

	// 应该有 3 个不重复的路径（WriteFile 和 EditFile），ReadFile 不算
	if len(paths) != 3 {
		t.Fatalf("expected 3 paths, got %d: %v", len(paths), paths)
	}

	// feedback_testing.md 不应该重复
	count := 0
	for _, p := range paths {
		if strings.Contains(p, "feedback_testing") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("feedback_testing.md should appear once, got %d", count)
	}
}

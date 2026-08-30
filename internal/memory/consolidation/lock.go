package consolidation

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const lockFileName = ".consolidate-lock"

// holderStaleMs 是锁的最大持有时间，超过后即使 PID 还活着也视为过期（防 PID 复用）
const holderStaleMs = 60 * 60 * 1000

func lockPath(memoryDir string) string {
	return filepath.Join(memoryDir, lockFileName)
}

// ReadLastConsolidatedAt 返回上次整理完成的时间戳（锁文件的 mtime）。
// 锁文件不存在则返回 0。每轮检查成本：一次 stat。
func ReadLastConsolidatedAt(memoryDir string) (int64, error) {
	info, err := os.Stat(lockPath(memoryDir))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return info.ModTime().UnixMilli(), nil
}

// TryAcquireLock 尝试获取整理锁。成功时返回获取前的 mtime（用于失败回滚），
// 失败时返回 -1（别人正在持有）。
//
// 获取流程：
//  1. 读锁文件的 mtime 和 PID
//  2. 如果文件存在、mtime 距今 < 1 小时、且 PID 进程还活着 → 放弃
//  3. 否则写入自己的 PID
//  4. 回读验证，PID 不是自己 → 竞争失败
func TryAcquireLock(memoryDir string) (priorMtime int64, err error) {
	path := lockPath(memoryDir)

	var mtimeMs int64
	var holderPid int
	var fileExists bool

	if info, statErr := os.Stat(path); statErr == nil {
		fileExists = true
		mtimeMs = info.ModTime().UnixMilli()
		if raw, readErr := os.ReadFile(path); readErr == nil {
			if pid, parseErr := strconv.Atoi(strings.TrimSpace(string(raw))); parseErr == nil {
				holderPid = pid
			}
		}
	}

	if fileExists && time.Now().UnixMilli()-mtimeMs < holderStaleMs {
		if holderPid > 0 && isProcessRunning(holderPid) {
			return -1, nil
		}
	}

	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return -1, fmt.Errorf("write lock: %w", err)
	}

	verify, err := os.ReadFile(path)
	if err != nil {
		return -1, fmt.Errorf("verify lock: %w", err)
	}
	if strings.TrimSpace(string(verify)) != strconv.Itoa(os.Getpid()) {
		return -1, nil
	}

	if fileExists {
		return mtimeMs, nil
	}
	return 0, nil
}

// RollbackLock 将锁文件的 mtime 回退到获取前的值，用于整理失败后恢复。
// priorMtime 为 0 时直接删除锁文件。
func RollbackLock(memoryDir string, priorMtime int64) {
	path := lockPath(memoryDir)
	if priorMtime == 0 {
		os.Remove(path)
		return
	}
	// 清空 PID，防止自己的 PID 被误认为还在持有
	os.WriteFile(path, []byte(""), 0o644)
	t := time.UnixMilli(priorMtime)
	os.Chtimes(path, t, t)
}

// isProcessRunning 在平台专属文件中实现（lock_unix.go / lock_windows.go）

package teams

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// lockAcquireTimeout 是等待文件锁的总时限。到点返回错误交给调用方处理，
	// 不能悄悄把这条消息扔掉。
	lockAcquireTimeout = 5 * time.Second
	// staleLockAge 超过这个时长的锁文件视为持有者已经崩溃，可以强行接管。
	staleLockAge = 10 * time.Second
	// maxLockBackoff 限制退避上限，避免高并发下越退越久。
	maxLockBackoff = 80 * time.Millisecond
)

type FileMailBox struct {
	baseDir string
	// 同进程内的并发直接用内存锁串行化，文件锁只负责隔离独立进程的 teammate。
	// 省掉一轮文件系统争抢，也避免同进程的 goroutine 互相把重试预算耗光。
	mu sync.Mutex
}

type FileMailMessage struct {
	From      string `json:"from"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	Read      bool   `json:"read"`
	Color     string `json:"color,omitempty"`

	// 结构化消息用的三个字段，普通文本消息留空。
	// Type 见 protocol.go 里的 Msg* 常量；RequestID 让应答能对上请求；
	// Approve 用指针是为了区分「没表态」和「明确拒绝」。
	Type      string `json:"type,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	Approve   *bool  `json:"approve,omitempty"`
}

// NewFileMailMessage 构造一条普通文本消息，时间戳按 RFC3339Nano 记。
func NewFileMailMessage(from, text string) FileMailMessage {
	return FileMailMessage{
		From:      from,
		Text:      text,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func NewFileMailBox(baseDir string) *FileMailBox {
	os.MkdirAll(baseDir, 0755)
	return &FileMailBox{baseDir: baseDir}
}

func (mb *FileMailBox) inboxPath(agentID string) string {
	return filepath.Join(mb.baseDir, agentID+".json")
}

func (mb *FileMailBox) lockPath(agentID string) string {
	return filepath.Join(mb.baseDir, agentID+".json.lock")
}

func (mb *FileMailBox) Send(recipient string, msg FileMailMessage) error {
	return mb.withLock(recipient, func(messages []FileMailMessage) ([]FileMailMessage, error) {
		msg.Read = false
		if msg.Timestamp == "" {
			msg.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
		}
		return append(messages, msg), nil
	})
}

func (mb *FileMailBox) ReadUnread(agentID string) ([]FileMailMessage, error) {
	messages, err := mb.readInbox(agentID)
	if err != nil {
		return nil, err
	}
	var unread []FileMailMessage
	for _, m := range messages {
		if !m.Read {
			unread = append(unread, m)
		}
	}
	return unread, nil
}

func (mb *FileMailBox) MarkAllRead(agentID string) error {
	return mb.withLock(agentID, func(messages []FileMailMessage) ([]FileMailMessage, error) {
		for i := range messages {
			messages[i].Read = true
		}
		return messages, nil
	})
}

// withLock 获取文件锁、读取收件箱、应用变更并写回。
func (mb *FileMailBox) withLock(agentID string, fn func([]FileMailMessage) ([]FileMailMessage, error)) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	lockFile := mb.lockPath(agentID)

	// 抢文件锁：退避时间指数增长并带抖动，避免多个进程醒在同一时刻反复对撞。
	// 总时限内抢不到就返回错误，让调用方知道这条消息没写进去。
	var lockFd *os.File
	var err error
	deadline := time.Now().Add(lockAcquireTimeout)
	backoff := 5 * time.Millisecond
	for {
		lockFd, err = os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return err
		}
		// 锁被别人持有，先看它是不是已经陈旧到可以接管
		if info, statErr := os.Stat(lockFile); statErr == nil {
			if time.Since(info.ModTime()) > staleLockAge {
				os.Remove(lockFile)
				continue
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mailbox %s: 等待文件锁超过 %s，消息未写入", agentID, lockAcquireTimeout)
		}
		time.Sleep(backoff + time.Duration(rand.Int63n(int64(backoff))))
		if backoff < maxLockBackoff {
			backoff *= 2
		}
	}
	lockFd.Close()
	defer os.Remove(lockFile)

	// 获取锁后重新读取收件箱
	messages, _ := mb.readInbox(agentID)

	// 应用变更
	messages, err = fn(messages)
	if err != nil {
		return err
	}

	// 写回
	return mb.writeInbox(agentID, messages)
}

func (mb *FileMailBox) readInbox(agentID string) ([]FileMailMessage, error) {
	path := mb.inboxPath(agentID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var messages []FileMailMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, nil
	}
	return messages, nil
}

func (mb *FileMailBox) writeInbox(agentID string, messages []FileMailMessage) error {
	path := mb.inboxPath(agentID)
	data, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return err
	}
	/*
	`os.WriteFile(path, data, perm)` 在 Linux 上行为：
	1. 创建一个**临时文件**，把全部内容写入临时文件
	2. 调用 `rename()`，原子 rename 覆盖旧文件。
	*/
	return os.WriteFile(path, data, 0644)
}


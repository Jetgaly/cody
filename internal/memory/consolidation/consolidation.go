// Package consolidation 实现后台记忆整理（autoDream）。
//
// 满足两个门控条件后自动触发：距上次整理超过 minHours 小时，
// 且期间累积了 minSessions 个以上会话。触发后 fork 一个子 Agent
// 在后台扫描现有记忆，合并重复、删除过时、修正矛盾、维护索引。
package consolidation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cody/internal/agent"
	"cody/internal/agents"
	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/memory"
	"cody/internal/permissions"
	"cody/internal/session"
	"cody/internal/tools"
)

const (
	defaultMinHours    = 24
	defaultMinSessions = 5
	// scanThrottleMs 防止时间门通过但会话门未通过时每轮都扫会话目录
	scanThrottleMs = 10 * 60 * 1000
)

// Deps 是 Consolidator 的外部依赖。
type Deps struct {
	MemoryDir     string                // <wd>/.cody/memory/
	UserMemoryDir string                // ~/.cody/memory/
	ProjectRoot   string                // 项目根目录绝对路径
	Client        llm.Client            // LLM 客户端
	ToolRegistry  *tools.Registry       // 父 Agent 的工具注册表
	Protocol      string                // "anthropic" / "openai"
	Conversation  *conversation.Manager // 父 Agent 对话
	AppendSystem  func(string)          // 通知 TUI
	DebugLogf     func(string, ...any)  // 调试日志
}

// Consolidator 管理后台记忆整理的状态和执行。
type Consolidator struct {
	deps           Deps
	lastScanAt     int64 // 上次扫描会话目录的时间（ms）
	minHours       int
	minSessions    int
}

// NewConsolidator 创建一个新的整理器。
func NewConsolidator(deps Deps) *Consolidator {
	return &Consolidator{
		deps:        deps,
		minHours:    defaultMinHours,
		minSessions: defaultMinSessions,
	}
}

// SetThresholds 用于测试：覆盖默认的门控阈值。
func (c *Consolidator) SetThresholds(minHours, minSessions int) {
	c.minHours = minHours
	c.minSessions = minSessions
}

// MaybeRun 检查门控条件，满足则执行一次后台整理。
// 每轮 Agent Loop 完成后调用，成本极低（一次 stat）。
func (c *Consolidator) MaybeRun(ctx context.Context) {
	if c == nil {
		return
	}
	// 记忆目录不存在则跳过
	if _, err := os.Stat(strings.TrimRight(c.deps.MemoryDir, string(filepath.Separator))); os.IsNotExist(err) {
		return
	}

	// 时间门：距上次整理是否超过阈值
	lastAt, err := ReadLastConsolidatedAt(c.deps.MemoryDir)
	if err != nil {
		c.debugf("[consolidation] ReadLastConsolidatedAt failed: %v", err)
		return
	}
	hoursSince := float64(time.Now().UnixMilli()-lastAt) / 3_600_000
	if hoursSince < float64(c.minHours) {
		return
	}

	// 扫描节流：防止每轮都扫会话目录
	now := time.Now().UnixMilli()
	if now-c.lastScanAt < scanThrottleMs {
		c.debugf("[consolidation] scan throttle — last scan %ds ago", (now-c.lastScanAt)/1000)
		return
	}
	c.lastScanAt = now

	// 会话门：累积的会话数是否达到阈值
	sessionIDs := listSessionsSince(c.deps.ProjectRoot, lastAt)
	if len(sessionIDs) < c.minSessions {
		c.debugf("[consolidation] skip — %d sessions since last consolidation, need %d",
			len(sessionIDs), c.minSessions)
		return
	}

	// 获取锁
	priorMtime, err := TryAcquireLock(c.deps.MemoryDir)
	if err != nil {
		c.debugf("[consolidation] lock acquire failed: %v", err)
		return
	}
	if priorMtime == -1 {
		c.debugf("[consolidation] lock held by another process")
		return
	}

	c.debugf("[consolidation] firing — %.1fh since last, %d sessions to review",
		hoursSince, len(sessionIDs))

	go c.run(ctx, sessionIDs, priorMtime)
}

func (c *Consolidator) run(ctx context.Context, sessionIDs []string, priorMtime int64) {
	defer func() {
		if r := recover(); r != nil {
			c.debugf("[consolidation] panic: %v", r)
			RollbackLock(c.deps.MemoryDir, priorMtime)
		}
	}()

	transcriptDir := filepath.Join(c.deps.ProjectRoot, ".cody", "sessions")
	prompt := BuildConsolidationPrompt(
		c.deps.MemoryDir, c.deps.UserMemoryDir,
		transcriptDir, sessionIDs,
	)

	// 构建独立对话：不继承父 Agent 上下文，从空白对话开始
	conv := conversation.NewManager()
	conv.AddUserMessage(prompt)

	// 工具过滤：给整理子 Agent async 级别的工具集
	subRegistry := agents.FilterToolsForAgent(c.deps.ToolRegistry, nil, nil, true)

	// 路径沙箱：只允许写入记忆目录
	sandboxRoots := []string{c.deps.MemoryDir}
	if c.deps.UserMemoryDir != "" {
		sandboxRoots = append(sandboxRoots, c.deps.UserMemoryDir)
	}
	subSandbox := permissions.NewPathSandbox(sandboxRoots[0], sandboxRoots[1:]...)
	subChecker := permissions.NewChecker(subSandbox, &permissions.RuleEngine{}, permissions.ModeBypass)

	subAgent := agent.New(c.deps.Client, subRegistry, c.deps.Protocol)
	subAgent.MaxIterations = 15 // 整理可能需要多轮读写
	subAgent.Checker = subChecker
	subAgent.WorkDir = c.deps.ProjectRoot

	startTime := time.Now()
	ch := subAgent.Run(ctx, conv)
	for range ch {
		// 排空
	}

	writtenPaths := extractWrittenPaths(conv.GetMessages())
	c.debugf("[consolidation] finished in %s, %d files touched: %v",
		time.Since(startTime), len(writtenPaths), writtenPaths)

	// 过滤掉索引文件，只通知实际的记忆文件修改
	var memoryPaths []string
	for _, p := range writtenPaths {
		if filepath.Base(p) == memory.AutoMemEntrypointName {
			continue
		}
		memoryPaths = append(memoryPaths, p)
	}

	if len(memoryPaths) > 0 && c.deps.AppendSystem != nil {
		var names []string
		for _, p := range memoryPaths {
			names = append(names, filepath.Base(p))
		}
		c.deps.AppendSystem(fmt.Sprintf("Memory improved: %s", strings.Join(names, ", ")))
	}
}

// listSessionsSince 返回 sinceMs 之后被修改过的会话 ID 列表。
func listSessionsSince(projectRoot string, sinceMs int64) []string {
	sessions := session.ListSessions(projectRoot)
	since := time.UnixMilli(sinceMs)
	var ids []string
	for _, s := range sessions {
		if s.ModTime.After(since) {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// extractWrittenPaths 从子 Agent 对话中提取所有 Write/Edit 的文件路径。
func extractWrittenPaths(messages []conversation.Message) []string {
	var paths []string
	seen := make(map[string]struct{})
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tu := range m.ToolUses {
			if tu.ToolName != "WriteFile" && tu.ToolName != "EditFile" {
				continue
			}
			fp, ok := tu.Arguments["file_path"].(string)
			if !ok || fp == "" {
				continue
			}
			if _, exists := seen[fp]; exists {
				continue
			}
			seen[fp] = struct{}{}
			paths = append(paths, fp)
		}
	}
	return paths
}

func (c *Consolidator) debugf(format string, args ...any) {
	if c.deps.DebugLogf != nil {
		c.deps.DebugLogf(format, args...)
	}
}

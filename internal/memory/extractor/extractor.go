// Package extractor 实现了后台记忆抽取子 Agent。
//
// TS 文件使用闭包作用域状态模式（initExtractMemories 不返回任何值，但会修改模块级的
// extractor/drainer 指针）；Go 移植版把同样的状态封装进 Extractor 结构体
// + sync.Mutex，这样每个调用方都能得到独立实例，测试也能干净地替换 Deps。
//
// 触发方式：TUI 把 agent.Agent.OnLoopComplete 设置为一个调用 (*Extractor).Execute 的闭包。agent
// 主循环在每次 LoopComplete 事件后以 fire-and-forget 方式触发该回调。Extractor 本身通过
// runExtraction 派生自己的 goroutine 栈，因此 Execute 会快速返回，真正的 fork 在后台进行。
package extractor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cody/internal/agent"
	"cody/internal/agents"
	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/memory"
	"cody/internal/permissions"
	"cody/internal/tools"
)

// Deps 持有 Extractor 所需的外部协作者。TUI 在启动时构造一个 Deps 值，
// 把它包进 Extractor，并把得到的 Execute 方法挂到
// agent.Agent.OnLoopComplete 上。
//
// AppendSystem 是用户在一次成功抽取后看到的“Memory saved: foo.md”通知的通道。
type Deps struct {
	MemoryDir     string                  // <wd>/.cody/memory/ — 项目/参考记忆（带尾部分隔符）
	UserMemoryDir string                  // ~/.cody/memory/ — 用户/反馈记忆（带尾部分隔符）；若 $HOME 无法解析则可能为 ""
	ProjectRoot   string                  // 项目根目录的绝对路径
	Client        llm.Client              // 分叉抽取子 Agent 的 LLM client
	ToolRegistry  *tools.Registry         // 父级工具注册表（会被过滤）
	Protocol      string                  // "anthropic" / "openai"
	Conversation  *conversation.Manager   // 父级对话引用
	AppendSystem  func(string)            // 可选：通知 TUI 已保存的记忆
	DebugLogf     func(format string, args ...any) // 可选：调试日志
}

// Extractor 是后台记忆抽取器（ch09）。状态封装在结构体字段里；mu
// 保护所有可变字段。每个 Extractor 实例相互独立——测试可以用 mock 的 Deps
// 构造一个实例，而不会触碰全局状态。
//
// 从 TS 的 initExtractMemories 闭包移植而来。映射关系：
// inFlightExtractions Set → inFlight map[*sync.WaitGroup]struct{}
// lastMemoryMessageUuid string|undefined → lastMemoryMessageIdx int
// （Cody 消息没有 uuid；游标是父对话消息数组中
// 上一次成功抽取运行时的索引）
// hasLoggedGateFailure / inProgress / turnsSinceLastExtraction → bool/int
// pendingContext → *pendingExtractionCtx
type Extractor struct {
	deps Deps

	mu                       sync.Mutex
	inFlight                 map[*sync.WaitGroup]struct{}
	lastMemoryMessageIdx     int
	hasLoggedGateFailure     bool
	inProgress               bool
	turnsSinceLastExtraction int
	pendingContext           *pendingExtractionCtx
}

// pendingExtractionCtx 是尾部抽取的暂存槽。TS 版携带 (context,
// appendSystemMessage)；Go 版把所有状态放在 Extractor 自身上，因此暂存区不需要
// 任何负载——存在一个非 nil 值就意味着“当前抽取结束后再运行一次抽取”。
type pendingExtractionCtx struct{}

// InitExtractMemories 用给定的 Deps 工厂（initExtractMemories）构造一个新的 Extractor。
func InitExtractMemories(deps Deps) *Extractor {
	return &Extractor{
		deps:     deps,
		inFlight: make(map[*sync.WaitGroup]struct{}),
	}
}

// Execute 是 fire-and-forget 入口。由 TUI 挂到 agent.Agent.OnLoopComplete 上。
// 快速返回；真正的抽取工作在调用方 goroutine 上进行。错误被吞掉
// （尽力而为）——调用方（agent 主循环）会忽略返回值。
func (e *Extractor) Execute(ctx context.Context) error {
	if e == nil {
		return nil
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	e.mu.Lock()
	e.inFlight[wg] = struct{}{}
	e.mu.Unlock()
	defer func() {
		wg.Done()
		e.mu.Lock()
		delete(e.inFlight, wg)
		e.mu.Unlock()
	}()

	return e.executeImpl(ctx)
}

func (e *Extractor) executeImpl(ctx context.Context) error {
	// 进行中的合并：如果另一个抽取正在运行，就把本次调用暂存为一次待运行的
	// 尾部抽取并立即返回。pendingContext / runExtraction.finally 构成尾部链路。
	e.mu.Lock()
	if e.inProgress {
		e.deps.debugf("[extractMemories] extraction in progress — stashing for trailing run")
		e.pendingContext = &pendingExtractionCtx{}
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	return e.runExtraction(ctx, false)
}

func (e *Extractor) runExtraction(ctx context.Context, isTrailingRun bool) error {
	messages := e.deps.Conversation.GetMessages()
	newMessageCount := countModelVisibleMessagesSince(messages, e.lastMemoryMessageIdx)

	// 互斥：当主 agent 自己写入了记忆时，分叉的抽取器就多余了。
	// 把游标推过这段范围并返回。
	if hasMemoryWritesSince(messages, e.lastMemoryMessageIdx, e.deps.ProjectRoot) {
		e.deps.debugf("[extractMemories] skipping — conversation already wrote to memory files")
		e.advanceCursor(len(messages))
		return nil
	}

	// 节流：默认为 1（每轮都运行）。Cody 硬编码为 1；尾部运行绕过节流，
	// 因为它们处理的是已经提交完成的工作。
	if !isTrailingRun {
		e.turnsSinceLastExtraction++
		if e.turnsSinceLastExtraction < 1 {
			return nil
		}
	}
	e.turnsSinceLastExtraction = 0

	e.mu.Lock()
	e.inProgress = true
	e.mu.Unlock()
	startTime := time.Now()

	defer func() {
		e.mu.Lock()
		e.inProgress = false
		trailing := e.pendingContext
		e.pendingContext = nil
		e.mu.Unlock()
		if trailing != nil {
			e.deps.debugf("[extractMemories] running trailing extraction for stashed context")
			_ = e.runExtraction(ctx, true)
		}
	}()

	e.deps.debugf("[extractMemories] starting — %d new messages, memoryDir=%s, userMemoryDir=%s",
		newMessageCount, e.deps.MemoryDir, e.deps.UserMemoryDir)

	// 预注入记忆目录的清单，避免抽取子 Agent 为了 `ls` 浪费一轮。
	// 同时扫描两个目录，这样用户级和项目级记忆会出现在同一个合并清单里。
	var combinedScan []memory.MemoryHeader
	if e.deps.UserMemoryDir != "" {
		userScan, _ := memory.ScanMemoryFiles(ctx, e.deps.UserMemoryDir, "user")
		combinedScan = append(combinedScan, userScan...)
	}
	projectScan, _ := memory.ScanMemoryFiles(ctx, e.deps.MemoryDir, "project")
	combinedScan = append(combinedScan, projectScan...)
	manifest := memory.FormatMemoryManifest(combinedScan)
	extractionPrompt := BuildExtractAutoOnlyPrompt(newMessageCount, manifest, false, e.deps.UserMemoryDir, e.deps.MemoryDir)

	// 构建分叉对话：复制父级消息，然后把抽取提示词作为一条新的
	// user 消息追加进去。刻意不加 agents.runFork 的 fork 样板——抽取器是
	// 主对话的“完美分叉”，我们也不注入额外的系统指令。
	forkedConv := buildExtractorConversation(e.deps.Conversation, extractionPrompt)

	// 工具白名单：ReadFile / WriteFile / EditFile / Glob / Grep / Bash / ToolSearch（通过
	// FilterToolsForAgent 的异步路径）。Agent 和 AskUserQuestion 会被自动排除。
	subRegistry := agents.FilterToolsForAgent(e.deps.ToolRegistry, nil, nil, true)

	// 严格的路径沙箱：文件工具只允许访问 memoryDir。这比原来的
	// createAutoMemCanUseTool（允许 Read/Grep/Glob 无限制漫游）更严格，但符合
	// 提示词中“不要 grep 源代码”的明确警告，因此行为影响很小，
	// 安全收益却很实在。
	//
	// Mode = ModeBypass，这样文件/命令工具永远不会走到 Ask 路径——抽取器
	// 在后台运行，没有 TUI 可以应答。
	sandboxRoots := []string{e.deps.MemoryDir}
	if e.deps.UserMemoryDir != "" {
		sandboxRoots = append(sandboxRoots, e.deps.UserMemoryDir)
	}
	subSandbox := permissions.NewPathSandbox(sandboxRoots[0], sandboxRoots[1:]...)
	subChecker := permissions.NewChecker(subSandbox, &permissions.RuleEngine{}, permissions.ModeBypass)

	subAgent := agent.New(e.deps.Client, subRegistry, e.deps.Protocol)
	subAgent.MaxIterations = 5
	subAgent.Checker = subChecker
	subAgent.WorkDir = e.deps.ProjectRoot

	// 驱动分叉 agent 直到完成。排空事件通道，让主循环干净退出；我们不
	// 展示流式文本——只有文件写入才是重点。
	ch := subAgent.Run(ctx, forkedConv)
	for range ch {
		// 排空；不把子 agent 的事件转发到 UI。
	}

	// 只在运行完成后推进游标（无论它是否写了文件——“运行了但
	// 什么都没选”的那一轮不应被重新考虑）。
	e.advanceCursor(len(messages))

	writtenPaths := extractWrittenPaths(forkedConv.GetMessages())
	e.deps.debugf("[extractMemories] finished in %s, %d files written: %v",
		time.Since(startTime), len(writtenPaths), writtenPaths)

	// 索引文件（MEMORY.md）是机械性的——用户可见的“记忆”是主题文件，而不是
	// 索引更新。
	var memoryPaths []string
	for _, p := range writtenPaths {
		if filepath.Base(p) == memory.AutoMemEntrypointName {
			continue
		}
		memoryPaths = append(memoryPaths, p)
	}

	if len(memoryPaths) > 0 && e.deps.AppendSystem != nil {
		var names []string
		for _, p := range memoryPaths {
			names = append(names, filepath.Base(p))
		}
		e.deps.AppendSystem(fmt.Sprintf("Memory saved: %s", strings.Join(names, ", ")))
	}

	return nil
}

// Drain 等待所有进行中的抽取（包括任何待运行的尾部抽取）结束，带一个
// 软超时。从 TUI 的关闭流程中调用，这样分叉的抽取子 Agent 不会被在写入中途
// 杀掉。
//
// timeoutMs 为 0 时，如果仍有工作在进行中，则立即返回。负数 timeoutMs 被当作
// 60000（默认 60 秒）。
func (e *Extractor) Drain(timeoutMs int) error {
	if e == nil {
		return nil
	}
	if timeoutMs < 0 {
		timeoutMs = 60000
	}

	e.mu.Lock()
	wgs := make([]*sync.WaitGroup, 0, len(e.inFlight))
	for wg := range e.inFlight {
		wgs = append(wgs, wg)
	}
	e.mu.Unlock()

	if len(wgs) == 0 {
		return nil
	}

	done := make(chan struct{})
	go func() {
		for _, wg := range wgs {
			wg.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil
	}
}

func (e *Extractor) advanceCursor(to int) {
	e.mu.Lock()
	if to > e.lastMemoryMessageIdx {
		e.lastMemoryMessageIdx = to
	}
	e.mu.Unlock()
}

// countModelVisibleMessagesSince 统计 sinceIdx 之后新增的 user/assistant 消息。当
// sinceIdx 超出范围时（例如对话被压缩、我们记录的游标不再对应当前位置），回退到
// 统计所有模型可见消息。"if !foundStart"
// 恢复路径。
func countModelVisibleMessagesSince(messages []conversation.Message, sinceIdx int) int {
	if sinceIdx < 0 || sinceIdx > len(messages) {
		return countModelVisible(messages)
	}
	n := 0
	for _, m := range messages[sinceIdx:] {
		if isModelVisibleMessage(m) {
			n++
		}
	}
	return n
}

func countModelVisible(messages []conversation.Message) int {
	n := 0
	for _, m := range messages {
		if isModelVisibleMessage(m) {
			n++
		}
	}
	return n
}

func isModelVisibleMessage(m conversation.Message) bool {
	return m.Role == "user" || m.Role == "assistant"
}

// hasMemoryWritesSince 返回 true 表示 sinceIdx 之后存在包含 Write/Edit tool_use 的
// assistant 消息，且该 tool_use 指向自动记忆路径。当它返回 true 时，runExtraction 会跳过
// agent fork。
func hasMemoryWritesSince(messages []conversation.Message, sinceIdx int, projectRoot string) bool {
	if sinceIdx < 0 {
		sinceIdx = 0
	}
	if sinceIdx >= len(messages) {
		return false
	}
	for _, m := range messages[sinceIdx:] {
		if m.Role != "assistant" {
			continue
		}
		for _, tu := range m.ToolUses {
			fp := getWrittenFilePath(tu)
			if fp == "" {
				continue
			}
			if memory.IsAutoMemPath(fp, projectRoot) {
				return true
			}
		}
	}
	return false
}

// getWrittenFilePath 从 Write/Edit 的 tool_use 块中提取 file_path 参数；如果该块
// 不是这样的调用，则返回 ""。
func getWrittenFilePath(tu conversation.ToolUseBlock) string {
	if tu.ToolName != "WriteFile" && tu.ToolName != "EditFile" {
		return ""
	}
	fp, ok := tu.Arguments["file_path"].(string)
	if !ok {
		return ""
	}
	return fp
}

// extractWrittenPaths 收集分叉 agent 的所有 assistant 消息中每个 Write/Edit tool_use
// 的 file_path 参数（去重）。首次出现的保留。
func extractWrittenPaths(messages []conversation.Message) []string {
	var paths []string
	seen := make(map[string]struct{})
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tu := range m.ToolUses {
			fp := getWrittenFilePath(tu)
			if fp == "" {
				continue
			}
			if _, ok := seen[fp]; ok {
				continue
			}
			seen[fp] = struct{}{}
			paths = append(paths, fp)
		}
	}
	return paths
}

// buildExtractorConversation 把父对话的消息复制到一个新的 Manager，并把
// 抽取提示词作为最后一条 user 消息追加进去。与 agents.buildForkedConversation 不同，它
// 不会注入 ForkBoilerplateTag——原来的抽取器是完美分叉，而不是“你是一个
// 子 agent”式的分叉。
func buildExtractorConversation(parent *conversation.Manager, prompt string) *conversation.Manager {
	forked := conversation.NewManager()
	for _, msg := range parent.GetMessages() {
		switch msg.Role {
		case "assistant":
			if len(msg.ToolUses) > 0 {
				forked.AddAssistantMessageWithTools(msg.Content, msg.ToolUses)
			} else {
				forked.AddAssistantMessage(msg.Content)
			}
		default:
			if len(msg.ToolResults) > 0 {
				forked.AddToolResultsMessage(msg.ToolResults)
			} else {
				forked.AddUserMessage(msg.Content)
			}
		}
	}
	forked.AddUserMessage(prompt)
	return forked
}

func (d Deps) debugf(format string, args ...any) {
	if d.DebugLogf != nil {
		d.DebugLogf(format, args...)
	}
}

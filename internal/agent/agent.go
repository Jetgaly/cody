package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"cody/internal/compact"
	"cody/internal/conversation"
	"cody/internal/filehistory"
	"cody/internal/hooks"
	"cody/internal/llm"
	"cody/internal/permissions"
	"cody/internal/planfile"
	"cody/internal/prompt"
	"cody/internal/session"
	"cody/internal/toolresult"
	"cody/internal/tools"
)

const (
	maxTokensCeiling          = 64000
	maxOutputTokensRecoveries = 3
)

type Agent struct {
	Client        llm.Client
	Registry      *tools.Registry
	Protocol      string
	WorkDir       string
	MaxIterations int
	ContextWindow int
	// MaxOutputTokens 是模型的最大输出预算；Layer 2 用它
	// 计算压缩阈值对应的有效窗口。为零时回退到 compact 内部的
	// summaryOutputReserve 默认值。
	MaxOutputTokens int
	Checker         *permissions.Checker
	Hooks           *hooks.Engine
	// SessionID 标识本 agent 追加写入的磁盘会话日志。设置后，
	// Layer 2 压缩会把 compact_boundary 记录写进该会话，
	// 这样后续恢复时可以直接重建压缩后的状态，而不必重放
	// 压缩前的完整对话记录。为空则禁用边界持久化（测试、
	// 一次性调用方）。
	SessionID      string
	NotificationFn func() []string
	// ToolNameFilter 非 nil 时，会从发送给 LLM 的 schemas 中剔除任何 Name 返回 false 的工具。
	// 该过滤器在每轮迭代开始时被查询，因此调用方可以在不重启 agent 的情况下
	// 打开或关闭 Coordinator 模式（例如在团队创建/拆除时）。
	Instructions  string
	MemoryContent string
	// MemoryRecallCh 非阻塞 memory recall：prefetch 与主 LLM 调用并行，
	// 工具执行后从 channel 读取并注入
	MemoryRecallCh <-chan string
	ToolNameFilter func(name string) bool
	// CoordinatorActiveFn 非 nil 时，用于报告 Coordinator 模式当前是否生效。
	// 每轮迭代与 ToolNameFilter 一起被查询，这样调度指引只在
	// 工具集被收窄时出现，并在团队拆除后消失。
	CoordinatorActiveFn func() bool
	// OnLoopComplete 非 nil 时，在 agent 到达 LoopComplete
	// （最后的助手消息，无剩余工具调用）后被以 fire-and-forget 方式调用。供 ch09 后台记忆提取使用。
	// 取代了原来的 stopHooks 分发器；失败是静默的，绝不能阻塞主
	// 循环。回调会收到实时的 conversation —— 不要从其它 goroutine 修改它。
	OnLoopComplete  func(conv *conversation.Manager)
	FileHistory     *filehistory.History
	compactTracking compact.AutoCompactTrackingState
	// RecoveryState 保存重建工作上下文所需的快照，
	// 用于 Layer 2 把对话折叠成摘要之后：最近的
	// 文件读取和技能调用。该结构体是并发安全的，因此
	// streaming executor 可以从多个 goroutine 写入它。
	RecoveryState *compact.RecoveryState
	eventCh       chan AgentEvent
	// activeSkills 记录本会话中已激活的 Skill SOP（name → body）。
	// 供 /skills 展示当前激活内容，也供 RecoveryState 在压缩后存活。
	// body 只作为一条消息注入对话一次 —— 不会每轮重复注入。
	activeSkills map[string]string
}

// ActivateSkill 记录一次技能激活。body 会保留给 /skills 列表和压缩
// 恢复使用，但不会每轮重新注入 —— 它作为普通消息存在于对话中。
func (a *Agent) ActivateSkill(name, body string) {
	if a.activeSkills == nil {
		a.activeSkills = make(map[string]string)
	}
	a.activeSkills[name] = body
}

// ClearActiveSkills 清除所有已固定的 SOP。由 /clear 调用，这样全新对话不会
// 继承上一个任务留下的 SOP。在从未激活过技能时调用也安全。
func (a *Agent) ClearActiveSkills() {
	a.activeSkills = nil
}

// GetActiveSkills 返回当前已固定 SOP 的副本（name → body）。供测试和
// /skills 展示当前激活内容使用。
func (a *Agent) GetActiveSkills() map[string]string {
	out := make(map[string]string, len(a.activeSkills))
	for k, v := range a.activeSkills {
		out[k] = v
	}
	return out
}

// SetToolFilter 为当前对话安装一个工具可见性过滤器。过滤器
// 在每轮迭代开始时被查询，因此调用方可以在不重启 agent 的情况下打开或关闭
// Coordinator 模式。传入 nil 会清除之前的过滤器。
func (a *Agent) SetToolFilter(allow func(name string) bool) {
	a.ToolNameFilter = allow
}

// ToolRegistry 返回当前生效的工具注册表。命名为 ToolRegistry（而不只是 Registry，尽管
// 那样会和字段名一致），是为了避开 Go 不允许的方法/字段同名冲突。满足
// skills.SkillHost 契约。
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.Registry
}

func New(client llm.Client, registry *tools.Registry, protocol string) *Agent {
	wd, _ := os.Getwd()
	return &Agent{
		Client:        client,
		Registry:      registry,
		Protocol:      protocol,
		WorkDir:       wd,
		MaxIterations: 0,
		ContextWindow: 200000,
		RecoveryState: compact.NewRecoveryState(),
	}
}

// SetSessionID 把磁盘会话日志 id 挂到 agent 上，这样 Layer 2
// 压缩可以把 compact_boundary 记录持久化到 TUI 正在追加普通消息的
// 同一个会话里。在 agent 构造完成后立即由 TUI 调用
// （恢复会话切换后还会再调用一次）。
func (a *Agent) SetSessionID(id string) { a.SessionID = id }

// currentToolSchemas 构建下一次 API 调用将使用的 schema 列表，
// 遵循任何生效的 ToolNameFilter（例如 Teams coordinator 模式）。
// 该列表在恢复附件（列出压缩后仍然可用的内容）和实际的 Stream 调用之间
// 共享，保证两处视图保持一致。
func (a *Agent) currentToolSchemas() []map[string]any {
	schemas := a.Registry.GetAllSchemas(a.Protocol)
	if a.ToolNameFilter == nil {
		return schemas
	}
	// 过滤器就是唯一依据，不留任何例外分支
	return filterSchemasByName(schemas, a.ToolNameFilter)
}

func (a *Agent) Run(ctx context.Context, conv *conversation.Manager) <-chan AgentEvent {
	ch := make(chan AgentEvent, 32)

	go func() {
		defer close(ch)
		defer a.emitHook(hooks.EventSessionEnd, "", nil)

		a.emitHook(hooks.EventSessionStart, "", nil)

		conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)

		var totalInput, totalOutput int
		maxTokensEscalated := false
		outputRecoveries := 0

		for iteration := 1; ; iteration++ {
			// 检查迭代次数上限，超过就发事件并退出
			if a.MaxIterations > 0 && iteration > a.MaxIterations {
				ch <- ErrorEvent{Message: fmt.Sprintf("Agent reached maximum iterations (%d)", a.MaxIterations)}
				return
			}

			if ctx.Err() != nil {
				return
			}

			// 每轮迭代只计算一次工具 schema 列表，让恢复附件
			// （压缩触发时）和下面实际的 Stream 调用
			// 对已接线的工具保持一致。技能过滤器只能在迭代之间
			// 变化，绝不能在单次迭代内变化。
			toolSchemas := a.currentToolSchemas()

			// Plan 模式：注入结构化的流程提醒。
			// 一是告诉权限检查器 Plan 文件的路径，让写 Plan 文件成为例外，
			// 不被 Plan Mode 的只读限制拦住。
			// 二是往对话里注入一段 system-reminder，
			// 告诉 LLM 现在处于规划模式，只能思考和分析，不能执行写操作。
			if a.Checker != nil && a.Checker.Mode == permissions.ModePlan {
				planPath := planfile.GetOrCreatePlanPath(a.WorkDir)
				a.Checker.PlanFilePath = planPath
				planExists := planfile.PlanExists(a.WorkDir)
				reminder := prompt.BuildPlanModeReminder(planPath, planExists, iteration)
				conv.AddSystemReminder(reminder)
			}

			// Coordinator 模式：工具集被收窄的同时注入调度指引。
			// 走 system-reminder 而不是系统提示词：长会话里开头那份约束会被淹没，
			// 每轮追加一次才拉得回来，而且系统提示词是缓存前缀，动它整段都要重新计费。
			if a.CoordinatorActiveFn != nil && a.CoordinatorActiveFn() {
				conv.AddSystemReminder(prompt.CoordinatorReminder(iteration))
			}

			// 通知函数：每轮迭代都调用一次，返回的消息注入 system-reminder。
			// 只在第一次对话时注入，配合 once: true 就行
			/*
							action:
				 				type: prompt
				 				message: "请先阅读 ARCHITECTURE.md 了解项目结构，然后再开始工作。"
								once: true
			*/
			//hook.DrainNotifications -> model.drainTaskNotifications -> agent.NotificationFn
			if a.NotificationFn != nil {
				for _, note := range a.NotificationFn() {
					conv.AddSystemReminder(note)
				}
			}

			a.emitHook(hooks.EventTurnStart, "", nil)

			// 把延迟加载的工具名注入 system-reminder，让模型知道可以通过
			// ToolSearch 使用哪些工具。
			if deferredNames := a.Registry.GetDeferredToolNames(); len(deferredNames) > 0 {
				reminder := "The following deferred tools are available via ToolSearch. Their schemas are NOT loaded - use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas before calling them:\n" + strings.Join(deferredNames, "\n")
				conv.AddSystemReminder(reminder)
			}

			a.emitHook(hooks.EventPreSend, "", nil)

			// Layer 2：自动压缩
			// Layer 1（工具结果预算）在结果入历史时已处理完，历史里的
			// 内容就是最终大小，直接用其消息估算 token
			/*
			正常压缩已触发（软线/硬线/手动 /compact）
				  → 把要压缩的旧前缀发给 LLM 生成摘要
				  → 摘要请求返回 ContextTooLongError（prompt_too_long）
				  → 丢弃最旧的 API-round 组，重试
				  → 最多重试 3 次（maxPTLRetries），仍失败就放弃压缩
			*/
			if msg, err := compact.ManageContext(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.MaxOutputTokens, &a.compactTracking, a.RecoveryState, toolSchemas); err == nil && msg != "" {
				ch <- CompactEvent{Message: msg}
				conv.ClearUsageAnchor()
				conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)
			}

			// 每次迭代都会获取新的sse流
			events, errs := a.Client.Stream(ctx, conv, toolSchemas)

			var text string
			var toolCalls []llm.ToolCallComplete
			var thinkingBlocks []conversation.ThinkingBlock
			var stopReason string
			var usage llm.UsageInfo

			executor := NewStreamingExecutor(a.Registry, ch)

			//流结束后进行安全性分批执行：只读工具并发，写/命令工具串行
			for ev := range events {
				switch e := ev.(type) {
				case llm.ThinkingDelta:
					ch <- ThinkingText{Text: e.Text}
				case llm.ThinkingComplete:
					thinkingBlocks = append(thinkingBlocks, conversation.ThinkingBlock{
						Thinking:  e.Thinking,
						Signature: e.Signature,
					})
				case llm.TextDelta:
					text += e.Text
					ch <- StreamText{Text: e.Text}
				case llm.ToolCallStart:
					ch <- ToolUseEvent{ToolID: e.ToolID, ToolName: e.ToolName}
				case llm.ToolCallDelta:
					// 忽略
				case llm.ToolCallComplete:
					toolCalls = append(toolCalls, e)
					ch <- ToolUseEvent{
						ToolID:   e.ToolID,
						ToolName: e.ToolName,
						Args:     e.Arguments,
					}
					// 收集工具调用，等流式结束后按安全性分批执行
					executor.Submit(toolCallInfo{
						toolID:    e.ToolID,
						toolName:  e.ToolName,
						arguments: e.Arguments,
					})
				case llm.StreamEnd:
					stopReason = e.StopReason
					usage = e.Usage
				}
			}
			a.emitHook(hooks.EventPostReceive, text, nil)

			// 处理流错误。
			select {
			case err := <-errs:
				if err != nil {
					// 处理上下文过长或被限流的错误，决定是否重试或压缩
					if retry, compacted := a.handleStreamError(ctx, ch, conv, err); retry {
						if compacted {
							conv.ClearUsageAnchor()
							conv.InjectLongTermMemory(a.Instructions, a.MemoryContent)
						}
						continue // 重试本轮
					}
					ch <- ErrorEvent{Message: err.Error()}
					return
				}
			default:
			}

			totalInput += usage.InputTokens
			totalOutput += usage.OutputTokens
			ch <- UsageEvent{InputTokens: totalInput, OutputTokens: totalOutput}

			anchorAfterAssistant := func() {
				conv.RecordUsageAnchor(
					usage.InputTokens,
					usage.OutputTokens,
					usage.CacheReadTokens,
					usage.CacheCreationTokens,
				)
			}

			// 处理 max_tokens 停止原因。
			if stopReason == "max_tokens" {
				if !maxTokensEscalated {
					// 首次触发：静默升级。
					if setter, ok := a.Client.(llm.MaxTokensSetter); ok {
						setter.SetMaxOutputTokens(maxTokensCeiling)
						maxTokensEscalated = true
					}
					if text != "" {
						conv.AddAssistantFull(text, thinkingBlocks, nil)
						a.persistLastMessage(conv)
						// 记录 usage anchor 以便下一轮继续从这里接着算预算
						anchorAfterAssistant()
						conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Do not apologize or repeat previous content. Pick up mid-thought if needed.")
					}
					ch <- RetryEvent{Reason: "max_tokens escalation", Wait: 0}
					continue
				} else if outputRecoveries < maxOutputTokensRecoveries {
					// 多轮恢复。
					outputRecoveries++
					conv.AddAssistantFull(text, thinkingBlocks, nil)
					a.persistLastMessage(conv)
					anchorAfterAssistant()
					conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Break remaining work into smaller pieces.")
					ch <- RetryEvent{Reason: fmt.Sprintf("max_tokens recovery %d/%d", outputRecoveries, maxOutputTokensRecoveries), Wait: 0}
					continue
				}
				// 已用尽：落到正常完成流程。
			} else {
				// 成功的一轮结束后重置恢复计数。
				outputRecoveries = 0
			}

			if len(toolCalls) == 0 {
				conv.AddAssistantFull(text, thinkingBlocks, nil)
				a.persistLastMessage(conv)
				if a.FileHistory != nil {
					summary := text
					if len(summary) > 60 {
						summary = summary[:60] + "..."
					}
					a.FileHistory.MakeSnapshot(conv.Len(), summary)
				}
				ch <- LoopComplete{TotalTurns: iteration}
				if a.OnLoopComplete != nil {
					go a.OnLoopComplete(conv)
				}
				return
			}

			var toolUses []conversation.ToolUseBlock
			for _, tc := range toolCalls {
				toolUses = append(toolUses, conversation.ToolUseBlock{
					ToolUseID: tc.ToolID,
					ToolName:  tc.ToolName,
					Arguments: tc.Arguments,
				})
			}
			conv.AddAssistantFull(text, thinkingBlocks, toolUses)
			a.persistLastMessage(conv)
			// 助手消息已就位，把真实 usage 锚定到对话上；
			// 后续的工具结果和下一个用户消息都将
			// 在此基线上做增量估算。
			anchorAfterAssistant()

			// 按安全性分批执行：只读工具并发，写/命令工具串行
			results := executor.ExecuteAll(ctx, a)

			// 溢写文件的回读结果豁免溢写：把模型刚读回来的内容再写盘换成
			// 预览，模型就永远看不到全文，还会在 读回、溢写 之间打转。
			// 模型用 ReadFile 读回之前的溢写文件时，读回的内容和原始结果一样大，
			// 如果照常溢写，模型拿到的又是一个预览加路径，永远看不到全文。
			exempt := make(map[string]bool)
			for _, tc := range toolCalls {
				if toolresult.IsSpillReadback(tc.ToolName, tc.Arguments, a.WorkDir, a.SessionID) {
					exempt[tc.ToolID] = true
				}
			}

			var toolResults []conversation.ToolResultBlock
			for _, r := range results {
				ch <- ToolResultEvent{
					ToolID:   r.toolID,
					ToolName: r.toolName,
					Output:   r.output,
					IsError:  r.isError,
					Elapsed:  r.elapsed,
				}

				content := r.output
				if len(content) > tools.MaxOutputChars && !exempt[r.toolID] {
					// 单条超限：写盘换预览。写盘失败会原样保留，同一块磁盘
					// 聚合预算也不必再试，所以两种结果都标记豁免。
					content = toolresult.PersistLargeResult(a.WorkDir, a.SessionID, r.toolID, r.output)
					exempt[r.toolID] = true
				}
				toolResults = append(toolResults, conversation.ToolResultBlock{
					ToolUseID: r.toolID,
					Content:   content,
					IsError:   r.isError,
				})
			}

			// 聚合预算：一轮并行工具的结果落在同一条消息里，单条阈值管不住
			// 合计超限的情况。进历史前把整批处理完，消息一出生就是终态。
			toolresult.ApplyBudget(toolResults, exempt, a.WorkDir, a.SessionID)

			exitPlanCalled := false
			for _, tc := range toolCalls {
				if tc.ToolName == "ExitPlanMode" {
					exitPlanCalled = true
					break
				}
			}
			conv.AddToolResultsMessage(toolResults)
			a.persistLastMessage(conv)

			// 非阻塞 memory recall：工具执行完后检查 prefetch 是否就绪
			if a.MemoryRecallCh != nil {
				select {
				case recall := <-a.MemoryRecallCh:
					if recall != "" {
						conv.AddSystemReminder(recall)
					}
					a.MemoryRecallCh = nil // 只消费一次
				default:
					// prefetch 还没好，下轮再检查
				}
			}

			if exitPlanCalled {
				ch <- TurnComplete{Turn: iteration}
				ch <- LoopComplete{TotalTurns: iteration}
				return
			}
			ch <- TurnComplete{Turn: iteration}
			a.emitHook(hooks.EventTurnEnd, "", nil)
		}
	}()

	return ch
}

// emitHook 在配置了 Engine 时触发一个 hook 事件。失败不致命，会通过
// hook 通知队列暴露（在下一轮的 system reminders 中排空）。
func (a *Agent) emitHook(event hooks.EventName, message string, args map[string]any) {
	if a.Hooks == nil {
		return
	}
	a.Hooks.RunHooks(hooks.HookContext{
		EventName: event,
		ToolArgs:  args,
		Message:   message,
	})
}

// filterSchemasByName 只保留 "name" 通过 allow 谓词的工具 schema。由
// Coordinator 模式用来把 Lead agent 限制为仅协调类工具，而队友执行
// 实际工作。
func filterSchemasByName(schemas []map[string]any, allow func(name string) bool) []map[string]any {
	out := make([]map[string]any, 0, len(schemas))
	for _, s := range schemas {
		name, _ := s["name"].(string)
		if allow(name) {
			out = append(out, s)
		}
	}
	return out
}

// handleStreamError 返回 (retry, compacted)：retry 通知调用方
// 重新运行这一轮；compacted 表示 ForceCompact 已重写
// 对话，因此调用方必须丢弃其 usage anchor（其 AnchorCount 不再
// 对应新的对话记录）。
func (a *Agent) handleStreamError(ctx context.Context, ch chan AgentEvent, conv *conversation.Manager, err error) (retry, compacted bool) {
	var ctxErr *llm.ContextTooLongError
	if errors.As(err, &ctxErr) {
		// 历史里的工具结果在入历史时已按预算处理为终态，直接传 nil
		// 让 ForceCompact 使用 conv 自身消息
		msg, compactErr := compact.ForceCompact(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.RecoveryState, a.currentToolSchemas())
		if compactErr == nil && msg != "" {
			ch <- CompactEvent{Message: "Auto-compacted due to context length: " + msg}
			return true, true // 重试，且 anchor 现在已过期
		}
		return false, false
	}

	var rlErr *llm.RateLimitError
	if errors.As(err, &rlErr) {
		wait := parseRetryAfter(rlErr.RetryAfter)
		ch <- RetryEvent{Reason: "rate limited", Wait: wait}
		select {
		case <-time.After(wait):
			return true, false // 重试但不压缩
		case <-ctx.Done():
			return false, false
		}
	}

	return false, false
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 5 * time.Second
	}
	if secs, err := strconv.Atoi(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	return 5 * time.Second
}

type toolExecResult struct {
	toolID   string
	toolName string
	output   string
	isError  bool
	elapsed  time.Duration
}

// extractFilePath 从常见的工具参数键中提取一个代表性路径，以便 hooks 能做路径
// 通配符匹配（`file_path =* "**/*.go"`）。
func extractFilePath(args map[string]any) string {
	for _, key := range []string{"file_path", "path", "pattern", "target"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (a *Agent) executeSingleTool(ctx context.Context, eventCh chan AgentEvent, tc toolCallInfo) toolExecResult {
	tool := a.Registry.Get(tc.toolName)
	start := time.Now()

	//工具检查
	if tool == nil {
		// 工具名不存在只回一条错误结果，让模型自己换个工具重来，不打断循环。
		return toolExecResult{
			toolID:   tc.toolID,
			toolName: tc.toolName,
			output:   fmt.Sprintf("Error: unknown tool '%s'", tc.toolName),
			isError:  true,
			elapsed:  time.Since(start),
		}
	}

	//权限检查
	if a.Checker != nil {
		decision := a.Checker.Check(tool, tc.arguments)
		if decision.Effect == permissions.Deny {
			return toolExecResult{
				toolID:   tc.toolID,
				toolName: tc.toolName,
				output:   fmt.Sprintf("Permission denied: %s", decision.Reason),
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
		if decision.Effect == permissions.Ask {
			respCh := make(chan PermissionResponse, 1)
			desc := permissions.DescribeToolAction(tc.toolName, tc.arguments)
			eventCh <- PermissionRequestEvent{
				ToolName:   tc.toolName,
				Desc:       desc,
				ResponseCh: respCh,
			}
			resp := <-respCh
			if resp == PermDeny {
				return toolExecResult{
					toolID:   tc.toolID,
					toolName: tc.toolName,
					output:   conversation.RejectedToolResult,
					isError:  true,
					elapsed:  time.Since(start),
				}
			}
			if resp == PermAllowAlways {
				content := permissions.ExtractContent(tc.toolName, tc.arguments)
				pattern := content + "*"
				if len(content) > 60 {
					pattern = content[:60] + "*"
				}
				a.Checker.AddSessionAllow(tc.toolName, pattern)
				a.Checker.RuleEngine.AppendLocalRule(permissions.Rule{
					ToolName: tc.toolName,
					Pattern:  pattern,
					Effect:   permissions.RuleAllow,
				})
			}
		}
	}

	if a.Hooks != nil {
		hctx := hooks.HookContext{
			EventName: hooks.EventPreToolUse,
			ToolName:  tc.toolName,
			ToolArgs:  tc.arguments,
			FilePath:  extractFilePath(tc.arguments),
		}
		if rejected, msg := a.Hooks.RunPreToolHooks(hctx); rejected {
			return toolExecResult{
				toolID:   tc.toolID,
				toolName: tc.toolName,
				output:   "Blocked by hook: " + msg,
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
	}

	result := tool.Execute(ctx, tc.arguments)

	if !result.IsError && tc.toolName == "ReadFile" {
		if p, _ := tc.arguments["file_path"].(string); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				a.RecoveryState.RecordFileRead(p, string(data))
			}
		}
	}

	if a.Hooks != nil {
		a.Hooks.RunHooks(hooks.HookContext{
			EventName: hooks.EventPostToolUse,
			ToolName:  tc.toolName,
			ToolArgs:  tc.arguments,
			FilePath:  extractFilePath(tc.arguments),
			Message:   result.Output,
		})
	}

	return toolExecResult{
		toolID:   tc.toolID,
		toolName: tc.toolName,
		output:   result.Output,
		isError:  result.IsError,
		elapsed:  time.Since(start),
	}
}

func formatToolArgs(args map[string]any) string {
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, s))
	}
	return strings.Join(parts, ", ")
}

// persistLastMessage 把刚追加进对话历史的那条消息写入会话日志。
//
// 落盘点放在主循环而不是各个前端：TUI 和 Web 共用同一条记录路径，
// 中间轮次的助手文本和完整的工具调用链都会被记下来，恢复会话时才能还原。
// WorkDir 或 SessionID 为空时（一次性调用、子 Agent）跳过，不写盘。
func (a *Agent) persistLastMessage(conv *conversation.Manager) {
	if a.WorkDir == "" || a.SessionID == "" {
		return
	}
	msgs := conv.GetMessages()
	if len(msgs) == 0 {
		return
	}
	session.SaveMessage(a.WorkDir, a.SessionID, session.FromConversation(msgs[len(msgs)-1]))
}

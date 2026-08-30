package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"cody/internal/agent"
	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/permissions"
	"cody/internal/teams"
	"cody/internal/tools"
	"cody/internal/worktree"
)

// sanitizeSlugSegment 把 [a-zA-Z0-9._-] 之外的任意字符替换成 '-'，并去掉多余的
// 分隔符，保证结果可以安全地用作 git 分支名。
var unsafeSlugChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeSlugSegment(s string) string {
	clean := unsafeSlugChars.ReplaceAllString(s, "-")
	clean = strings.Trim(clean, "-_.")
	if clean == "" {
		clean = "subagent"
	}
	if len(clean) > 40 {
		clean = clean[:40]
	}
	return clean
}

type SubAgentProgress struct {
	AgentDesc string
	AgentType string
	ToolName  string
	ToolArgs  map[string]any
	Elapsed   float64
	IsError   bool
	Done      bool
	ToolCount int
	TotalTime float64
}

const ForkBoilerplateTag = "<fork_boilerplate>"

// ForkAgentType 是 fork 子 Agent 的类型名，拼进 QuerySource 用来识别 fork 出来的会话。
const ForkAgentType = "fork"

// GeneralPurposeAgentType 是 fork 关闭时，省略 subagent_type 的回退目标。
const GeneralPurposeAgentType = "general-purpose"

// ForkQuerySource 是 fork 子 Agent 的来源标记，形如 `agent:builtin:fork`。
// 它是判断「当前身处 fork 子 Agent」的首选信号，拿不到时再退回扫描对话历史里的
// ForkBoilerplateTag。
const ForkQuerySource = "agent:builtin:" + ForkAgentType

type AgentTool struct {
	Client        llm.Client
	ModelResolver func(string) (llm.Client, error)
	Registry      *tools.Registry
	Protocol      string
	TaskMgr       *TaskManager
	ProgressCh    chan<- SubAgentProgress
	Loader        *AgentLoader
	Conversation  *conversation.Manager // 父对话，Fork 时需要
	TeamMgr       *teams.TeamManager    // 可选，启用 team_name 参数

	// ParentChecker 是父 agent 的权限检查器。Sandbox 与 RuleEngine 复用，
	// 只有当子 agent 定义或调用设置了不同的 permissionMode 时才覆盖 Mode。
	// 可选——为 nil 时，子 agent 不继承任何检查器（早期启动 / 测试场景）。
	ParentChecker *permissions.Checker

	// QuerySource 用于识别派生出当前 AgentTool 的 agent，以便检测嵌套 fork。主线程下为空；
	// 当 AgentTool 实例位于已派生的子 agent 内部时，设为 ForkQuerySource（或 "agent:builtin:<type>"）。
	// 对压缩免疫——即使 fork 样板文本被概括进对话历史摘要，它仍然有效。
	QuerySource string

	// ForkDisabled 为真时，省略 subagent_type 不再 fork，而是回退到通用 agent。
	// 用「关闭」而不是「开启」语义，是为了让零值就是默认行为（fork 可用），
	// 每个构造点不必都显式赋值。
	ForkDisabled bool
}

func (t *AgentTool) Name() string                 { return "Agent" }
func (t *AgentTool) Category() tools.ToolCategory { return tools.CategoryCommand }

func (t *AgentTool) Description() string {
	desc := `Launch a sub-agent to handle a complex task. Each sub-agent runs independently with its own context. The sub-agent cannot see the current conversation.

This is ONE tool with multiple roles. Roles are NOT separate tools — you pick one by passing its name in the "subagent_type" parameter. Do not search for a tool named after a role; call THIS tool ("Agent") and set "subagent_type".

Available roles for the "subagent_type" parameter:`

	if t.Loader != nil {
		for _, name := range t.Loader.ListNames() {
			def := t.Loader.Get(name)
			desc += "\n- " + name + ": " + def.WhenToUse
		}
	} else {
		desc += "\n- general-purpose: Full tool access for multi-step tasks (default)"
		desc += "\n- plan: Read-only tools for designing implementation plans"
		desc += "\n- explore: Read-only search agent for locating code"
	}

	desc += `

Example call shape:
{
  "name": "Agent",
  "input": {
    "subagent_type": "<role from the list above>",
    "description": "Short task label",
    "prompt": "Detailed instructions — the sub-agent has zero prior context"
  }
}

Write a detailed prompt explaining what the sub-agent should do and why — it has no prior context.
When tasks are independent, launch multiple sub-agents in parallel by making multiple Agent tool calls in a single response.`
	return desc
}

func (t *AgentTool) Schema() map[string]any {
	agentTypes := []string{"general-purpose", "plan", "explore"}
	if t.Loader != nil {
		agentTypes = t.Loader.ListNames()
	}

	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "A short (3-5 word) description of the task",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task for the agent to perform. Be detailed — the agent has no context from this conversation.",
				},
				"subagent_type": map[string]any{
					"type":        "string",
					"enum":        agentTypes,
					"description": "The type of agent to use. If omitted, forks current conversation context.",
				},
				"model": map[string]any{
					"type":        "string",
					"enum":        []string{"sonnet", "opus", "haiku"},
					"description": "Override the model for this agent.",
				},
				"run_in_background": map[string]any{
					"type":        "boolean",
					"description": "Set to true to run the agent in the background.",
				},
				"name": map[string]any{
					"type":        "string",
					"description": "Name for the agent, enabling SendMessage communication.",
				},
				"isolation": map[string]any{
					"type":        "string",
					"enum":        []string{"worktree"},
					"description": "Isolation mode. Set to 'worktree' to give the agent its own git worktree so its file edits don't collide with peers or the lead. REQUIRED when spawning two or more teammates in parallel that may write files; STRONGLY RECOMMENDED for any single teammate doing non-trivial file edits while the lead is still working in the same repo. Skip only for read-only tasks (explore/grep/plan) or when you explicitly want the teammate to share your working tree.",
				},
				"plan_mode_required": map[string]any{
					"type":        "boolean",
					"description": "Only meaningful together with team_name. When true, the teammate starts in plan mode: it can read and investigate but cannot modify anything until it submits a plan and you approve it via SendMessage with type='plan_approval_response'. Use it for risky or ambiguous tasks where a wrong direction would cost a lot of rework.",
				},
				"team_name": map[string]any{
					"type":        "string",
					"description": "REQUIRED when creating team members. Spawns the agent as a long-running teammate under this team (created via TeamCreate). Unlike regular sub-agents, team members run in their own terminal, persist after the lead returns, and communicate with each other via SendMessage. Without team_name the agent runs as a one-shot sub-agent that blocks and returns inline.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"default", "acceptEdits", "plan", "bypassPermissions"},
					"description": "Permission mode override for the spawned agent (e.g., 'plan' to require plan approval).",
				},
			},
			"required": []string{"description", "prompt"},
		},
	}
}

func (t *AgentTool) selectClient(specModel, overrideModel string) llm.Client {
	model := overrideModel
	if model == "" {
		model = specModel
	}
	if model == "" || model == "inherit" {
		return t.Client
	}
	if t.ModelResolver != nil {
		if c, err := t.ModelResolver(model); err == nil {
			return c
		}
	}
	return t.Client
}

func (t *AgentTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	description, _ := args["description"].(string)
	prompt, _ := args["prompt"].(string)
	if description == "" || prompt == "" {
		return tools.ToolResult{Output: "Error: description and prompt are required", IsError: true}
	}

	subagentType, _ := args["subagent_type"].(string)
	modelOverride, _ := args["model"].(string)
	runInBackground, _ := args["run_in_background"].(bool)
	agentName, _ := args["name"].(string)
	teamName, _ := args["team_name"].(string)
	modeOverride, _ := args["mode"].(string)
	isolation, _ := args["isolation"].(string)
	planModeRequired, _ := args["plan_mode_required"].(bool)

	if modeOverride != "" && !validPermissionModes[modeOverride] {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: invalid mode '%s'. Valid: default, acceptEdits, plan, bypassPermissions", modeOverride),
			IsError: true,
		}
	}

	// 团队成员路径：显式传入 team_name 时，本次派生会变成该团队后端下的一个常驻队友。
	// 队友一启动，Lead 就拿回控制权；后续协作通过 SendMessage / 信箱通知进行。
	if teamName != "" && t.TeamMgr != nil {
		return t.runAsTeammate(ctx, teamName, agentName, description, prompt, modelOverride, subagentType, isolation, planModeRequired)
	}

	// 省略 subagent_type 时的走向由配置决定：fork 开着就继承父对话，关着就当成
	// 没指定类型，回退到通用 agent。这里不报错，模型只是没填一个可选参数，
	// 为此中断一次调用不值得，回退到通用 agent 一样能把活干了。
	if subagentType == "" && t.ForkDisabled {
		subagentType = GeneralPurposeAgentType
	}

	// Fork 路径：没有指定 subagent_type。
	if subagentType == "" {
		// fork 子 Agent 继承父对话上下文，后台执行
		return t.runFork(ctx, description, prompt, modelOverride, agentName)
	}

	// 定义路径：从 loader 或内建定义解析 spec。
	var spec SubAgentSpec
	if t.Loader != nil {
		def := t.Loader.Get(subagentType)
		if def == nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown agent type '%s'. Available: %s", subagentType, strings.Join(t.Loader.ListNames(), ", ")),
				IsError: true,
			}
		}
		spec = def.ToSpec()
	} else {
		s, ok := BuiltinSpecs[subagentType]
		if !ok {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown agent type '%s'. Available: general-purpose, plan, explore", subagentType),
				IsError: true,
			}
		}
		spec = s
	}

	// 单次调用的 mode 覆盖优先级高于定义里的 permissionMode。
	if modeOverride != "" {
		spec.PermissionMode = modeOverride
	}

	// 调用方传 run_in_background，或者 Agent 定义自己标了 background，都走异步派发。
	// 两个条件是或的关系：定义里声明的后台属性不该被调用方漏传而失效。
	if runInBackground || spec.Background {
		// 后台异步调用，立即返回 task ID。子 Agent 的输出会在后台任务通知里送回来。
		return t.runAsync(ctx, spec, description, prompt, modelOverride)
	}

	// 前台同步调用，阻塞直到子 Agent 完成。子 Agent 的输出会被拼进返回值里。
	return t.runSync(ctx, spec, description, prompt, modelOverride, isolation)
}

func (t *AgentTool) runSync(ctx context.Context, spec SubAgentSpec, description, prompt, modelOverride, isolation string) tools.ToolResult {
	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, spec.DisallowedTools, false)
	client := t.selectClient(spec.Model, modelOverride)

	subAgent := agent.New(client, subRegistry, t.Protocol)
	subAgent.Checker = deriveSubAgentChecker(t.ParentChecker, spec.PermissionMode)
	if spec.MaxTurns > 0 {
		subAgent.MaxIterations = spec.MaxTurns
	} else {
		subAgent.MaxIterations = 200
	}

	// Worktree 隔离：为子 agent 创建独立的 worktree。
	var wtResult *worktree.AgentWorktreeResult
	if isolation == "worktree" {
		slug := generateAgentSlug(description)
		var err error
		wtResult, err = worktree.CreateAgentWorktree(ctx, slug)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error creating agent worktree: %s", err),
				IsError: true,
			}
		}
		subAgent.WorkDir = wtResult.WorktreePath

		// 把 worktree 通知注入 prompt。
		parentCwd, _ := os.Getwd()
		notice := worktree.BuildWorktreeNotice(parentCwd, wtResult.WorktreePath)
		prompt = notice + "\n\n" + prompt
	}

	conv := conversation.NewManager()
	if spec.SystemPromptOverride != "" {
		conv.AddSystemReminder(spec.SystemPromptOverride)
	}
	// initialPrompt 会前置拼接到第一轮用户消息之前。
	if spec.InitialPrompt != "" {
		conv.AddUserMessage(spec.InitialPrompt)
	}
	conv.AddUserMessage(prompt)

	start := time.Now()
	var output strings.Builder
	toolCount := 0
	ch := subAgent.Run(ctx, conv)

	for ev := range ch {
		switch e := ev.(type) {
		case agent.StreamText:
			output.WriteString(e.Text)
		case agent.PermissionRequestEvent:
			// 子 agent 是无头的——没有界面可供弹权限请求。自动拒绝让子 agent 的
			// executeSingleTool 不被阻塞，而不是在 respCh 上永远挂起。respCh 缓冲区大小为 1，
			// 所以这次发送是非阻塞的。
			e.ResponseCh <- agent.PermDeny
		case agent.ToolResultEvent:
			toolCount++
			emitProgress(t.ProgressCh, ctx, SubAgentProgress{
				AgentDesc: description,
				AgentType: spec.Name,
				ToolName:  e.ToolName,
				ToolArgs:  map[string]any{"_summary": e.Output},
				Elapsed:   e.Elapsed.Seconds(),
				IsError:   e.IsError,
			})
		case agent.ErrorEvent:
			emitProgress(t.ProgressCh, ctx, SubAgentProgress{
				AgentDesc: description,
				AgentType: spec.Name,
				Done:      true,
				ToolCount: toolCount,
				TotalTime: time.Since(start).Seconds(),
				IsError:   true,
			})
			return tools.ToolResult{
				Output:  fmt.Sprintf("Agent failed: %s", e.Message),
				IsError: true,
			}
		}
	}

	elapsed := time.Since(start)

	emitProgress(t.ProgressCh, ctx, SubAgentProgress{
		AgentDesc: description,
		AgentType: spec.Name,
		Done:      true,
		ToolCount: toolCount,
		TotalTime: elapsed.Seconds(),
	})

	result := output.String()
	if result == "" {
		result = "(agent produced no output)"
	}

	// Worktree 清理：如果子 agent 在独立 worktree 中运行，干净则自动删除，有改动则保留。
	if wtResult != nil {
		if worktree.HasWorktreeChanges(ctx, wtResult.WorktreePath, wtResult.HeadCommit) {
			result += fmt.Sprintf("\n\nWorktree kept at %s (branch %s) — has uncommitted changes or new commits.",
				wtResult.WorktreePath, wtResult.WorktreeBranch)
		} else {
			worktree.RemoveAgentWorktree(ctx, wtResult.WorktreePath, wtResult.WorktreeBranch, wtResult.GitRoot)
		}
	}

	return tools.ToolResult{
		Output: fmt.Sprintf("Agent \"%s\" completed in %s.\n\n%s", description, elapsed.Round(time.Millisecond), result),
	}
}

func (t *AgentTool) runFork(ctx context.Context, description, prompt, modelOverride, agentName string) tools.ToolResult {
	if t.Conversation == nil {
		return tools.ToolResult{Output: "Error: fork requires parent conversation context", IsError: true}
	}

	// 嵌套 fork 防护，分两层：
	// (1) 主防线：querySource——在 fork 子 agent 内构造 AgentTool 实例时被设置。
	//     对压缩免疫；能拦住对话历史被改写或概括后仍然存在的场景。
	// (2) 兜底：扫描消息中的 ForkBoilerplateTag。
	if t.QuerySource == ForkQuerySource {
		return tools.ToolResult{
			Output:  "Error: cannot fork from a forked agent. Use subagent_type to spawn a definition-based agent instead.",
			IsError: true,
		}
	}
	// 检查对话历史里有没有 fork 的痕迹，防止用户在 fork 里又去 fork。
	for _, msg := range t.Conversation.GetMessages() {
		if strings.Contains(msg.Content, ForkBoilerplateTag) {
			return tools.ToolResult{
				Output:  "Error: cannot fork from a forked agent. Use subagent_type to spawn a definition-based agent instead.",
				IsError: true,
			}
		}
	}

	// 构建 fork 后的对话：复制父消息 + 修补未完成的 tool_use + 追加任务。
	forkedConv := buildForkedConversation(t.Conversation, prompt)

	client := t.selectClient("", modelOverride)
	// fork 原样继承父 Agent 的工具池，这样发出去的请求前缀和父 Agent 逐字节一致，
	// prompt 缓存才命中得上。其中的 Agent 工具被换成一份 QuerySource=ForkQuerySource
	// 的浅拷贝，于是再往下 fork 会在 runFork 的首道检查那里被拒。
	subRegistry := cloneRegistryForFork(t.Registry)

	subAgent := agent.New(client, subRegistry, t.Protocol)
	subAgent.Checker = t.ParentChecker // fork 逐字节继承父 agent 的权限状态
	subAgent.MaxIterations = 200

	// Fork 总是在后台运行。
	taskName := "fork"
	if agentName != "" {
		taskName = agentName
	}
	taskID := t.TaskMgr.CreateTask(taskName + ": " + truncate(prompt, 50))
	forkCtx, cancel := context.WithCancel(ctx)
	t.TaskMgr.SetRunning(taskID, cancel)

	go func() {
		var output string
		ch := subAgent.Run(forkCtx, forkedConv)
		for ev := range ch {
			switch e := ev.(type) {
			case agent.StreamText:
				output += e.Text
			case agent.PermissionRequestEvent:
				// 无头模式：自动拒绝，避免 fork 在 respCh 上卡住。
				e.ResponseCh <- agent.PermDeny
			case agent.ErrorEvent:
				t.TaskMgr.SetFailed(taskID, e.Message)
				return
			}
		}
		t.TaskMgr.SetCompleted(taskID, output)
	}()

	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Forked agent \"%s\" launched in background (task %s). Results will arrive via task-notification.",
			description, taskID,
		),
	}
}

// emitProgress 发送 SubAgentProgress 事件，但绝不会阻塞调用方。如果消费者
// （TUI）落后，事件会被丢弃——进度只是尽力而为的 UI 反馈，不是承载关键状态的通道。
// 此前这里使用阻塞发送，当 ProgressCh 缓冲区填满时会导致子 agent 循环死锁，
// 进而使 ESC / ctx cancel 永远无法生效，因为子 agent 卡在这次发送上而不是
// 停在可感知 ctx 的位置。
func emitProgress(ch chan<- SubAgentProgress, ctx context.Context, p SubAgentProgress) {
	if ch == nil {
		return
	}
	select {
	case ch <- p:
	case <-ctx.Done():
	default:
		// 消费者落后了。丢弃事件，而不是阻塞子 agent 的事件循环。
	}
}

// deriveSubAgentChecker 把子 agent 的 permissionMode 贯穿到被派生的 agent 上：`mode`
// 覆盖 agent 定义里的 permissionMode，进而进入子 agent 的
// ToolPermissionContext。Sandbox 与 RuleEngine 是共享的——我们只替换 Mode，让
// 子 agent 的工具调用走不同的决策矩阵，同时不使权限状态分叉。
//
// 未请求覆盖时，原样返回父检查器。
func deriveSubAgentChecker(parent *permissions.Checker, modeOverride string) *permissions.Checker {
	if parent == nil {
		return nil
	}
	if modeOverride == "" || permissions.PermissionMode(modeOverride) == parent.Mode {
		return parent
	}
	return permissions.NewChecker(parent.Sandbox, parent.RuleEngine, permissions.PermissionMode(modeOverride))
}

// cloneRegistryForFork 返回一个逐字节复制父注册表的注册表，唯一的例外是
// 任何 *AgentTool 实例都会被替换成 QuerySource 设为 ForkQuerySource 的浅拷贝。
// 这样 fork 子 Agent 看到的工具定义和父 Agent 在协议层完全一样（prompt 缓存因此命中），
// 但它再想 fork 时会在调用那一刻被 runFork 里的 QuerySource 检查拦下。
func cloneRegistryForFork(reg *tools.Registry) *tools.Registry {
	forked := tools.NewRegistry()
	for _, tool := range reg.ListTools() {
		if at, ok := tool.(*AgentTool); ok {
			clone := *at
			clone.QuerySource = ForkQuerySource
			forked.Register(&clone)
			continue
		}
		forked.Register(tool)
	}
	return forked
}

const forkBoilerplate = ForkBoilerplateTag + `
You are a forked worker process. You are NOT the main agent.
Rules (non-negotiable):
1. Do NOT fork again.
2. Do NOT converse, ask questions, or request confirmation.
3. Use tools directly: read files, search code, make changes.
4. Stay strictly within your assigned task scope.
5. Final report must be under 500 characters, starting with "Scope:".
` + "</fork_boilerplate>"

func buildForkedConversation(parent *conversation.Manager, task string) *conversation.Manager {
	forked := conversation.NewManager()
	msgs := parent.GetMessages()

	// 逐字节精确重放：保留 thinking 块以及 tool_use，让 API 请求前缀与
	// 父请求完全一致（参见 "keeping all content blocks (thinking, text, and every tool_use)"）。
	// 缺少 thinking 块会改变助手消息的形态，破坏 prompt 缓存。
	for _, msg := range msgs {
		if len(msg.ToolUses) > 0 && len(msg.ToolResults) == 0 {
			forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, msg.ToolUses)
			var placeholders []conversation.ToolResultBlock
			for _, tu := range msg.ToolUses {
				placeholders = append(placeholders, conversation.ToolResultBlock{
					ToolUseID: tu.ToolUseID,
					Content:   "(tool execution interrupted by fork)",
					IsError:   false,
				})
			}
			forked.AddToolResultsMessage(placeholders)
		} else if len(msg.ToolUses) > 0 {
			forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, msg.ToolUses)
		} else if len(msg.ToolResults) > 0 {
			forked.AddToolResultsMessage(msg.ToolResults)
		} else if msg.Role == "assistant" {
			if len(msg.ThinkingBlocks) > 0 {
				forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, nil)
			} else {
				forked.AddAssistantMessage(msg.Content)
			}
		} else {
			forked.AddUserMessage(msg.Content)
		}
	}

	// 把 fork 样板文本 + 任务作为用户消息追加。
	forked.AddUserMessage(forkBoilerplate + "\n\nYour task:\n" + task)
	return forked
}

func (t *AgentTool) runAsync(ctx context.Context, spec SubAgentSpec, description, prompt, modelOverride string) tools.ToolResult {
	client := t.selectClient(spec.Model, modelOverride)
	taskID := SpawnSubAgent(ctx, t.TaskMgr, client, t.Registry, t.Protocol, spec, prompt, t.ParentChecker)

	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Agent \"%s\" launched in background (task %s). You will be notified when it completes.",
			description, taskID,
		),
	}
}

// runAsTeammate 在已有 Team 上注册一个常驻团队成员。与 runSync/runAsync 不同，
// 这条路径绝不会让 Lead 等待成员的输出：Lead 总是立即返回，
// 通过 SendMessage + 团队信箱里的空闲通知协调。后端（进程内
// / tmux / iTerm）由 teams.SpawnTeammate 根据 Team.Mode 选择。
//
// 当 isolation == "worktree" 且配置了 WorktreeMgr 时，队友会获得一个专属 git
// worktree，这样它的文件修改就不会与其他队友的工作冲突。
func (t *AgentTool) runAsTeammate(
	ctx context.Context,
	teamName, memberName, description, prompt, modelOverride, subagentType, isolation string,
	planModeRequired bool,
) tools.ToolResult {
	// 团队不存在就顺手建一个：coordinator 模式下 TeamCreate 不在白名单里，
	// 要求 Lead 先建团队再派人，它会卡在第一步。
	team := t.TeamMgr.GetTeam(teamName)
	if team == nil {
		team = t.TeamMgr.CreateTeamFull(teamName, teams.DetectBackend(), teams.LeadName, description)
	}

	if memberName == "" {
		memberName = sanitizeSlugSegment(description)
	}
	if _, exists := team.Members[memberName]; exists {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: team '%s' already has a member named '%s'", teamName, memberName),
			IsError: true,
		}
	}

	// 设置了 subagent_type 时解析 spec，让队友遵守与该类型其他子 agent 相同的禁用列表。
	// 没有 spec 时，把完整注册表交给队友。
	var spec SubAgentSpec
	if subagentType != "" {
		if t.Loader != nil {
			if def := t.Loader.Get(subagentType); def != nil {
				spec = def.ToSpec()
			}
		} else if s, ok := BuiltinSpecs[subagentType]; ok {
			spec = s
		}
	}

	teammateDisallowed := append(append([]string{}, spec.DisallowedTools...), TeammateDisallowedTools...)
	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, teammateDisallowed, false)
	// 队友协作工具：以队友自己的名字发消息，并注入团队共享任务板工具
	// （覆盖继承来的个人版同名工具，让队友之间共享同一份任务列表）。
	subRegistry.Register(&teams.SendMessageTool{TeamMgr: t.TeamMgr, SenderName: memberName})
	subRegistry.Register(&teams.TaskCreateTool{TeamMgr: t.TeamMgr, TeamName: teamName, AgentName: memberName})
	subRegistry.Register(&teams.TaskGetTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	subRegistry.Register(&teams.TaskListTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	subRegistry.Register(&teams.TaskUpdateTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	client := t.selectClient(spec.Model, modelOverride)

	var otherMembers []string
	for n := range team.Members {
		otherMembers = append(otherMembers, n)
	}
	addendum := teams.BuildTeammateAddendum(teamName, memberName, otherMembers)

	var workdir string
	if isolation == "worktree" {
		slug := generateAgentSlug(description)
		wtResult, err := worktree.CreateAgentWorktree(ctx, slug)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error creating teammate worktree: %s", err),
				IsError: true,
			}
		}
		workdir = wtResult.WorktreePath
		parentCwd, _ := os.Getwd()
		notice := worktree.BuildWorktreeNotice(parentCwd, wtResult.WorktreePath)
		prompt = notice + "\n\n" + prompt
	}

	team.SetMemberMeta(memberName, subagentType, modelOverride, workdir)

	// 标了 plan_mode_required 的队友以计划模式启动：只能读不能改，
	// 写出计划交 Lead 审批，通过后才切回正常权限。
	teammateChecker := t.ParentChecker
	if planModeRequired && t.ParentChecker != nil {
		teammateChecker = permissions.NewChecker(
			t.ParentChecker.Sandbox, t.ParentChecker.RuleEngine, permissions.ModePlan)
	}

	result, err := teams.SpawnTeammate(ctx, teams.TeammateSpawnConfig{
		Team:       team,
		MemberName: memberName,
		Checker:    teammateChecker,
		Task:       prompt,
		Addendum:   addendum,
		Client:     client,
		Registry:   subRegistry,
		Protocol:   t.Protocol,
		Workdir:    workdir,
	})
	if err != nil {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error spawning teammate: %v", err),
			IsError: true,
		}
	}

	// 进程内派生会交还一个实时事件通道；在后台排空它，避免 goroutine
	// 阻塞在已满的 channel 上。Lead 可见的进度走信箱，不经过这条排空路径。
	if result.Mode == teams.ModeInProcess && result.EventCh != nil {
		go drainTeammateEvents(memberName, result.EventCh, t.ProgressCh)
	}

	backendHint := string(result.Mode)
	if result.PaneID != "" {
		backendHint += " pane=" + result.PaneID
	}
	if workdir != "" {
		backendHint += " worktree=" + workdir
	}
	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Teammate \"%s\" started on team \"%s\" [%s]. Use SendMessage to talk to it; its idle notifications will arrive as system reminders.",
			memberName, teamName, backendHint,
		),
	}
}

// drainTeammateEvents 消费队友的事件流，让生产方永远不会因 channel 已满而阻塞。
// 设置 ProgressCh 时，工具/错误事件会被转发过去，让父级 UI 能显示活动。
func drainTeammateEvents(name string, ch <-chan agent.AgentEvent, progressCh chan<- SubAgentProgress) {
	for ev := range ch {
		if progressCh == nil {
			continue
		}
		switch e := ev.(type) {
		case agent.ToolResultEvent:
			emitProgress(progressCh, context.Background(), SubAgentProgress{
				AgentDesc: name,
				AgentType: "teammate",
				ToolName:  e.ToolName,
				Elapsed:   e.Elapsed.Seconds(),
				IsError:   e.IsError,
			})
		case agent.ErrorEvent:
			emitProgress(progressCh, context.Background(), SubAgentProgress{
				AgentDesc: name,
				AgentType: "teammate",
				ToolName:  "error",
				IsError:   true,
			})
		}
	}
}

// generateAgentSlug 生成匹配 ^agent-a[0-9a-f]{7}$ 的子 agent worktree slug。
func generateAgentSlug(description string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "agent-a" + hex.EncodeToString(b)[:7]
}

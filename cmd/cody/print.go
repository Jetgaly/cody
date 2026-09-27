package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cody/internal/agent"
	"cody/internal/agents"
	"cody/internal/config"
	"cody/internal/conversation"
	"cody/internal/filehistory"
	"cody/internal/hooks"
	"cody/internal/llm"
	"cody/internal/memory"
	"cody/internal/permissions"
	"cody/internal/prompt"
	"cody/internal/session"
	"cody/internal/skills"
	"cody/internal/teams"
	"cody/internal/todo"
	"cody/internal/tools"
	"cody/internal/worktree"
)

type printResult struct {
	Type       string         `json:"type"`
	Result     string         `json:"result"`
	DurationMs int64          `json:"duration_ms"`
	NumTurns   int            `json:"num_turns"`
	ToolCalls  []toolCallInfo `json:"tool_calls"`
	Usage      usageInfo      `json:"usage"`
	StopReason string         `json:"stop_reason"`
}

type toolCallInfo struct {
	Name    string `json:"name"`
	IsError bool   `json:"is_error"`
}

type usageInfo struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parsePrintFlags 从命令行参数中解析 -p/--print 模式相关参数
// 返回 prompt, outputFormat, ok
func parsePrintFlags(args []string) (string, string, bool) {
	isPrint := false
	outputFormat := "text"
	var prompt string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-p", "--print":
			isPrint = true
		case "--output-format":
			if i+1 < len(args) {
				outputFormat = args[i+1]
				i++
			}
		default:
			if isPrint && prompt == "" && !strings.HasPrefix(args[i], "-") {
				prompt = args[i]
			}
		}
	}

	return prompt, outputFormat, isPrint
}

func runPrint(userPrompt string, cfg *config.AppConfig, hookCfgs []hooks.Hook, outputFormat string) error {
	p := &cfg.Providers[0]
	wd, _ := os.Getwd()

	defaultTools := tools.CreateDefaultTools()
	registry := defaultTools.Registry

	skillCatalog := skills.LoadCatalog(wd)
	instructionsContent := loadPrintInstructions(wd)
	memoryContent := memory.LoadAutoMemoryPrompt(wd)
	skillSection := buildPrintSkillSection(skillCatalog)

	env := prompt.DetectEnvironment(wd)
	env.Model = p.Model
	// 指令与自动记忆由 conversation.InjectLongTermMemory 以 system-reminder
	// 消息注入对话，系统提示词这里只放 Skill。
	systemPrompt := prompt.BuildSystemPrompt(env, prompt.BuildOptions{
		SkillSection: skillSection,
	})

	client, err := llm.NewClient(p, systemPrompt)
	if err != nil {
		return err
	}

	conv := conversation.NewManager()
	sessionID := session.NewID()
	fh := filehistory.New(wd, sessionID)
	defaultTools.EditFile.FileHistory = fh
	defaultTools.WriteFile.FileHistory = fh

	llm.ResolveContextWindow(context.Background(), p)

	// 注册工具
	taskMgr := agents.NewTaskManager()
	store := todo.NewStore(wd, sessionID)
	todoList := todo.NewTaskList(store)
	loader := agents.NewAgentLoader(wd)
	loader.LoadAll()

	registry.Register(&todo.TaskCreateTool{List: todoList})
	registry.Register(&todo.TaskGetTool{List: todoList})
	registry.Register(&todo.TaskListTool{List: todoList})
	registry.Register(&todo.TaskUpdateTool{List: todoList})
	registry.Register(&tools.ToolSearchTool{Registry: registry, Protocol: p.Protocol})

	// 团队工具在 -p 模式下同样可用，Lead 能在一次非交互执行里组建团队把活派出去
	teamMgr := teams.NewTeamManager()
	registry.Register(&teams.TeamCreateTool{TeamMgr: teamMgr})
	registry.Register(&teams.TeamDeleteTool{TeamMgr: teamMgr})
	registry.Register(&teams.SendMessageTool{TeamMgr: teamMgr, SenderName: "lead"})
	registry.Register(&teams.TaskStopTool{TeamMgr: teamMgr})
	registry.Register(&tools.SyntheticOutputTool{})

	subProgressCh := make(chan agents.SubAgentProgress, 32)
	registry.Register(&agents.AgentTool{
		Client:        client,
		ModelResolver: llm.NewModelResolver(*p),
		Registry:      registry,
		Protocol:      p.Protocol,
		TaskMgr:       taskMgr,
		ProgressCh:    subProgressCh,
		Loader:        loader,
		Conversation:  conv,
		TeamMgr:       teamMgr,
		ForkDisabled:  !cfg.ForkEnabled(),
	})

	ag := agent.New(client, registry, p.Protocol)
	ag.ContextWindow = p.GetContextWindow()
	ag.MaxOutputTokens = p.GetMaxOutputTokens()
	ag.Instructions = instructionsContent
	ag.MemoryContent = memoryContent
	ag.FileHistory = fh
	ag.SetSessionID(sessionID)

	// print 模式自动允许所有权限
	sandboxAllow := []string{memory.GetAutoMemPath(wd)}
	if userMem := memory.GetUserAutoMemPath(); userMem != "" {
		sandboxAllow = append(sandboxAllow, userMem)
	}
	ag.Checker = permissions.NewChecker(
		permissions.NewPathSandbox(wd, sandboxAllow...),
		&permissions.RuleEngine{
			LocalPath: filepath.Join(wd, ".mewcode", "permissions.local.yaml"),
		},
		permissions.ModeBypass,
	)

	if len(hookCfgs) > 0 {
		eng := hooks.NewEngine()
		eng.LoadHooks(hookCfgs)
		ag.Hooks = eng
	}

	// 队员干完活的回传落在 lead 信箱里，每轮排空成 system-reminder 交给 Lead
	ag.NotificationFn = func() []string {
		return teams.DrainLeadMailbox(teamMgr)
	}
	ag.ToolNameFilter = teams.CoordinatorToolFilter(cfg.EnableCoordinatorMode)
	ag.CoordinatorActiveFn = teams.CoordinatorActiveFn(cfg.EnableCoordinatorMode)

	if at, ok := registry.Get("Agent").(*agents.AgentTool); ok {
		at.ParentChecker = ag.Checker
	}

	gitRoot := worktree.FindCanonicalGitRoot(wd)
	registry.Register(&tools.EnterWorktreeTool{SessionID: sessionID, RepoRoot: gitRoot})
	registry.Register(&tools.ExitWorktreeTool{RepoRoot: gitRoot})

	// 执行
	conv.AddUserMessage(userPrompt)
	ctx := context.Background()
	ch := ag.Run(ctx, conv)

	start := time.Now()
	var textBuf string
	var totalInput, totalOutput int
	var toolCalls []toolCallInfo
	isJSON := outputFormat == "stream-json"

	for ev := range ch {
		switch e := ev.(type) {
		case agent.StreamText:
			textBuf += e.Text
			if isJSON {
				emitJSON(map[string]any{"type": "assistant", "text": e.Text})
			}

		case agent.ThinkingText:
			if isJSON {
				emitJSON(map[string]any{"type": "thinking", "text": e.Text})
			}

		case agent.ToolUseEvent:
			toolCalls = append(toolCalls, toolCallInfo{Name: e.ToolName})
			if isJSON {
				emitJSON(map[string]any{
					"type":      "tool_use",
					"tool_name": e.ToolName,
					"tool_id":   e.ToolID,
					"args":      e.Args,
				})
			}

		case agent.ToolResultEvent:
			if len(toolCalls) > 0 {
				toolCalls[len(toolCalls)-1].IsError = e.IsError
			}
			if isJSON {
				emitJSON(map[string]any{
					"type":      "tool_result",
					"tool_name": e.ToolName,
					"tool_id":   e.ToolID,
					"output":    e.Output,
					"is_error":  e.IsError,
					"elapsed":   e.Elapsed.Seconds(),
				})
			}

		case agent.UsageEvent:
			totalInput = e.InputTokens
			totalOutput = e.OutputTokens
			if isJSON {
				emitJSON(map[string]any{
					"type":          "usage",
					"input_tokens":  e.InputTokens,
					"output_tokens": e.OutputTokens,
				})
			}

		case agent.TurnComplete:
			if isJSON {
				emitJSON(map[string]any{"type": "turn_complete", "turn": e.Turn})
			}

		case agent.LoopComplete:
			elapsed := time.Since(start)
			if isJSON {
				emitJSON(printResult{
					Type:       "result",
					Result:     textBuf,
					DurationMs: elapsed.Milliseconds(),
					NumTurns:   e.TotalTurns,
					ToolCalls:  toolCalls,
					Usage:      usageInfo{InputTokens: totalInput, OutputTokens: totalOutput},
					StopReason: "end_turn",
				})
			} else {
				fmt.Print(textBuf)
			}
			return nil

		case agent.ErrorEvent:
			if isJSON {
				emitJSON(map[string]any{"type": "error", "message": e.Message})
			} else {
				fmt.Fprintf(os.Stderr, "Error: %s\n", e.Message)
			}

		case agent.CompactEvent:
			if isJSON {
				emitJSON(map[string]any{"type": "compact", "message": e.Message})
			}

		case agent.RetryEvent:
			if isJSON {
				emitJSON(map[string]any{"type": "retry", "reason": e.Reason})
			}
		}
	}

	return nil
}

func emitJSON(v any) {
	data, _ := json.Marshal(v)
	fmt.Println(string(data))
}

func loadPrintInstructions(wd string) string {
	paths := []string{
		filepath.Join(wd, ".mewcode", "instructions.md"),
		filepath.Join(wd, "MEWCODE.md"),
	}
	var parts []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			parts = append(parts, string(data))
		}
	}
	return strings.Join(parts, "\n\n")
}

func buildPrintSkillSection(catalog *skills.Catalog) string {
	if catalog == nil {
		return ""
	}
	metas := catalog.List()
	if len(metas) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	for _, meta := range metas {
		desc := meta.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		sb.WriteString(fmt.Sprintf("- /%s: %s\n", meta.Name, desc))
	}
	return sb.String()
}

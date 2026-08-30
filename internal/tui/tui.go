package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"cody/internal/agent"
	"cody/internal/agents"
	"cody/internal/commands"
	"cody/internal/compact"
	"cody/internal/config"
	"cody/internal/conversation"
	"cody/internal/filehistory"
	"cody/internal/history"
	"cody/internal/hooks"
	"cody/internal/llm"
	"cody/internal/mcp"
	"cody/internal/memory"
	"cody/internal/memory/consolidation"
	"cody/internal/memory/extractor"
	"cody/internal/permissions"
	"cody/internal/planfile"
	"cody/internal/prompt"
	"cody/internal/sandbox"
	"cody/internal/session"
	"cody/internal/skills"
	"cody/internal/teams"
	"cody/internal/todo"
	"cody/internal/tools"
	"cody/internal/worktree"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/rivo/uniseg"
)

type appState int

const (
	stateProviderSelect appState = iota
	stateChat
	stateResume
)

type chatMessage struct {
	role          string
	content       string
	toolGroup     []toolBlockInfo
	subAgentBlock *subAgentBlock
	expanded      bool
}

type subAgentBlock struct {
	desc      string
	agentType string
	toolUses  []toolBlockInfo
	done      bool
	toolCount int
	totalTime float64
}

type toolBlockInfo struct {
	toolName  string
	args      map[string]any
	output    string
	isError   bool
	elapsed   float64
	collapsed bool
	loading   bool
}

type agentEventMsg struct {
	event agent.AgentEvent
	ch    <-chan agent.AgentEvent
}
type agentDoneMsg struct {
	ch <-chan agent.AgentEvent
}
type agentErrMsg struct{ err error }
type agentReadyMsg struct {
	ch <-chan agent.AgentEvent
}
type mailboxPollMsg struct{}
type mcpReadyMsg struct{ result mcp.ConnectResult }
type compactDoneMsg struct {
	message string
	err     error
}

// forkSkillDoneMsg 在 fork 模式的 Skill 子代理达到 LoopComplete（或失败）时
// 被派发。Update 把 result 作为单条 assistant chatMessage 注入主对话日志，
// 让用户看到子代理的最终回答，而不会污染父代理的上下文窗口。
type forkSkillDoneMsg struct {
	name   string
	result string
	err    error
}

type Model struct {
	providers        []config.ProviderConfig
	selectedProvider *config.ProviderConfig
	client           llm.Client
	registry         *tools.Registry
	ag               *agent.Agent

	state     appState
	streaming bool

	textarea textarea.Model
	viewport viewport.Model
	width    int
	height   int
	ready    bool

	providerCursor int

	conversation *conversation.Manager

	chatMessages []chatMessage
	toolBlocks   []toolBlockInfo
	streamBuf    string
	agentCh      <-chan agent.AgentEvent
	cancelStream context.CancelFunc

	totalInput  int
	totalOutput int

	permDialog   bool
	permToolName string
	permDesc     string
	permRespCh   chan<- agent.PermissionResponse
	permCursor   int

	cmdRegistry   *commands.Registry
	slashMenuOpen bool
	slashMatches  []*commands.Command
	slashCursor   int

	userScrolled  bool
	committedUpTo int
	bannerPrinted bool

	atMenuOpen bool
	atMatches  []string
	atCursor   int
	atPrefix   string

	spinner       spinner.Model
	thinking      bool
	thinkingStart time.Time
	thinkingDone  float64
	thinkingVerb  string

	instructionsContent string
	memoryContent       string

	mcpConfigs        []config.MCPServerConfig
	mcpMgr            *mcp.Manager
	mcpConnecting     bool
	mcpInstructions   string
	mcpInstructionsOK bool
	mcpServerInfo     string
	hookConfigs       []hooks.Hook

	historyEntries []string
	historyIndex   int
	historyDraft   string

	sessionID    string
	fileHistory  *filehistory.History
	defaultTools tools.DefaultTools
	prePlanMode  permissions.PermissionMode

	planApprovalDialog bool
	planApprovalCursor int
	planApprovalInput  string

	rewindDialog       bool
	rewindSnapshots    []filehistory.Snapshot
	rewindCursor       int
	rewindPhase        int // 0=选择检查点，1=选择恢复选项
	rewindOptionCursor int

	askUserCh          chan tools.AskUserRequest
	subAgentProgressCh chan agents.SubAgentProgress
	activeSubAgent     *subAgentBlock
	askUserDialog      bool
	askUserQuestions   []tools.Question
	askUserCursors     []int
	askUserSelected    []map[int]bool
	askUserOther       []string
	askUserQIdx        int
	askUserRespCh      chan tools.QuestionResponse
	askUserAnswered    map[int]string
	askUserOnSubmit    bool
	askUserSubmitIdx   int
	skillCatalog       *skills.Catalog
	taskMgr            *agents.TaskManager
	todoList           *todo.TaskList
	memoryMgr          *memory.Manager
	memoryExtractor    *extractor.Extractor
	memoryConsolidator *consolidation.Consolidator
	teamMgr            *teams.TeamManager

	sandboxDialog         bool                 // 沙箱模式选择对话框是否打开
	sandboxCursor         int                  // 当前选中的沙箱模式索引
	sandboxCfg            config.SandboxConfig // 配置文件中的沙箱设置
	EnableCoordinatorMode bool                 // Coordinator 模式配置开关
	ForkDisabled          bool                 // 关掉 fork 后，省略 subagent_type 回退到通用 agent

	resumeSessions  []session.SessionInfo
	resumeFiltered  []session.SessionInfo
	resumeCursor    int
	resumeSearch    string
	resumeScrollTop int

	hasExitedPlanMode bool // 记录本次会话是否曾退出过 Plan Mode，用于重入时注入提示
}

func New(providers []config.ProviderConfig, mcpConfigs []config.MCPServerConfig, hookConfigs []hooks.Hook, sandboxCfg ...config.SandboxConfig) Model {
	ta := textarea.New()
	ta.Placeholder = "Send a message..."
	ta.Prompt = ""
	ta.CharLimit = 0
	ta.MaxHeight = 0
	ta.ShowLineNumbers = false
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	ta.SetHeight(1)

	sp := spinner.New()
	sp.Spinner = spinner.Spinner{
		Frames: []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
		FPS:    80 * time.Millisecond,
	}
	sp.Style = lipgloss.NewStyle().Foreground(brandPurple)

	askCh := make(chan tools.AskUserRequest, 1)
	subProgressCh := make(chan agents.SubAgentProgress, 32)
	dt := tools.CreateDefaultTools()
	reg := dt.Registry
	reg.Register(&tools.AskUserQuestionTool{RequestCh: askCh})

	var sCfg config.SandboxConfig
	if len(sandboxCfg) > 0 {
		sCfg = sandboxCfg[0]
	}

	m := Model{
		providers:          providers,
		mcpConfigs:         mcpConfigs,
		hookConfigs:        hookConfigs,
		sandboxCfg:         sCfg,
		state:              stateProviderSelect,
		textarea:           ta,
		conversation:       conversation.NewManager(),
		registry:           reg,
		defaultTools:       dt,
		cmdRegistry:        commands.CreateDefaultRegistry(),
		spinner:            sp,
		askUserCh:          askCh,
		subAgentProgressCh: subProgressCh,
	}

	if len(providers) == 1 {
		m.state = stateChat
	}

	return m
}

type initSingleProviderMsg struct{}

func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{textarea.Blink}
	if len(m.providers) == 1 {
		cmds = append(cmds, func() tea.Msg { return initSingleProviderMsg{} })
	}
	return tea.Batch(cmds...)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.textarea.SetWidth(msg.Width - 4)

		statusHeight := 1
		sepHeight := 2 // 输入框上下各一条分隔线
		inputHeight := m.textarea.Height() + 1
		vpHeight := msg.Height - statusHeight - sepHeight - inputHeight - 1
		if vpHeight < 1 {
			vpHeight = 1
		}

		if !m.ready {
			m.viewport = viewport.New(msg.Width, vpHeight)
			m.viewport.MouseWheelEnabled = false
			m.ready = true
			m.bannerPrinted = true
			m.updateViewport()
			return m, tea.Println(m.renderBanner() + "\n")
		} else {
			m.viewport.Width = msg.Width
			m.viewport.Height = vpHeight
		}
		m.updateViewport()
		return m, nil

	case subAgentProgressMsg:
		p := msg.progress
		if p.Done {
			if m.activeSubAgent != nil {
				m.activeSubAgent.done = true
				m.activeSubAgent.toolCount = p.ToolCount
				m.activeSubAgent.totalTime = p.TotalTime
			}
		} else {
			if m.activeSubAgent == nil || m.activeSubAgent.done {
				m.activeSubAgent = &subAgentBlock{
					desc:      p.AgentDesc,
					agentType: p.AgentType,
				}
			}
			m.activeSubAgent.toolUses = append(m.activeSubAgent.toolUses, toolBlockInfo{
				toolName: p.ToolName,
				args:     p.ToolArgs,
				elapsed:  p.Elapsed,
				isError:  p.IsError,
			})
		}
		m.updateViewport()
		return m, m.listenForSubAgentProgress()

	case askUserMsg:
		m.askUserDialog = true
		m.askUserQuestions = msg.req.Questions
		m.askUserRespCh = msg.req.ResponseCh
		m.askUserQIdx = 0
		m.askUserCursors = make([]int, len(msg.req.Questions))
		m.askUserSelected = make([]map[int]bool, len(msg.req.Questions))
		m.askUserOther = make([]string, len(msg.req.Questions))
		m.askUserAnswered = make(map[int]string)
		m.askUserOnSubmit = false
		m.askUserSubmitIdx = 0
		for i := range msg.req.Questions {
			m.askUserSelected[i] = make(map[int]bool)
		}
		m.updateViewport()
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.streaming && m.cancelStream != nil {
				m.cancelStream()
				m.savePartialResponse()
				m.finishStreaming()
				return m, nil
			}
			if m.cancelStream != nil {
				m.cancelStream()
			}
			if m.mcpMgr != nil {
				m.mcpMgr.Shutdown()
			}
			if m.memoryExtractor != nil {
				_ = m.memoryExtractor.Drain(5000)
			}
			return m, tea.Quit
		}

		if m.permDialog {
			return m.handlePermDialog(msg)
		}

		switch m.state {
		case stateProviderSelect:
			return m.handleProviderSelect(msg)
		case stateChat:
			return m.handleChat(msg)
		case stateResume:
			return m.handleResumeKeys(msg)
		}

	case initSingleProviderMsg:
		p := &m.providers[0]
		m.selectedProvider = p
		wd, _ := os.Getwd()
		systemPrompt := m.loadSkillsAndBuildPrompt(wd)
		client, err := llm.NewClient(p, systemPrompt)
		if err != nil {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: err.Error()})
			m.updateViewport()
			return m, nil
		}
		m.client = client
		m.sessionID = session.NewID()
		m.fileHistory = filehistory.New(wd, m.sessionID)
		m.defaultTools.EditFile.FileHistory = m.fileHistory
		m.defaultTools.WriteFile.FileHistory = m.fileHistory
		m.registerAgentTools(client, p, p.Protocol, wd)
		// 尽力而为：从 provider 拉取一次模型的上下文窗口（仅限 Anthropic），
		// 并在 GetContextWindow 读取之前缓存到 p 上。任何失败都会静默降级到
		// 映射表 / 默认值。
		llm.ResolveContextWindow(context.Background(), p)
		ag := agent.New(client, m.registry, p.Protocol)
		ag.ContextWindow = p.GetContextWindow()
		ag.MaxOutputTokens = p.GetMaxOutputTokens()
		ag.Instructions = m.instructionsContent
		ag.MemoryContent = m.memoryContent
		ag.FileHistory = m.fileHistory
		ag.SetSessionID(m.sessionID)
		sandboxAllow := []string{memory.GetAutoMemPath(wd)}
		if userMem := memory.GetUserAutoMemPath(); userMem != "" {
			sandboxAllow = append(sandboxAllow, userMem)
		}
		pathSandbox := permissions.NewPathSandbox(wd, sandboxAllow...)
		ag.Checker = permissions.NewChecker(
			pathSandbox,
			&permissions.RuleEngine{
				LocalPath: filepath.Join(wd, ".cody", "permissions.local.yaml"),
			},
			permissions.ModeDefault,
		)
		// 根据配置文件初始化 OS 级沙箱
		if m.sandboxCfg.Enabled {
			sb := sandbox.New()
			if bashTool, ok := m.registry.Get("Bash").(*tools.BashTool); ok && sb != nil {
				bashTool.Sandbox = sb
				bashTool.SandboxConfig = sandbox.Config{
					AllowWrite:     pathSandbox.GetAllowedRoots(),
					DenyWrite:      pathSandbox.GetDenyWrite(),
					NetworkEnabled: m.sandboxCfg.NetworkEnabled,
				}
			}
			if m.sandboxCfg.AutoAllow {
				ag.Checker.SandboxEnabled = true
			}
		}
		ag.NotificationFn = m.drainTaskNotifications
		ag.ToolNameFilter = teams.CoordinatorToolFilter(m.EnableCoordinatorMode)
		ag.CoordinatorActiveFn = teams.CoordinatorActiveFn(m.EnableCoordinatorMode)
		if len(m.hookConfigs) > 0 {
			eng := hooks.NewEngine()
			eng.LoadHooks(m.hookConfigs)
			eng.AgentRunner = newAgentHookRunner(client)
			ag.Hooks = eng
		}
		m.ag = ag
		if at, ok := m.registry.Get("Agent").(*agents.AgentTool); ok {
			at.ParentChecker = ag.Checker
		}
		m.wireSkillsToAgent()
		m.memoryExtractor = m.installMemoryExtractor(ag, wd, p.Protocol)
		m.historyEntries = history.Load(wd)
		m.textarea.Focus()
		m.updateViewport()
		return m, m.initMCPServersCmd()

	case forkSkillDoneMsg:
		var commit string
		if msg.err != nil {
			line := fmt.Sprintf("Skill %s (fork) failed: %v", msg.name, msg.err)
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: line})
			commit = errorStyle.Render("✖ " + line)
		} else {
			result := strings.TrimSpace(msg.result)
			if result == "" {
				result = fmt.Sprintf("(Skill %s returned no output)", msg.name)
			}
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: result})
			commit = m.renderMessagesRange(len(m.chatMessages)-1, len(m.chatMessages))
		}
		m.committedUpTo = len(m.chatMessages)
		m.updateViewport()
		if commit != "" {
			return m, tea.Println(commit)
		}
		return m, nil

	case compactDoneMsg:
		switch {
		case msg.err != nil:
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: "Compact failed: " + msg.err.Error()})
		case msg.message == "":
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: "Compact: no changes."})
		default:
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: "Compact: " + msg.message})
		}
		m.updateViewport()
		return m, nil

	case mcpReadyMsg:
		m.mcpConnecting = false
		m.mcpMgr = msg.result.Mgr
		for _, t := range msg.result.Tools {
			m.registry.Register(t)
		}
		var mcpPrintLines []string
		for _, errMsg := range msg.result.Errors {
			m.chatMessages = append(m.chatMessages, chatMessage{
				role:    "error",
				content: errMsg,
			})
			mcpPrintLines = append(mcpPrintLines, errorStyle.Render("✖ "+errMsg))
		}
		registered := len(msg.result.Tools)
		if registered > 0 {
			m.mcpServerInfo = fmt.Sprintf("Connected to %d MCP server(s), %d tools registered", len(m.mcpConfigs)-len(msg.result.Errors), registered)
		}
		m.committedUpTo = len(m.chatMessages)
		// 构建 MCP 指令，用于注入系统提示词
		if len(msg.result.Servers) > 0 {
			// 按服务器对已注册的工具名进行分组
			toolsByServer := make(map[string][]string)
			for _, t := range msg.result.Tools {
				toolName := t.Name()
				for _, srv := range msg.result.Servers {
					if strings.HasPrefix(toolName, "mcp__"+mcp.SanitizeName(srv.Name)+"__") {
						toolsByServer[srv.Name] = append(toolsByServer[srv.Name], toolName)
						break
					}
				}
			}

			var mcpParts []string
			for _, srv := range msg.result.Servers {
				var sb strings.Builder
				sb.WriteString(fmt.Sprintf("## %s\n", srv.Name))
				if srv.Instructions != "" {
					sb.WriteString(srv.Instructions + "\n")
				}
				if toolNames, ok := toolsByServer[srv.Name]; ok && len(toolNames) > 0 {
					sb.WriteString("\nAvailable tools: " + strings.Join(toolNames, ", "))
				}
				mcpParts = append(mcpParts, sb.String())
			}
			m.mcpInstructions = "# MCP Server Instructions\n\nThe following MCP servers are connected. Use their tools when the user asks.\n\n" + strings.Join(mcpParts, "\n\n")
		}
		m.updateViewport()
		if len(mcpPrintLines) > 0 {
			return m, tea.Println(strings.Join(mcpPrintLines, "\n"))
		}
		return m, nil

	case spinner.TickMsg:
		if m.streaming {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			m.updateViewport()
			return m, cmd
		}

	case agentReadyMsg:
		m.agentCh = msg.ch
		return m, m.listenForAgentEvents()

	case agentEventMsg:
		if msg.ch != m.agentCh {
			return m, nil
		}
		return m.handleAgentEvent(msg.event)

	case agentDoneMsg:
		if msg.ch != m.agentCh {
			return m, nil
		}
		m.finishStreaming()
		return m, nil

	case agentErrMsg:
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: msg.err.Error(),
		})
		m.finishStreaming()
		return m, nil

	case mailboxPollMsg:
		if m.streaming {
			return m, nil
		}
		notifications := teams.DrainLeadMailbox(m.teamMgr)
		if len(notifications) == 0 {
			return m, m.pollMailbox()
		}
		for _, note := range notifications {
			m.conversation.AddSystemReminder(note)
		}
		m.streaming = true
		m.thinking = true
		m.thinkingStart = time.Now()
		m.thinkingDone = 0
		m.thinkingVerb = randomVerb()
		m.streamBuf = ""
		m.toolBlocks = nil
		m.userScrolled = false
		ctx, cancel := context.WithCancel(context.Background())
		m.cancelStream = cancel
		m.agentCh = m.ag.Run(ctx, m.conversation)
		m.updateViewport()
		return m, tea.Batch(
			m.listenForAgentEvents(),
			m.listenForAskUser(),
			m.listenForSubAgentProgress(),
			m.spinner.Tick,
		)
	}

	return m, nil
}

func (m *Model) drainTaskNotifications() []string {
	var messages []string
	if m.taskMgr != nil {
		for _, n := range m.taskMgr.DrainNotifications() {
			msg := fmt.Sprintf("<task-notification>\n<task_id>%s</task_id>\n<status>%s</status>\n<summary>Agent \"%s\" %s</summary>\n<result>%s</result>\n</task-notification>",
				n.TaskID, n.Status, n.Name, n.Status, n.Output)
			messages = append(messages, msg)
		}
	}
	// 队友的空闲通知会落在 lead 的收件箱；将它们以 system-reminder 形式呈现，
	// 使 Lead 模型在下一轮开头就能看到，并可以分派后续工作。
	messages = append(messages, teams.DrainLeadMailbox(m.teamMgr)...)
	// Hook 通知（post_tool_use 输出、异步 hook 结果等）会排空到
	// system-reminder 中，让模型看到副作用。
	if m.ag != nil && m.ag.Hooks != nil {
		for _, r := range m.ag.Hooks.DrainNotifications() {
			if r.Output == "" || r.Output == "(async)" {
				continue
			}
			messages = append(messages, fmt.Sprintf("<hook-notification id=%q>\n%s\n</hook-notification>", r.HookID, r.Output))
		}
	}
	return messages
}

// newAgentHookRunner 构建 `type: agent` hook 所使用的 AgentRunner 闭包。
// hook 提示词作为单条用户消息发送给主代理使用的同一个 LLM，且不带工具
// 注册表——输出是原始的 assistant 文本，会回到通知队列并排空到下一轮的
// 系统提醒中。
func newAgentHookRunner(client llm.Client) func(prompt string, ctx hooks.HookContext) (string, error) {
	return func(prompt string, _ hooks.HookContext) (string, error) {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conv := conversation.NewManager()
		conv.AddUserMessage(prompt)
		events, errs := client.Stream(c, conv, nil)
		var text string
		for ev := range events {
			if td, ok := ev.(llm.TextDelta); ok {
				text += td.Text
			}
		}
		select {
		case err := <-errs:
			if err != nil {
				return "", err
			}
		default:
		}
		return text, nil
	}
}

func (m *Model) registerAgentTools(client llm.Client, providerCfg *config.ProviderConfig, protocol, wd string) {
	m.taskMgr = agents.NewTaskManager()

	store := todo.NewStore(wd, m.sessionID)
	m.todoList = todo.NewTaskList(store)

	m.memoryMgr = memory.NewManager(wd)

	loader := agents.NewAgentLoader(wd)
	loader.LoadAll()

	teamMgr := teams.NewTeamManager()
	m.teamMgr = teamMgr

	// 接线 worktree 工具（T9：会话恢复，T13-T15：LLM 工具，T17：清理）
	gitRoot := worktree.FindCanonicalGitRoot(wd)
	m.registry.Register(&tools.EnterWorktreeTool{
		SessionID: m.sessionID,
		RepoRoot:  gitRoot,
	})
	m.registry.Register(&tools.ExitWorktreeTool{
		RepoRoot: gitRoot,
	})

	// 从上次崩溃中恢复 worktree 会话（T9）
	if gitRoot != "" {
		if savedSession, err := worktree.LoadWorktreeSession(gitRoot); err == nil && savedSession != nil {
			if info, err := os.Stat(savedSession.WorktreePath); err == nil && info.IsDir() {
				worktree.RestoreWorktreeSession(savedSession)
			}
		}
	}

	// 启动后台过时 worktree 清理（T17）
	worktree.StartCleanupLoop(context.Background())

	m.registry.Register(&tools.ExitPlanModeTool{
		IsPlanMode: func() bool {
			return m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Mode == permissions.ModePlan
		},
		PlanExists: func() bool {
			wd, _ := os.Getwd()
			return planfile.PlanExists(wd)
		},
	})
	m.registry.Register(&todo.TaskCreateTool{List: m.todoList})
	m.registry.Register(&todo.TaskGetTool{List: m.todoList})
	m.registry.Register(&todo.TaskListTool{List: m.todoList})
	m.registry.Register(&todo.TaskUpdateTool{List: m.todoList})
	m.registry.Register(&tools.ToolSearchTool{Registry: m.registry, Protocol: protocol})
	m.registry.Register(&teams.TeamCreateTool{TeamMgr: teamMgr})
	m.registry.Register(&teams.TeamDeleteTool{TeamMgr: teamMgr})
	m.registry.Register(&teams.SendMessageTool{TeamMgr: teamMgr, SenderName: "lead"})
	m.registry.Register(&teams.TaskStopTool{TeamMgr: teamMgr})
	m.registry.Register(&tools.SyntheticOutputTool{})
	m.registry.Register(&agents.AgentTool{
		Client:        client,
		ModelResolver: llm.NewModelResolver(*providerCfg),
		Registry:      m.registry,
		Protocol:      protocol,
		TaskMgr:       m.taskMgr,
		ProgressCh:    m.subAgentProgressCh,
		Loader:        loader,
		Conversation:  m.conversation,
		TeamMgr:       teamMgr,
		ForkDisabled:  m.ForkDisabled,
		// ParentChecker 在下面 m.ag.Checker 构造完成后才接线——
		// registerAgentTools 在主代理的 Checker 设置之前运行。
	})

}

func (m *Model) initMCPServersCmd() tea.Cmd {
	if len(m.mcpConfigs) == 0 {
		return nil
	}

	m.mcpConnecting = true
	configs := m.mcpConfigs

	return func() tea.Msg {
		mgr := mcp.NewManager()
		var serverConfigs []mcp.ServerConfig
		for _, c := range configs {
			serverConfigs = append(serverConfigs, mcp.ServerConfig{
				Name:      c.Name,
				Command:   c.Command,
				Args:      c.Args,
				URL:       c.URL,
				Transport: c.Transport,
				Headers:   c.Headers,
				Env:       c.Env,
			})
		}
		mgr.LoadConfigs(serverConfigs)
		return mcpReadyMsg{result: mgr.ConnectAll(context.Background())}
	}
}

func (m *Model) loadSkillsAndBuildPrompt(wd string) string {
	m.skillCatalog = skills.LoadCatalog(wd)

	for _, cmd := range commands.LoadUserCommands(wd) {
		if m.cmdRegistry.HasConflict(cmd) {
			continue
		}
		m.cmdRegistry.Register(cmd)
	}

	return m.rebuildSystemPrompt(wd)
}

// wireSkillsToAgent 完成 loadSkillsAndBuildPrompt 无法完成的 Skill 初始化，
// 因为那时 Agent 尚未构造：注册每个 Skill 的斜杠命令（inline 与 fork 模式
// 的派发方式不同）并安装 LoadSkillTool。必须在 m.ag 赋值后、处理第一条
// 用户输入前立即调用。
//
// 幂等：对已被占用的斜杠命令名静默跳过，与 loadSkillsAndBuildPrompt 中
// LoadUserCommands 的优先级保持一致，并遵守项目规则「不要改变现有命令集」。
func (m *Model) wireSkillsToAgent() {
	if m.skillCatalog == nil || m.ag == nil {
		return
	}
	for _, meta := range m.skillCatalog.List() {
		m.registerSkillCommand(meta.Name)
	}
	m.registry.Register(&skills.LoadSkillTool{
		Catalog:  m.skillCatalog,
		Host:     m,
		ForkHost: m,
	})
	m.registry.Register(&skills.InstallSkillTool{
		Catalog: m.skillCatalog,
		OnInstalled: func(name string) {
			m.registerSkillCommand(name)
			if m.client != nil {
				wd, _ := os.Getwd()
				m.client.SetSystemPrompt(m.rebuildSystemPrompt(wd))
			}
		},
	})
}

// registerSkillCommand 接线单个技能的斜杠命令。Inline 技能通过 TypePrompt +
// skills.RunInline 路由，这样 SOP 会被固定到上下文，allowed_tools 过滤也会
// 生效。Fork 技能通过 TypeSkillFork 路由，使派发器可以卸载到 goroutine +
// 子代理。
//
// 幂等：如果命令名已被占用则静默返回。从 wireSkillsToAgent 中抽出，以便
// InstallSkillTool 的 OnInstalled hook 可以重新注册单个新获取的技能，而无需
// 重跑完整的启动循环。
func (m *Model) registerSkillCommand(name string) {
	if m.skillCatalog == nil || m.cmdRegistry == nil {
		return
	}
	if m.cmdRegistry.Find(name) != nil {
		return
	}
	meta := m.skillCatalog.Get(name)
	if meta == nil {
		return
	}
	cmd := &commands.Command{
		Name:        name,
		Description: meta.Meta.Description + " [skill]",
	}
	if meta.Meta.IsFork() {
		cmd.Type = commands.TypeSkillFork
		cmd.Handler = func(ctx *commands.Context) string {
			// Handler 在 fork 派发中不会被用到——executeCommand 会在调用
			// Handler 之前先按 TypeSkillFork 分支。保持它非 nil，使那些依赖
			// Handler 是否存在的旧代码路径仍然可用。
			return ""
		}
	} else {
		cmd.Type = commands.TypePrompt
		captured := name
		cmd.Handler = func(ctx *commands.Context) string {
			skill, err := m.skillCatalog.GetFull(captured)
			if err != nil && skill == nil {
				return fmt.Sprintf("[skill error] %v", err)
			}
			body, runErr := skills.RunInline(context.Background(), skill, ctx.Args, m)
			if runErr != nil {
				return fmt.Sprintf("[skill error] %v", runErr)
			}
			if m.ag != nil {
				m.ag.RecoveryState.RecordSkillInvocation(skill.Meta.Name, body)
			}
			return body
		}
	}
	m.cmdRegistry.Register(cmd)
}

// refreshSkillsIfNeeded 检查自上次加载 catalog 以来技能目录是否发生变化。
// 若变化则重新加载 catalog、注册任何新的斜杠命令，并更新 LLM 客户端的
// 系统提示词，使模型能看到新增的技能。
func (m *Model) refreshSkillsIfNeeded() {
	if m.skillCatalog == nil || m.client == nil {
		return
	}
	if !m.skillCatalog.NeedsReload() {
		return
	}
	wd, _ := os.Getwd()
	m.skillCatalog.Reload(wd)
	for _, meta := range m.skillCatalog.List() {
		m.registerSkillCommand(meta.Name)
	}
	m.client.SetSystemPrompt(m.rebuildSystemPrompt(wd))
}

// rebuildSystemPrompt 根据当前状态（技能、自定义指令、记忆）重新生成完整
// 系统提示词。由 refreshSkillsIfNeeded 和 /skill reload 使用。
func (m *Model) rebuildSystemPrompt(wd string) string {
	skillSection := m.buildSkillSection(wd)
	m.instructionsContent = m.loadCustomInstructions(wd)
	m.memoryContent = memory.LoadAutoMemoryPrompt(wd)
	env := prompt.DetectEnvironment(wd)
	if m.selectedProvider != nil {
		env.Model = m.selectedProvider.Model
	}
	// 指令与自动记忆由 conversation.InjectLongTermMemory 以 system-reminder
	// 消息注入对话，系统提示词这里只放 Skill。
	return prompt.BuildSystemPrompt(env, prompt.BuildOptions{
		SkillSection: skillSection,
	})
}

// buildSkillSection 根据当前 catalog 生成「## Available Skills」提示词段落。
// 从 loadSkillsAndBuildPrompt 中抽出，以便 rebuildSystemPrompt 复用。
func (m *Model) buildSkillSection(wd string) string {
	if m.skillCatalog == nil {
		return ""
	}
	metas := m.skillCatalog.List()
	if len(metas) == 0 {
		return ""
	}
	skillsDir := filepath.Join(wd, ".cody", "skills")
	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	sb.WriteString(fmt.Sprintf("Skills are installed at: %s\n", skillsDir))
	sb.WriteString("When creating new skills, always place them under this directory as <skill-name>/SKILL.md.\n\n")
	sb.WriteString("Only Skill names and one-line descriptions are listed below. To activate a Skill on demand call the LoadSkill tool with {name: \"<skill-name>\"}. After activation the Skill's full SOP gets pinned to the environment context, and any tools the Skill declares get registered. Users can also invoke a Skill directly with /<name>.\n\n")
	sb.WriteString("If the user pastes a Skill URL (skills.sh, github.com tree URL, or raw SKILL.md URL) and asks to install / add / get it, call the InstallSkill tool with {url: \"<url>\"} — the new Skill becomes available immediately afterwards.\n\n")
	for _, meta := range metas {
		desc := meta.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		sb.WriteString(fmt.Sprintf("- /%s: %s\n", meta.Name, desc))
	}
	return sb.String()
}

// ----- *Model 上的 SkillForkHost 实现 -----

// ActivateSkill 委托给底层 Agent，使 RunInline 可以把 SOP 固定到 env
// 上下文。在 m.ag 存在之前调用也是安全的（no-op）。
func (m Model) ActivateSkill(name, body string) {
	if m.ag != nil {
		m.ag.ActivateSkill(name, body)
	}
}

// SetToolFilter 在 Agent 上安装工具可见性过滤器（用于 Teams coordinator
// 模式）。传入 nil 会清除过滤器。
func (m Model) SetToolFilter(allow func(name string) bool) {
	if m.ag == nil {
		return
	}
	m.ag.SetToolFilter(allow)
}

// ToolRegistry 暴露实时的注册表，用于 fail-fast 检查和目录类型工具的注册。
func (m Model) ToolRegistry() *tools.Registry {
	return m.registry
}

// SnapshotParentMessages 复制当前主对话的消息日志，使 fork 执行器可以按照
// fork_context 为子代理植入种子消息。返回浅拷贝；调用方不得修改该切片。
func (m Model) SnapshotParentMessages() []conversation.Message {
	if m.conversation == nil {
		return nil
	}
	src := m.conversation.GetMessages()
	out := make([]conversation.Message, len(src))
	copy(out, src)
	return out
}

// RunSubAgent 在隔离的子代理中把 `body` 作为第一条用户消息运行。子代理获得
// 一个遵循 allowedTools 的过滤注册表（系统工具始终放行），并使用与主循环
// 相同的 LLM client / protocol。会阻塞直到子代理达到 LoopComplete 或出错；
// 返回最终的 assistant 文本。
//
// 调用方应在一个 goroutine 上派发它（通过 tea.Cmd）——这里的 channel 排空
// 是同步的，如果在 bubbletea 的 Update 路径上调用会冻结 UI。
func (m Model) RunSubAgent(ctx context.Context, body string, seed []conversation.Message, _ string) (string, error) {
	if m.client == nil {
		return "", fmt.Errorf("RunSubAgent: no llm client (provider not selected)")
	}
	subConv := conversation.NewManager()
	for _, msg := range seed {
		switch msg.Role {
		case "user":
			subConv.AddUserMessage(msg.Content)
		case "assistant":
			subConv.AddAssistantMessage(msg.Content)
		}
	}
	subConv.AddUserMessage(body)

	subAgent := agent.New(m.client, m.registry, "")
	if m.selectedProvider != nil {
		subAgent.Protocol = m.selectedProvider.Protocol
		subAgent.ContextWindow = m.selectedProvider.GetContextWindow()
		subAgent.MaxOutputTokens = m.selectedProvider.GetMaxOutputTokens()
	}
	subAgent.MaxIterations = 50

	var output strings.Builder
	ch := subAgent.Run(ctx, subConv)
	for ev := range ch {
		switch e := ev.(type) {
		case agent.StreamText:
			output.WriteString(e.Text)
		case agent.ErrorEvent:
			return output.String(), fmt.Errorf("%s", e.Message)
		}
	}
	return output.String(), nil
}

func (m *Model) loadCustomInstructions(wd string) string {
	return memory.LoadInstructions(wd)
}

// installMemoryExtractor 把 ch09 的后台记忆提取接线到给定的 agent 上。用
// 当前 TUI 上下文构造一个 Extractor，并挂接到 ag.OnLoopComplete。返回
// Extractor，使调用方可以把它存到 Model.memoryExtractor 上供稍后 Drain。
func (m *Model) installMemoryExtractor(ag *agent.Agent, wd, protocol string) *extractor.Extractor {
	if m.client == nil || m.conversation == nil {
		return nil
	}
	conv := m.conversation
	extr := extractor.InitExtractMemories(extractor.Deps{
		MemoryDir:     memory.GetAutoMemPath(wd),
		UserMemoryDir: memory.GetUserAutoMemPath(),
		ProjectRoot:   wd,
		Client:        m.client,
		ToolRegistry:  m.registry,
		Protocol:      protocol,
		Conversation:  conv,
		AppendSystem:  func(s string) { conv.AddSystemReminder(s) },
	})

	// 记忆整理器：后台自动合并重复、删除过时、修正矛盾
	consolidator := consolidation.NewConsolidator(consolidation.Deps{
		MemoryDir:     memory.GetAutoMemPath(wd),
		UserMemoryDir: memory.GetUserAutoMemPath(),
		ProjectRoot:   wd,
		Client:        m.client,
		ToolRegistry:  m.registry,
		Protocol:      protocol,
		Conversation:  conv,
		AppendSystem:  func(s string) { conv.AddSystemReminder(s) },
	})
	m.memoryConsolidator = consolidator

	ag.OnLoopComplete = func(_ *conversation.Manager) {
		_ = extr.Execute(context.Background())
		consolidator.MaybeRun(context.Background())
	}
	return extr
}

// prefetchRelevantMemories 在 goroutine 中运行召回选择器，并返回一个
// channel，该 channel 将收到渲染好的 system-reminder 字符串（如果没有选中
// 任何内容 / 选择器超时则收到 ""）。调用方必须使用自己的超时从 channel
// 中恰好读取一次。
//
// 每次调用都会启动一个新的 side-query llm.Client，使选择器的 SYSTEM
// 提示词独立于主对话的系统提示词。
func (m *Model) prefetchRelevantMemories(query string) <-chan string {
	out := make(chan string, 1)
	if m.memoryMgr == nil || m.selectedProvider == nil {
		out <- ""
		return out
	}
	provider := m.selectedProvider
	memDir := m.memoryMgr.Dir()
	userMemDir := m.memoryMgr.UserDir()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		selector := func(ctx context.Context, sys, user string) (string, error) {
			sideClient, err := llm.NewClient(provider, sys)
			if err != nil {
				return "", err
			}
			miniConv := conversation.NewManager()
			miniConv.AddUserMessage(user)
			events, errs := sideClient.Stream(ctx, miniConv, nil)
			var sb strings.Builder
			for ev := range events {
				if td, ok := ev.(llm.TextDelta); ok {
					sb.WriteString(td.Text)
				}
			}
			select {
			case err := <-errs:
				if err != nil {
					return "", err
				}
			default:
			}
			return sb.String(), nil
		}
		results, _ := memory.FindRelevantMemories(ctx, query, userMemDir, memDir, nil, nil, selector)
		out <- renderRelevantMemoriesReminder(results)
	}()
	return out
}

// collectPrefetchedRecall 最多等待 timeout 时长，等待预取 channel 产生渲染好
// 的提醒，然后把它作为 system-reminder 注入到给定对话。如果先触发超时，
// 预取 goroutine 会继续运行但其结果会被丢弃——召回是尽力而为，绝不能
// 阻塞用户的主请求。
func collectPrefetchedRecall(conv *conversation.Manager, prefetchCh <-chan string, timeout time.Duration) {
	if conv == nil || prefetchCh == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case reminder := <-prefetchCh:
		if reminder != "" {
			conv.AddSystemReminder(reminder)
		}
	case <-timer.C:
		// 放弃——选择器仍在后台运行，但结果会被丢弃
	}
}

// renderRelevantMemoriesReminder 把最多 5 个被召回的记忆文件格式化为单个
// system-reminder 正文。每条记忆带一个新鲜度标题（今天 / N 天前）并内联
// 其文件内容。读取失败的文件会被静默跳过。
func renderRelevantMemoriesReminder(memories []memory.RelevantMemory) string {
	if len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("The following relevant memories from prior conversations may help:\n\n")
	for _, mem := range memories {
		data, err := os.ReadFile(mem.Path)
		if err != nil {
			continue
		}
		sb.WriteString(fmt.Sprintf("## Memory: %s (saved %s)\n\n", filepath.Base(mem.Path), memory.MemoryAge(mem.MtimeMs)))
		if note := memory.MemoryFreshnessText(mem.MtimeMs); note != "" {
			sb.WriteString(note)
			sb.WriteString("\n\n")
		}
		sb.Write(data)
		sb.WriteString("\n\n---\n\n")
	}
	return sb.String()
}

func (m *Model) savePartialResponse() {
	if m.streamBuf != "" {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "assistant",
			content: m.streamBuf,
		})
		m.conversation.AddAssistantMessage(m.streamBuf)
		wd, _ := os.Getwd()
		session.SaveMessage(wd, m.sessionID, session.Message{
			Role: "assistant", Content: m.streamBuf, Ts: time.Now().Unix(),
		})
		m.streamBuf = ""
	}
	m.toolBlocks = nil

	msgs := m.conversation.GetMessages()
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if last.Role == "assistant" && len(last.ToolUses) > 0 {
			var results []conversation.ToolResultBlock
			for _, tu := range last.ToolUses {
				results = append(results, conversation.ToolResultBlock{
					ToolUseID: tu.ToolUseID,
					Content:   "Tool execution was interrupted by user.",
					IsError:   true,
				})
			}
			m.conversation.AddToolResultsMessage(results)
		}
	}

	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: "(response interrupted)",
	})
	m.updateViewport()
}

func (m *Model) finishStreaming() {
	if m.thinking {
		m.thinkingDone = time.Since(m.thinkingStart).Seconds()
		m.thinking = false
	}
	m.streaming = false
	m.cancelStream = nil
	m.agentCh = nil
	m.updateViewport()
}

func (m Model) handleProviderSelect(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.providerCursor > 0 {
			m.providerCursor--
		}
	case "down", "j":
		if m.providerCursor < len(m.providers)-1 {
			m.providerCursor++
		}
	case "enter":
		p := &m.providers[m.providerCursor]
		m.selectedProvider = p
		wd, _ := os.Getwd()
		systemPrompt := m.loadSkillsAndBuildPrompt(wd)
		client, err := llm.NewClient(p, systemPrompt)
		if err != nil {
			m.state = stateChat
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: err.Error()})
			return m, nil
		}
		m.client = client
		m.sessionID = session.NewID()
		m.fileHistory = filehistory.New(wd, m.sessionID)
		m.defaultTools.EditFile.FileHistory = m.fileHistory
		m.defaultTools.WriteFile.FileHistory = m.fileHistory
		m.registerAgentTools(client, p, p.Protocol, wd)
		// 尽力而为：从 provider 拉取一次模型的上下文窗口（仅限 Anthropic），
		// 并在 GetContextWindow 读取之前缓存到 p 上。任何失败都会静默降级到
		// 映射表 / 默认值。
		llm.ResolveContextWindow(context.Background(), p)
		ag := agent.New(client, m.registry, p.Protocol)
		ag.ContextWindow = p.GetContextWindow()
		ag.MaxOutputTokens = p.GetMaxOutputTokens()
		ag.Instructions = m.instructionsContent
		ag.MemoryContent = m.memoryContent
		ag.FileHistory = m.fileHistory
		ag.SetSessionID(m.sessionID)
		sandboxAllow := []string{memory.GetAutoMemPath(wd)}
		if userMem := memory.GetUserAutoMemPath(); userMem != "" {
			sandboxAllow = append(sandboxAllow, userMem)
		}
		pathSandbox2 := permissions.NewPathSandbox(wd, sandboxAllow...)
		ag.Checker = permissions.NewChecker(
			pathSandbox2,
			&permissions.RuleEngine{
				LocalPath: filepath.Join(wd, ".cody", "permissions.local.yaml"),
			},
			permissions.ModeDefault,
		)
		// 根据配置文件初始化 OS 级沙箱
		if m.sandboxCfg.Enabled {
			sb := sandbox.New()
			if bashTool, ok := m.registry.Get("Bash").(*tools.BashTool); ok && sb != nil {
				bashTool.Sandbox = sb
				bashTool.SandboxConfig = sandbox.Config{
					AllowWrite:     pathSandbox2.GetAllowedRoots(),
					DenyWrite:      pathSandbox2.GetDenyWrite(),
					NetworkEnabled: m.sandboxCfg.NetworkEnabled,
				}
			}
			if m.sandboxCfg.AutoAllow {
				ag.Checker.SandboxEnabled = true
			}
		}
		ag.NotificationFn = m.drainTaskNotifications
		ag.ToolNameFilter = teams.CoordinatorToolFilter(m.EnableCoordinatorMode)
		ag.CoordinatorActiveFn = teams.CoordinatorActiveFn(m.EnableCoordinatorMode)
		if len(m.hookConfigs) > 0 {
			eng := hooks.NewEngine()
			eng.LoadHooks(m.hookConfigs)
			eng.AgentRunner = newAgentHookRunner(client)
			ag.Hooks = eng
		}
		m.ag = ag
		if at, ok := m.registry.Get("Agent").(*agents.AgentTool); ok {
			at.ParentChecker = ag.Checker
		}
		m.wireSkillsToAgent()
		m.memoryExtractor = m.installMemoryExtractor(ag, wd, p.Protocol)
		m.historyEntries = history.Load(wd)
		// 注意：保持 m.sessionID == 上面接线进 ag（SetSessionID）和
		// m.fileHistory 的那个 id；不要在这里重新生成新 id，否则 compact
		// 边界会落到与 TUI 追加时不同的会话文件中。
		m.state = stateChat
		m.textarea.Focus()
		m.updateViewport()
		return m, m.initMCPServersCmd()
	}
	return m, nil
}

func (m Model) handleChat(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.askUserDialog {
		return m.handleAskUserDialog(msg)
	}

	if m.planApprovalDialog {
		return m.handlePlanApproval(msg)
	}

	if m.rewindDialog {
		return m.handleRewindKeys(msg)
	}

	if m.sandboxDialog {
		return m.handleSandboxDialog(msg)
	}

	// ctrl+o：在所有可折叠块上切换展开/折叠
	if msg.String() == "ctrl+o" {
		toggled := false
		for i := range m.chatMessages {
			r := m.chatMessages[i].role
			if r == "tool_group" || r == "tool_collapsed" || r == "sub_agent" {
				m.chatMessages[i].expanded = !m.chatMessages[i].expanded
				toggled = true
			}
		}
		if toggled {
			m.updateViewport()
		}
		return m, nil
	}

	// 流式传输期间按 ESC：把运行中的子代理转入后台
	if msg.String() == "esc" && m.streaming && m.agentCh != nil && m.cancelStream != nil {
		if m.taskMgr != nil {
			taskID := m.taskMgr.AdoptRunning("manual-background", m.agentCh, m.cancelStream)
			m.chatMessages = append(m.chatMessages, chatMessage{
				role:    "system",
				content: fmt.Sprintf("Agent moved to background (task %s). You will be notified when it completes.", taskID),
			})
			m.agentCh = nil
			m.cancelStream = nil
			m.finishStreaming()
			commitText := m.renderMessagesRange(m.committedUpTo, len(m.chatMessages))
			m.committedUpTo = len(m.chatMessages)
			if commitText != "" {
				return m, tea.Println(commitText)
			}
		}
		return m, nil
	}

	if m.atMenuOpen {
		switch msg.String() {
		case "up":
			if m.atCursor > 0 {
				m.atCursor--
			}
			return m, nil
		case "down":
			if m.atCursor < len(m.atMatches)-1 {
				m.atCursor++
			}
			return m, nil
		case "enter", "tab":
			if m.atCursor < len(m.atMatches) {
				selected := m.atMatches[m.atCursor]
				// 用 @filepath 替换 @prefix
				text := m.textarea.Value()
				atIdx := strings.LastIndex(text, "@")
				if atIdx >= 0 {
					m.textarea.Reset()
					m.textarea.SetHeight(1)
					m.textarea.InsertString(text[:atIdx] + "@" + selected + " ")
				}
				m.atMenuOpen = false
				m.atMatches = nil
				m.atCursor = 0
			}
			return m, nil
		case "esc":
			m.atMenuOpen = false
			m.atMatches = nil
			m.atCursor = 0
			return m, nil
		}
	}

	if m.slashMenuOpen {
		switch msg.String() {
		case "up":
			if m.slashCursor > 0 {
				m.slashCursor--
			}
			return m, nil
		case "down":
			if m.slashCursor < len(m.slashMatches)-1 {
				m.slashCursor++
			}
			return m, nil
		case "enter":
			if m.slashCursor < len(m.slashMatches) {
				selected := m.slashMatches[m.slashCursor]
				m.slashMenuOpen = false
				m.slashMatches = nil
				m.slashCursor = 0
				m.textarea.Reset()
				m.textarea.SetHeight(1)
				return m.executeCommand(selected.Name, "")
			}
			return m, nil
		case "esc":
			m.slashMenuOpen = false
			m.slashMatches = nil
			m.slashCursor = 0
			return m, nil
		case "tab":
			if m.slashCursor < len(m.slashMatches) {
				selected := m.slashMatches[m.slashCursor]
				m.textarea.Reset()
				m.textarea.SetHeight(1)
				m.textarea.InsertString("/" + selected.Name + " ")
				m.slashMenuOpen = false
				m.slashMatches = nil
				m.slashCursor = 0
			}
			return m, nil
		}
	}

	if msg.String() == "shift+tab" {
		if m.ag != nil && m.ag.Checker != nil && !m.streaming {
			m.ag.Checker.Mode = nextPermissionMode(m.ag.Checker.Mode)
			m.updateViewport()
		}
		return m, nil
	}

	if msg.String() == "ctrl+j" {
		m.textarea.InsertString("\n")
		m.resizeTextarea()
		return m, nil
	}

	if msg.String() == "enter" {
		text := strings.TrimSpace(m.textarea.Value())
		if text == "" {
			return m, nil
		}
		if m.streaming {
			if m.cancelStream != nil {
				m.cancelStream()
			}
			m.savePartialResponse()
			m.finishStreaming()
		}
		if strings.HasPrefix(text, "/") {
			name, args := commands.Parse(text)
			m.textarea.Reset()
			m.textarea.SetHeight(1)
			m.slashMenuOpen = false
			m.slashMatches = nil
			m.slashCursor = 0
			return m.executeCommand(name, args)
		}
		m.slashMenuOpen = false
		return m.sendMessage(text)
	}

	switch msg.String() {
	case "pgup", "pgdown", "home", "end":
		var vpCmd tea.Cmd
		m.viewport, vpCmd = m.viewport.Update(msg)
		m.userScrolled = !m.viewport.AtBottom()
		return m, vpCmd
	case "up":
		if !m.streaming && m.textarea.Line() == 0 {
			m.historyUp()
			return m, nil
		}
	case "down":
		if !m.streaming && m.textarea.Line() == m.textarea.LineCount()-1 {
			m.historyDown()
			return m, nil
		}
	}

	prevText := m.textarea.Value()
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	if m.textarea.Value() != prevText {
		m.historyIndex = 0
		m.resizeTextarea()
	}

	m.updateSlashMenu()
	m.updateAtMenu()

	return m, cmd
}

func (m *Model) resizeTextarea() {
	content := m.textarea.Value()
	textWidth := m.textarea.Width()
	if textWidth < 1 {
		textWidth = 1
	}
	total := 0
	for _, line := range strings.Split(content, "\n") {
		w := uniseg.StringWidth(line)
		if w <= textWidth {
			total++
		} else {
			total += (w + textWidth - 1) / textWidth
		}
	}
	maxH := m.height / 2
	if maxH < 1 {
		maxH = 1
	}
	if total > maxH {
		total = maxH
	}
	if total < 1 {
		total = 1
	}
	m.textarea.SetHeight(total)
	m.updateViewport()
}

func (m *Model) updateSlashMenu() {
	text := m.textarea.Value()
	if !strings.HasPrefix(text, "/") || m.historyIndex > 0 {
		m.slashMenuOpen = false
		m.slashMatches = nil
		m.slashCursor = 0
		return
	}

	prefix := strings.TrimPrefix(text, "/")
	if strings.Contains(prefix, " ") {
		m.slashMenuOpen = false
		m.slashMatches = nil
		m.slashCursor = 0
		return
	}

	names := m.cmdRegistry.Complete(prefix)
	var matches []*commands.Command
	for _, name := range names {
		if cmd := m.cmdRegistry.Find(name); cmd != nil {
			seen := false
			for _, existing := range matches {
				if existing.Name == cmd.Name {
					seen = true
					break
				}
			}
			if !seen {
				matches = append(matches, cmd)
			}
		}
	}

	if len(matches) > 8 {
		matches = matches[:8]
	}
	m.slashMatches = matches
	m.slashMenuOpen = len(matches) > 0
	if m.slashCursor >= len(matches) {
		m.slashCursor = 0
	}
}

func (m *Model) updateAtMenu() {
	if m.slashMenuOpen {
		m.atMenuOpen = false
		return
	}

	text := m.textarea.Value()
	atIdx := strings.LastIndex(text, "@")
	if atIdx < 0 {
		m.atMenuOpen = false
		m.atMatches = nil
		m.atCursor = 0
		return
	}

	after := text[atIdx+1:]
	if strings.Contains(after, " ") {
		m.atMenuOpen = false
		m.atMatches = nil
		m.atCursor = 0
		return
	}

	m.atPrefix = after
	matches := scanFiles(after, 8)
	m.atMatches = matches
	m.atMenuOpen = len(matches) > 0
	if m.atCursor >= len(matches) {
		m.atCursor = 0
	}
}

func scanFiles(prefix string, limit int) []string {
	dir := "."
	searchPrefix := prefix

	if strings.Contains(prefix, "/") {
		lastSlash := strings.LastIndex(prefix, "/")
		dir = prefix[:lastSlash]
		if dir == "" {
			dir = "/"
		}
		searchPrefix = prefix[lastSlash+1:]
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	skipDirs := map[string]bool{
		".git": true, "node_modules": true, ".venv": true,
		"__pycache__": true, ".cody": true, "vendor": true,
	}

	var matches []string
	for _, e := range entries {
		if skipDirs[e.Name()] {
			continue
		}
		if strings.HasPrefix(e.Name(), ".") && searchPrefix == "" {
			continue
		}
		if searchPrefix != "" && !strings.HasPrefix(strings.ToLower(e.Name()), strings.ToLower(searchPrefix)) {
			continue
		}

		path := e.Name()
		if dir != "." {
			path = dir + "/" + e.Name()
		}
		if e.IsDir() {
			path += "/"
		}
		matches = append(matches, path)
		if len(matches) >= limit {
			break
		}
	}
	return matches
}

func (m Model) buildCommandContext(args string) *commands.Context {
	wd, _ := os.Getwd()
	modelName := ""
	if m.selectedProvider != nil {
		modelName = m.selectedProvider.Model
	}
	return &commands.Context{
		Args:    args,
		WorkDir: wd,
		Model:   modelName,
		MemoryList: func() []string {
			if m.memoryMgr == nil {
				return nil
			}
			return m.memoryMgr.GetMemories()
		},
		MemoryClear: func() {
			if m.memoryMgr != nil {
				m.memoryMgr.Clear()
			}
		},
		TokenCount: func() (int, int) {
			return m.totalInput, m.totalOutput
		},
		PermissionMode: func() string {
			if m.ag != nil && m.ag.Checker != nil {
				return string(m.ag.Checker.Mode)
			}
			return "default"
		},
		SetPermissionMode: func(mode string) error {
			if m.ag == nil || m.ag.Checker == nil {
				return fmt.Errorf("permission system not initialized")
			}
			target := permissions.PermissionMode(mode)
			switch target {
			case permissions.ModeDefault, permissions.ModeAcceptEdits, permissions.ModePlan, permissions.ModeBypass:
				m.ag.Checker.Mode = target
				return nil
			default:
				return fmt.Errorf("invalid mode: %s (expected: default|acceptEdits|plan|bypassPermissions)", mode)
			}
		},
		ToolCount: func() int {
			return len(m.registry.ListTools())
		},
		SessionInfo: func() string {
			return fmt.Sprintf("Current session: %d messages", len(m.chatMessages))
		},
		SkillList: func() []commands.SkillInfo {
			if m.skillCatalog == nil {
				return nil
			}
			var result []commands.SkillInfo
			for _, meta := range m.skillCatalog.List() {
				result = append(result, commands.SkillInfo{
					Name:        meta.Name,
					Description: meta.Description,
				})
			}
			return result
		},
		SkillReload: func() int {
			if m.skillCatalog == nil {
				return 0
			}
			wd, _ := os.Getwd()
			m.skillCatalog.Reload(wd)
			for _, meta := range m.skillCatalog.List() {
				m.registerSkillCommand(meta.Name)
			}
			if m.client != nil {
				m.client.SetSystemPrompt(m.rebuildSystemPrompt(wd))
			}
			return len(m.skillCatalog.List())
		},
		MCPInfo: func() string {
			return m.mcpServerInfo
		},
	}
}

func (m Model) executeCommand(name, args string) (tea.Model, tea.Cmd) {
	cmd := m.cmdRegistry.Find(name)
	if cmd == nil {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: fmt.Sprintf("Unknown command: /%s — type /help to see available commands", name),
		})
		m.updateViewport()
		return m, nil
	}

	if args == "" && cmd.ArgPrompt != "" {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: cmd.ArgPrompt,
		})
		m.updateViewport()
		return m, nil
	}

	ctx := m.buildCommandContext(args)

	switch cmd.Type {
	case commands.TypeLocalUI:
		switch name {
		case "clear":
			m.chatMessages = nil
			m.committedUpTo = 0
			m.conversation = conversation.NewManager()
			if m.ag != nil {
				m.ag.ClearActiveSkills()
				// Skill 的工具收窄随对话一起清掉，但 coordinator 的约束不清：
				// 它跟着 Team 走，Team 还在就该继续管着 Lead。
				m.ag.SetToolFilter(teams.CoordinatorToolFilter(m.EnableCoordinatorMode))
			}
			// 开启全新会话：重置 session ID 及关联的持久化存储
			wd, _ := os.Getwd()
			m.sessionID = session.NewID()
			m.fileHistory = filehistory.New(wd, m.sessionID)
			m.defaultTools.EditFile.FileHistory = m.fileHistory
			m.defaultTools.WriteFile.FileHistory = m.fileHistory
			if m.ag != nil {
				m.ag.FileHistory = m.fileHistory
				m.ag.SetSessionID(m.sessionID)
			}
			store := todo.NewStore(wd, m.sessionID)
			m.todoList = todo.NewTaskList(store)
			// 重置 token 计数
			m.totalInput = 0
			m.totalOutput = 0
			m.updateViewport()
			return m, tea.Batch(
				func() tea.Msg { return tea.ClearScreen() },
				tea.Println(m.renderBanner()+"\n"),
			)
		case "plan":
			wd, _ := os.Getwd()
			if m.ag != nil && m.ag.Checker != nil {
				m.prePlanMode = m.ag.Checker.Mode
				m.ag.Checker.Mode = permissions.ModePlan
				planPath := planfile.GetOrCreatePlanPath(wd)
				m.ag.Checker.PlanFilePath = planPath
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:    "system",
					content: fmt.Sprintf("Entered Plan mode. Plan file: %s\nExplore the codebase and design your approach.", planPath),
				})

				// 重入检测：如果本次会话曾退出过 Plan Mode 且 plan 文件已存在，注入重入提示
				if m.hasExitedPlanMode && planfile.PlanExists(wd) {
					reentryMsg := prompt.BuildPlanModeReentryReminder(planPath, true)
					if reentryMsg != "" {
						m.chatMessages = append(m.chatMessages, chatMessage{
							role:    "system",
							content: reentryMsg,
						})
					}
					m.hasExitedPlanMode = false
				}
			}
			if args != "" {
				m.updateViewport()
				return m.sendMessage(args)
			}
			m.updateViewport()
			return m, nil
		case "compact":
			if m.client == nil || m.conversation == nil {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role: "error", content: "Compact requires an active provider.",
				})
				m.updateViewport()
				return m, nil
			}
			m.chatMessages = append(m.chatMessages, chatMessage{
				role: "system", content: "Compacting conversation…",
			})
			m.updateViewport()
			client := m.client
			conv := m.conversation
			window := 200000
			if m.selectedProvider != nil {
				window = m.selectedProvider.GetContextWindow()
			}
			var recovery *compact.RecoveryState
			var schemas []map[string]any
			if m.ag != nil {
				recovery = m.ag.RecoveryState
				schemas = m.ag.Registry.GetAllSchemas(m.ag.Protocol)
			}
			compactWD, _ := os.Getwd()
			compactSessionID := m.sessionID
			return m, func() tea.Msg {
				msg, err := compact.ForceCompact(context.Background(), conv, client, compactWD, compactSessionID, window, recovery, schemas)
				return compactDoneMsg{message: msg, err: err}
			}
		case "resume":
			return m.handleResume(args)
		case "rewind":
			return m.handleRewind()
		case "sandbox":
			m.sandboxDialog = true
			m.sandboxCursor = 0
			m.updateViewport()
			return m, nil
		}

	case commands.TypePrompt:
		if cmd.Handler != nil {
			prompt := cmd.Handler(ctx)
			displayText := "/" + name
			if args != "" {
				displayText += " " + args
			}
			m.updateViewport()
			newModel, teaCmd := m.sendPromptCommand(displayText, prompt)
			if strings.HasSuffix(cmd.Description, "[skill]") {
				loadedLine := lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(
					fmt.Sprintf("skill(%s)\nSuccessfully loaded skill", name))
				return newModel, tea.Batch(tea.Println(loadedLine), teaCmd)
			}
			return newModel, teaCmd
		}

	case commands.TypeSkillFork:
		// Fork 模式技能：在隔离的子代理中运行技能，在主聊天中显示进度提示，
		// 并在子代理回报后注入最终的 assistant 文本。在线程外运行，使子代理
		// 思考期间 TUI 保持响应。
		//
		// 两行头部（用户回显 + 「Forking…」提示）通过 tea.Println 提交到终端
		// 滚动缓冲区，方式与 sendMessage 提交其 userLine 相同——否则在子代理
		// 运行期间 viewport 会不断增长，把更早的历史推到可视区域之外。
		displayText := "/" + name
		if args != "" {
			displayText += " " + args
		}
		m.chatMessages = append(m.chatMessages, chatMessage{role: "user", content: displayText})
		userLine := promptStyle.Render("❯ ") + lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(displayText)
		forkNotice := fmt.Sprintf("Forking skill %s in isolated sub-agent…", name)
		m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: forkNotice})
		m.committedUpTo = len(m.chatMessages)
		m.updateViewport()
		skillName := name
		skillArgs := args
		commitText := userLine + "\n" + lipgloss.NewStyle().Foreground(dimText).Render(forkNotice)
		return m, tea.Batch(
			tea.Println(commitText),
			func() tea.Msg {
				skill, err := m.skillCatalog.GetFull(skillName)
				if err != nil && skill == nil {
					return forkSkillDoneMsg{name: skillName, err: err}
				}
				if m.ag != nil {
					m.ag.RecoveryState.RecordSkillInvocation(skill.Meta.Name, skill.PromptBody)
				}
				result, runErr := skills.RunFork(context.Background(), skill, skillArgs, m)
				return forkSkillDoneMsg{name: skillName, result: result, err: runErr}
			},
		)

	case commands.TypeLocal:
		if cmd.Handler != nil {
			output := cmd.Handler(ctx)
			m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: output})
			m.updateViewport()
			return m, nil
		}
	}

	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: fmt.Sprintf("/%s — not yet implemented", name),
	})
	m.updateViewport()
	return m, nil
}

var permOptions = []struct {
	label    string
	response agent.PermissionResponse
}{
	{"Yes", agent.PermAllow},
	{"Yes, and don't ask again for this pattern", agent.PermAllowAlways},
	{"No", agent.PermDeny},
}

func (m Model) handlePermDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up":
		if m.permCursor > 0 {
			m.permCursor--
		}
		return m, nil
	case "down":
		if m.permCursor < len(permOptions)-1 {
			m.permCursor++
		}
		return m, nil
	case "enter":
		m.permRespCh <- permOptions[m.permCursor].response
		m.permDialog = false
		m.permCursor = 0
		m.updateViewport()
		return m, m.listenForAgentEvents()
	case "esc":
		m.permRespCh <- agent.PermDeny
		m.permDialog = false
		m.permCursor = 0
		m.updateViewport()
		return m, m.listenForAgentEvents()
	}
	return m, nil
}

func (m Model) handlePlanApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.planApprovalCursor > 0 {
			m.planApprovalCursor--
		}
		m.updateViewport()
		return m, nil
	case "down", "j":
		if m.planApprovalCursor < 2 {
			m.planApprovalCursor++
		}
		m.updateViewport()
		return m, nil
	case "enter":
		if m.planApprovalCursor == 2 && m.planApprovalInput != "" {
			return m.sendPlanFeedback(m.planApprovalInput, false)
		}
		return m.executePlanApproval()
	case "shift+tab":
		if m.planApprovalCursor == 2 && m.planApprovalInput != "" {
			return m.sendPlanFeedback(m.planApprovalInput, true)
		}
		return m, nil
	case "esc":
		m.planApprovalDialog = false
		m.updateViewport()
		return m, nil
	case "backspace":
		if m.planApprovalCursor == 2 && len(m.planApprovalInput) > 0 {
			_, size := utf8.DecodeLastRuneInString(m.planApprovalInput)
			m.planApprovalInput = m.planApprovalInput[:len(m.planApprovalInput)-size]
			m.updateViewport()
		}
		return m, nil
	default:
		if m.planApprovalCursor == 2 && len(msg.Runes) > 0 {
			m.planApprovalInput += string(msg.Runes)
			m.updateViewport()
		}
		return m, nil
	}
}

func (m Model) executePlanApproval() (tea.Model, tea.Cmd) {
	m.planApprovalDialog = false
	wd, _ := os.Getwd()

	var modeMsg string
	switch m.planApprovalCursor {
	case 0: // YOLO 模式
		if m.ag != nil && m.ag.Checker != nil {
			m.ag.Checker.Mode = permissions.ModeBypass
			m.ag.Checker.PlanFilePath = ""
		}
		modeMsg = "Plan approved. Entered YOLO mode (all operations auto-approved)."
	case 1: // 手动审批
		if m.ag != nil && m.ag.Checker != nil {
			restoreMode := m.prePlanMode
			if restoreMode == "" {
				restoreMode = permissions.ModeDefault
			}
			m.ag.Checker.Mode = restoreMode
			m.ag.Checker.PlanFilePath = ""
		}
		modeMsg = "Plan approved. Each edit will require your confirmation."
	}

	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: modeMsg,
	})

	// 加载计划并作为上下文发送，让 agent 开始执行
	planPath := planfile.GetPlanFilePath(wd)
	planContent, _ := planfile.LoadPlan(wd)
	planExists := planfile.PlanExists(wd)
	planfile.ResetPlanPath()

	executeMsg := prompt.BuildPlanModeExitReminder(planPath, planExists)
	// 标记本次会话已退出过 Plan Mode，后续重入时可注入提示
	m.hasExitedPlanMode = true
	executeMsg += "\n\nUser has approved your plan. You can now start coding."
	if planContent != "" {
		executeMsg += "\n\nApproved Plan:\n" + planContent
	}

	m.updateViewport()
	return m.sendMessage(executeMsg)
}

func (m Model) sendPlanFeedback(feedback string, alsoExit bool) (tea.Model, tea.Cmd) {
	m.planApprovalDialog = false
	m.planApprovalInput = ""

	if alsoExit {
		if m.ag != nil && m.ag.Checker != nil {
			restoreMode := m.prePlanMode
			if restoreMode == "" {
				restoreMode = permissions.ModeDefault
			}
			m.ag.Checker.Mode = restoreMode
			m.ag.Checker.PlanFilePath = ""
		}
		planfile.ResetPlanPath()
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: "Exiting plan mode with feedback. Edits will require confirmation.",
		})
	}

	m.updateViewport()
	return m.sendMessage(feedback)
}

func (m Model) renderPlanApprovalDialog() string {
	var sb strings.Builder

	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(
		" Cody has written up a plan and is ready to execute. Would you like to proceed?",
	)
	sb.WriteString(header)
	sb.WriteString("\n\n")

	options := []string{
		"Yes, enter YOLO mode (auto-approve all)",
		"Yes, manually approve edits",
		"Tell Cody what to change",
	}

	for i, opt := range options {
		prefix := "   "
		if i == m.planApprovalCursor {
			prefix = lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
		}
		label := opt
		if i == m.planApprovalCursor {
			label = lipgloss.NewStyle().Bold(true).Render(opt)
		} else {
			label = lipgloss.NewStyle().Foreground(dimText).Render(opt)
		}
		sb.WriteString(prefix)
		sb.WriteString(fmt.Sprintf("%d. %s", i+1, label))
		sb.WriteString("\n")

		if i == 2 {
			inputLine := m.planApprovalInput
			if m.planApprovalCursor == 2 {
				inputLine += "█"
			}
			if inputLine == "█" || inputLine == "" {
				placeholder := lipgloss.NewStyle().Foreground(dimText).Render("Type feedback here...")
				if m.planApprovalCursor == 2 {
					sb.WriteString("      " + placeholder + "\n")
				}
			} else {
				sb.WriteString("      " + inputLine + "\n")
			}
			hint := lipgloss.NewStyle().Foreground(dimText).Render("      shift+tab to approve with this feedback")
			sb.WriteString(hint)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
	return sb.String()
}

// handleSandboxDialog 处理沙箱模式选择对话框的按键交互
func (m Model) handleSandboxDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	labels := commands.SandboxModeLabels()
	switch msg.String() {
	case "up", "k":
		if m.sandboxCursor > 0 {
			m.sandboxCursor--
		}
		m.updateViewport()
		return m, nil
	case "down", "j":
		if m.sandboxCursor < len(labels)-1 {
			m.sandboxCursor++
		}
		m.updateViewport()
		return m, nil
	case "enter":
		m.sandboxDialog = false
		mode := commands.SandboxMode(m.sandboxCursor)
		return m.applySandboxMode(mode)
	case "esc":
		m.sandboxDialog = false
		m.updateViewport()
		return m, nil
	}
	return m, nil
}

// applySandboxMode 根据选择的模式更新 BashTool 和权限检查器
func (m Model) applySandboxMode(mode commands.SandboxMode) (tea.Model, tea.Cmd) {
	bashTool, _ := m.registry.Get("Bash").(*tools.BashTool)
	labels := commands.SandboxModeLabels()
	descriptions := commands.SandboxModeDescriptions()

	switch mode {
	case commands.SandboxAutoAllow:
		// 启用沙箱 + 自动放行
		sb := sandbox.New()
		if bashTool != nil {
			bashTool.Sandbox = sb
			if m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Sandbox != nil {
				bashTool.SandboxConfig = sandbox.Config{
					AllowWrite:     m.ag.Checker.Sandbox.GetAllowedRoots(),
					DenyWrite:      m.ag.Checker.Sandbox.GetDenyWrite(),
					NetworkEnabled: false,
				}
			}
		}
		if m.ag != nil && m.ag.Checker != nil {
			m.ag.Checker.SandboxEnabled = true
		}
	case commands.SandboxRegular:
		// 启用沙箱但保留常规权限确认
		sb := sandbox.New()
		if bashTool != nil {
			bashTool.Sandbox = sb
			if m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Sandbox != nil {
				bashTool.SandboxConfig = sandbox.Config{
					AllowWrite:     m.ag.Checker.Sandbox.GetAllowedRoots(),
					DenyWrite:      m.ag.Checker.Sandbox.GetDenyWrite(),
					NetworkEnabled: false,
				}
			}
		}
		if m.ag != nil && m.ag.Checker != nil {
			m.ag.Checker.SandboxEnabled = false
		}
	case commands.SandboxOff:
		// 关闭沙箱
		if bashTool != nil {
			bashTool.Sandbox = nil
			bashTool.SandboxConfig = sandbox.Config{}
		}
		if m.ag != nil && m.ag.Checker != nil {
			m.ag.Checker.SandboxEnabled = false
		}
	}

	msg := fmt.Sprintf("沙箱模式已切换：%s\n%s", labels[mode], descriptions[mode])
	m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: msg})
	m.updateViewport()
	return m, nil
}

// renderSandboxDialog 渲染沙箱模式选择界面
func (m Model) renderSandboxDialog() string {
	if !m.sandboxDialog {
		return ""
	}
	var sb strings.Builder

	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(
		" 选择沙箱模式",
	)
	sb.WriteString(header)
	sb.WriteString("\n\n")

	labels := commands.SandboxModeLabels()
	descs := commands.SandboxModeDescriptions()

	for i, label := range labels {
		prefix := "   "
		if i == m.sandboxCursor {
			prefix = lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
		}
		displayLabel := label
		if i == m.sandboxCursor {
			displayLabel = lipgloss.NewStyle().Bold(true).Render(label)
		} else {
			displayLabel = lipgloss.NewStyle().Foreground(dimText).Render(label)
		}
		desc := lipgloss.NewStyle().Foreground(dimText).Render(" — " + descs[i])
		sb.WriteString(fmt.Sprintf("%s%d. %s%s\n", prefix, i+1, displayLabel, desc))
	}
	sb.WriteString("\n")
	return sb.String()
}

func (m Model) handleAskUserDialog(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	multiQuestion := len(m.askUserQuestions) > 1

	// 提交页签的处理
	if m.askUserOnSubmit {
		switch msg.String() {
		case "up", "k":
			if m.askUserSubmitIdx > 0 {
				m.askUserSubmitIdx--
			}
			m.updateViewport()
			return m, nil
		case "down", "j":
			if m.askUserSubmitIdx < 1 {
				m.askUserSubmitIdx++
			}
			m.updateViewport()
			return m, nil
		case "left", "shift+tab":
			m.askUserOnSubmit = false
			m.askUserQIdx = len(m.askUserQuestions) - 1
			m.updateViewport()
			return m, nil
		case "enter":
			if m.askUserSubmitIdx == 0 {
				return m.submitAllAnswers()
			}
			return m.cancelAskUser()
		case "esc":
			return m.cancelAskUser()
		}
		return m, nil
	}

	q := m.askUserQuestions[m.askUserQIdx]
	optCount := len(q.Options) + 1
	cursor := m.askUserCursors[m.askUserQIdx]

	switch msg.String() {
	case "up", "k":
		if cursor > 0 {
			m.askUserCursors[m.askUserQIdx]--
		}
		m.updateViewport()
		return m, nil
	case "down", "j":
		if cursor < optCount-1 {
			m.askUserCursors[m.askUserQIdx]++
		}
		m.updateViewport()
		return m, nil
	case "left", "shift+tab":
		if multiQuestion && m.askUserQIdx > 0 {
			m.askUserQIdx--
			m.updateViewport()
		}
		return m, nil
	case "right", "tab":
		if multiQuestion {
			if m.askUserQIdx < len(m.askUserQuestions)-1 {
				m.askUserQIdx++
			} else {
				m.askUserOnSubmit = true
				m.askUserSubmitIdx = 0
			}
			m.updateViewport()
		}
		return m, nil
	case " ":
		if q.MultiSelect && cursor < len(q.Options) {
			sel := m.askUserSelected[m.askUserQIdx]
			sel[cursor] = !sel[cursor]
			m.updateViewport()
			return m, nil
		}
	case "enter":
		m.saveCurrentAnswer()

		if !multiQuestion && !q.MultiSelect {
			return m.submitAllAnswers()
		}

		if m.askUserQIdx < len(m.askUserQuestions)-1 {
			m.askUserQIdx++
		} else {
			m.askUserOnSubmit = true
			m.askUserSubmitIdx = 0
		}
		m.updateViewport()
		return m, nil
	case "backspace":
		if cursor == len(q.Options) && len(m.askUserOther[m.askUserQIdx]) > 0 {
			s := m.askUserOther[m.askUserQIdx]
			_, size := utf8.DecodeLastRuneInString(s)
			m.askUserOther[m.askUserQIdx] = s[:len(s)-size]
			m.updateViewport()
		}
		return m, nil
	case "esc":
		return m.cancelAskUser()
	default:
		if cursor == len(q.Options) && len(msg.Runes) > 0 {
			m.askUserOther[m.askUserQIdx] += string(msg.Runes)
			m.updateViewport()
		}
		return m, nil
	}
	return m, nil
}

func (m *Model) saveCurrentAnswer() {
	q := m.askUserQuestions[m.askUserQIdx]
	cursor := m.askUserCursors[m.askUserQIdx]

	if cursor == len(q.Options) {
		other := m.askUserOther[m.askUserQIdx]
		if other == "" {
			other = "Other"
		}
		m.askUserAnswered[m.askUserQIdx] = other
	} else if q.MultiSelect {
		var selected []string
		for i, opt := range q.Options {
			if m.askUserSelected[m.askUserQIdx][i] {
				selected = append(selected, opt.Label)
			}
		}
		if len(selected) == 0 {
			selected = append(selected, q.Options[cursor].Label)
		}
		m.askUserAnswered[m.askUserQIdx] = strings.Join(selected, ", ")
	} else {
		m.askUserAnswered[m.askUserQIdx] = q.Options[cursor].Label
	}
}

func (m *Model) collectAskUserAnswers() map[string]string {
	result := make(map[string]string)
	for idx, answer := range m.askUserAnswered {
		if idx < len(m.askUserQuestions) {
			result[m.askUserQuestions[idx].Text] = answer
		}
	}
	return result
}

func (m *Model) submitAllAnswers() (tea.Model, tea.Cmd) {
	m.askUserDialog = false
	m.askUserRespCh <- tools.QuestionResponse{Answers: m.collectAskUserAnswers()}
	m.updateViewport()
	return m, tea.Batch(m.listenForAgentEvents(), m.listenForAskUser())
}

func (m *Model) cancelAskUser() (tea.Model, tea.Cmd) {
	m.askUserDialog = false
	m.askUserRespCh <- tools.QuestionResponse{Answers: map[string]string{"_declined": "true"}}
	m.updateViewport()
	return m, tea.Batch(m.listenForAgentEvents(), m.listenForAskUser())
}

func (m Model) renderAskUserDialog() string {
	if !m.askUserDialog || len(m.askUserQuestions) == 0 {
		return ""
	}
	var sb strings.Builder
	multiQuestion := len(m.askUserQuestions) > 1

	// 导航栏（仅限多问题场景）
	if multiQuestion {
		sb.WriteString(m.renderQuestionNavBar())
		sb.WriteString("\n\n")
	}

	if m.askUserOnSubmit {
		sb.WriteString(m.renderSubmitView())
	} else {
		sb.WriteString(m.renderQuestionView())
	}

	// 底部提示
	if multiQuestion && !m.askUserOnSubmit {
		hint := lipgloss.NewStyle().Foreground(dimText).Render("      ← → navigate questions · enter to confirm")
		sb.WriteString(hint)
		sb.WriteString("\n\n")
	}

	return sb.String()
}

func (m Model) renderQuestionNavBar() string {
	var sb strings.Builder
	activeTab := lipgloss.NewStyle().Background(lipgloss.Color("99")).Foreground(lipgloss.Color("255")).Bold(true).Padding(0, 1)
	inactiveTab := lipgloss.NewStyle().Foreground(lipgloss.Color("250")).Padding(0, 1)
	dimArrow := lipgloss.NewStyle().Foreground(dimText)
	brightArrow := lipgloss.NewStyle().Foreground(brandPurple).Bold(true)

	// 左箭头
	if m.askUserQIdx == 0 && !m.askUserOnSubmit {
		sb.WriteString(dimArrow.Render(" ←"))
	} else {
		sb.WriteString(brightArrow.Render(" ←"))
	}

	// 问题页签
	for i, q := range m.askUserQuestions {
		header := q.Header
		if header == "" {
			header = fmt.Sprintf("Q%d", i+1)
		}
		_, answered := m.askUserAnswered[i]
		check := "☐"
		if answered {
			check = "☑"
		}
		label := header + " " + check

		if !m.askUserOnSubmit && i == m.askUserQIdx {
			sb.WriteString(activeTab.Render(label))
		} else {
			sb.WriteString(inactiveTab.Render(label))
		}
	}

	// 提交页签
	submitLabel := "✓ Submit"
	if m.askUserOnSubmit {
		sb.WriteString(activeTab.Render(submitLabel))
	} else {
		sb.WriteString(inactiveTab.Render(submitLabel))
	}

	// 右箭头
	if m.askUserOnSubmit {
		sb.WriteString(dimArrow.Render(" →"))
	} else {
		sb.WriteString(brightArrow.Render(" →"))
	}

	return sb.String()
}

func (m Model) askUserMaxLines() int {
	maxLines := 0
	for _, q := range m.askUserQuestions {
		lines := 2 + len(q.Options) + 1 // 头部 + 空行 + 选项 + Other
		if q.MultiSelect {
			lines++ // 「空格切换」提示
		}
		if lines > maxLines {
			maxLines = lines
		}
	}
	return maxLines
}

func (m Model) renderQuestionView() string {
	var sb strings.Builder
	q := m.askUserQuestions[m.askUserQIdx]
	cursor := m.askUserCursors[m.askUserQIdx]
	lines := 0

	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(" " + q.Text)
	sb.WriteString(header)
	sb.WriteString("\n\n")
	lines += 2

	for i, opt := range q.Options {
		prefix := "   "
		if i == cursor {
			prefix = lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
		}
		if q.MultiSelect {
			check := "○"
			if m.askUserSelected[m.askUserQIdx][i] {
				check = "●"
			}
			prefix += check + " "
		}
		label := opt.Label
		if i == cursor {
			label = lipgloss.NewStyle().Bold(true).Render(opt.Label)
		}
		desc := lipgloss.NewStyle().Foreground(dimText).Render(" — " + opt.Description)
		sb.WriteString(fmt.Sprintf("%s%s%s\n", prefix, label, desc))
		lines++
	}

	// 「Other」选项
	otherIdx := len(q.Options)
	prefix := "   "
	if cursor == otherIdx {
		prefix = lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
	}
	otherLabel := "Other"
	if cursor == otherIdx {
		otherLabel = lipgloss.NewStyle().Bold(true).Render("Other")
	} else {
		otherLabel = lipgloss.NewStyle().Foreground(dimText).Render("Other")
	}
	sb.WriteString(fmt.Sprintf("%s%s", prefix, otherLabel))
	if cursor == otherIdx {
		input := m.askUserOther[m.askUserQIdx] + "█"
		sb.WriteString(": " + input)
	}
	sb.WriteString("\n")
	lines++

	if q.MultiSelect {
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("      space to toggle, enter to confirm"))
		sb.WriteString("\n")
		lines++
	}

	// 补齐到固定高度，使切换问题不会引起布局跳动
	if len(m.askUserQuestions) > 1 {
		target := m.askUserMaxLines()
		for lines < target {
			sb.WriteString("\n")
			lines++
		}
	}

	return sb.String()
}

func (m Model) renderSubmitView() string {
	var sb strings.Builder
	lines := 0

	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(" Review your answers:")
	sb.WriteString(header)
	sb.WriteString("\n\n")
	lines += 2

	for i, q := range m.askUserQuestions {
		label := q.Header
		if label == "" {
			label = fmt.Sprintf("Q%d", i+1)
		}
		answer, ok := m.askUserAnswered[i]
		if ok {
			sb.WriteString(fmt.Sprintf("   %s: %s\n", label, answer))
		} else {
			dim := lipgloss.NewStyle().Foreground(dimText).Render(fmt.Sprintf("   %s: (not answered)", label))
			sb.WriteString(dim + "\n")
		}
		lines++
	}
	sb.WriteString("\n")
	lines++

	// 提交 / 取消选项
	for i, opt := range []string{"Submit answers", "Cancel"} {
		if i == m.askUserSubmitIdx {
			prefix := lipgloss.NewStyle().Foreground(brandPurple).Render(" ❯ ")
			label := lipgloss.NewStyle().Bold(true).Render(opt)
			sb.WriteString(prefix + label + "\n")
		} else {
			sb.WriteString("   " + opt + "\n")
		}
		lines++
	}

	// 补齐到与问题视图一致的高度
	target := m.askUserMaxLines()
	for lines < target {
		sb.WriteString("\n")
		lines++
	}

	return sb.String()
}

func expandAtRefs(text string) string {
	re := regexp.MustCompile(`@([\w./_-]+)`)
	return re.ReplaceAllStringFunc(text, func(match string) string {
		path := strings.TrimPrefix(match, "@")
		path = strings.TrimSuffix(path, "/")
		data, err := os.ReadFile(path)
		if err != nil {
			return match
		}
		content := string(data)
		if len(content) > 10000 {
			content = content[:10000] + "\n… (truncated)"
		}
		return fmt.Sprintf("[File: %s]\n```\n%s\n```", path, content)
	})
}

func (m Model) sendMessage(text string) (tea.Model, tea.Cmd) {
	m.refreshSkillsIfNeeded()
	wd, _ := os.Getwd()
	history.Append(wd, text)
	m.historyEntries = append(m.historyEntries, text)
	m.historyIndex = 0
	m.historyDraft = ""
	session.SaveMessage(wd, m.sessionID, session.Message{Role: "user", Content: text, Ts: time.Now().Unix()})

	m.streaming = true
	m.thinking = true
	m.thinkingStart = time.Now()
	m.thinkingDone = 0
	m.thinkingVerb = randomVerb()
	m.atMenuOpen = false
	m.atMatches = nil
	m.textarea.Reset()
	m.textarea.SetHeight(1)

	expanded := expandAtRefs(text)

	m.chatMessages = append(m.chatMessages, chatMessage{role: "user", content: text})
	userLine := promptStyle.Render("❯ ") + lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(text)
	m.committedUpTo = len(m.chatMessages)
	m.conversation.AddUserMessage(expanded)

	if m.mcpInstructions != "" && !m.mcpInstructionsOK {
		m.conversation.AddSystemReminder(m.mcpInstructions)
		m.mcpInstructionsOK = true
	}

	prefetchCh := m.prefetchRelevantMemories(expanded)

	m.streamBuf = ""
	m.toolBlocks = nil
	m.userScrolled = false

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelStream = cancel

	conv := m.conversation
	ag := m.ag
	// 非阻塞 memory recall：prefetchCh 传给 agent，工具执行后注入
	ag.MemoryRecallCh = prefetchCh
	startAgentCmd := func() tea.Msg {
		return agentReadyMsg{ch: ag.Run(ctx, conv)}
	}

	m.updateViewport()
	return m, tea.Batch(tea.Println(userLine), startAgentCmd, m.listenForAskUser(), m.listenForSubAgentProgress(), m.spinner.Tick)
}

func (m Model) sendPromptCommand(displayText, prompt string) (tea.Model, tea.Cmd) {
	wd, _ := os.Getwd()
	history.Append(wd, displayText)
	m.historyEntries = append(m.historyEntries, displayText)
	m.historyIndex = 0
	m.historyDraft = ""
	session.SaveMessage(wd, m.sessionID, session.Message{Role: "user", Content: displayText, Ts: time.Now().Unix()})

	m.streaming = true
	m.thinking = true
	m.thinkingStart = time.Now()
	m.thinkingDone = 0
	m.thinkingVerb = randomVerb()
	m.atMenuOpen = false
	m.atMatches = nil
	m.textarea.Reset()
	m.textarea.SetHeight(1)

	m.chatMessages = append(m.chatMessages, chatMessage{role: "user", content: displayText})
	userLine := promptStyle.Render("❯ ") + lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(displayText)
	m.committedUpTo = len(m.chatMessages)
	m.conversation.AddUserMessage(prompt)

	prefetchCh := m.prefetchRelevantMemories(prompt)

	m.streamBuf = ""
	m.toolBlocks = nil
	m.userScrolled = false

	ctx, cancel := context.WithCancel(context.Background())
	m.cancelStream = cancel

	conv := m.conversation
	ag := m.ag
	ag.MemoryRecallCh = prefetchCh
	startAgentCmd := func() tea.Msg {
		return agentReadyMsg{ch: ag.Run(ctx, conv)}
	}

	m.updateViewport()
	return m, tea.Batch(tea.Println(userLine), startAgentCmd, m.listenForAskUser(), m.listenForSubAgentProgress(), m.spinner.Tick)
}

type askUserMsg struct {
	req tools.AskUserRequest
}

type subAgentProgressMsg struct {
	progress agents.SubAgentProgress
}

func (m *Model) drainSubAgentProgress() {
	for {
		select {
		case p := <-m.subAgentProgressCh:
			if p.Done {
				if m.activeSubAgent != nil {
					m.activeSubAgent.done = true
					m.activeSubAgent.toolCount = p.ToolCount
					m.activeSubAgent.totalTime = p.TotalTime
				}
			} else {
				if m.activeSubAgent == nil || m.activeSubAgent.done {
					m.activeSubAgent = &subAgentBlock{
						desc:      p.AgentDesc,
						agentType: p.AgentType,
					}
				}
				m.activeSubAgent.toolUses = append(m.activeSubAgent.toolUses, toolBlockInfo{
					toolName: p.ToolName,
					args:     p.ToolArgs,
					elapsed:  p.Elapsed,
					isError:  p.IsError,
				})
			}
		default:
			return
		}
	}
}

func (m Model) listenForSubAgentProgress() tea.Cmd {
	ch := m.subAgentProgressCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		p := <-ch
		return subAgentProgressMsg{progress: p}
	}
}

func (m Model) listenForAskUser() tea.Cmd {
	ch := m.askUserCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		req := <-ch
		return askUserMsg{req: req}
	}
}

func (m Model) listenForAgentEvents() tea.Cmd {
	ch := m.agentCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return agentDoneMsg{ch: ch}
		}
		return agentEventMsg{event: ev, ch: ch}
	}
}

func (m Model) pollMailbox() tea.Cmd {
	if m.teamMgr == nil {
		return nil
	}
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg {
		return mailboxPollMsg{}
	})
}

func (m Model) handleAgentEvent(ev agent.AgentEvent) (tea.Model, tea.Cmd) {
	switch e := ev.(type) {
	case agent.StreamText:
		m.streamBuf += e.Text
		m.updateViewport()

	case agent.ToolUseEvent:
		if e.Args != nil {
			found := false
			for i := range m.toolBlocks {
				if m.toolBlocks[i].toolName == e.ToolName && m.toolBlocks[i].args == nil {
					m.toolBlocks[i].args = e.Args
					found = true
					break
				}
			}
			if !found {
				m.toolBlocks = append(m.toolBlocks, toolBlockInfo{
					toolName: e.ToolName,
					args:     e.Args,
					loading:  true,
				})
			}
		} else {
			m.toolBlocks = append(m.toolBlocks, toolBlockInfo{
				toolName: e.ToolName,
				loading:  true,
			})
		}
		m.updateViewport()

	case agent.ToolResultEvent:
		var emitted *toolBlockInfo
		for i := range m.toolBlocks {
			if m.toolBlocks[i].toolName == e.ToolName && m.toolBlocks[i].loading {
				m.toolBlocks[i].output = e.Output
				m.toolBlocks[i].isError = e.IsError
				m.toolBlocks[i].elapsed = e.Elapsed.Seconds()
				m.toolBlocks[i].loading = false
				m.toolBlocks[i].collapsed = true
				// Agent 工具延迟到 TurnComplete 再处理，以便合并子代理进度。
				if m.toolBlocks[i].toolName != "Agent" {
					tb := m.toolBlocks[i]
					emitted = &tb
					m.toolBlocks = append(m.toolBlocks[:i], m.toolBlocks[i+1:]...)
				}
				break
			}
		}
		if emitted != nil {
			// 先冲刷待处理的 assistant 文本，使顺序与 Cody 的按块流一致：
			// text → tool result → text → tool result。
			if m.streamBuf != "" {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:    "assistant",
					content: m.streamBuf,
				})
				m.streamBuf = ""
			}
			m.chatMessages = append(m.chatMessages, chatMessage{
				role:      "tool_visible",
				content:   renderToolBlockText(*emitted),
				toolGroup: []toolBlockInfo{*emitted},
			})
			commitText := m.renderMessagesRange(m.committedUpTo, len(m.chatMessages))
			m.committedUpTo = len(m.chatMessages)
			m.updateViewport()
			if commitText != "" {
				return m, tea.Batch(tea.Println(commitText), m.listenForAgentEvents())
			}
		}
		m.updateViewport()

	case agent.TurnComplete:
		if m.streamBuf != "" {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: m.streamBuf})
			m.streamBuf = ""
		}
		// 排空任何缓冲的子代理进度事件
		m.drainSubAgentProgress()
		if len(m.toolBlocks) > 0 {
			var nonAgentTools []toolBlockInfo
			for _, tb := range m.toolBlocks {
				if tb.toolName == "Agent" && m.activeSubAgent != nil && len(m.activeSubAgent.toolUses) > 0 {
					// 使用收集到的进度收尾子代理块
					sab := *m.activeSubAgent
					if !sab.done {
						sab.done = true
						sab.toolCount = len(sab.toolUses)
						var total float64
						for _, tu := range sab.toolUses {
							total += tu.elapsed
						}
						sab.totalTime = total
					}
					m.chatMessages = append(m.chatMessages, chatMessage{
						role:          "sub_agent",
						subAgentBlock: &sab,
						expanded:      false,
					})
					m.activeSubAgent = nil
				} else if tb.toolName == "Agent" {
					// Agent 工具运行了但未收集到进度——从结果中提取信息
					desc := ""
					if d, ok := tb.args["description"].(string); ok {
						desc = d
					}
					agentType := "general-purpose"
					if at, ok := tb.args["subagent_type"].(string); ok {
						agentType = at
					}
					sab := &subAgentBlock{
						desc:      desc,
						agentType: agentType,
						done:      true,
						totalTime: tb.elapsed,
						toolCount: 0,
					}
					m.chatMessages = append(m.chatMessages, chatMessage{
						role:          "sub_agent",
						subAgentBlock: sab,
						expanded:      false,
					})
					m.activeSubAgent = nil
				} else {
					nonAgentTools = append(nonAgentTools, tb)
				}
			}
			// 对非 Agent 工具分类：可见（写/命令）与折叠（读）
			var visibleTools, collapsedTools []toolBlockInfo
			for _, tb := range nonAgentTools {
				if isCollapsibleTool(tb.toolName) {
					collapsedTools = append(collapsedTools, tb)
				} else {
					visibleTools = append(visibleTools, tb)
				}
			}
			// 可见工具：每个单独成行展示
			for _, tb := range visibleTools {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:      "tool_visible",
					content:   renderToolBlockText(tb),
					toolGroup: []toolBlockInfo{tb},
				})
			}
			// 折叠的读操作：默认隐藏，按 ctrl+o 显示
			if len(collapsedTools) > 0 {
				m.chatMessages = append(m.chatMessages, chatMessage{
					role:      "tool_collapsed",
					toolGroup: collapsedTools,
					expanded:  false,
				})
			}
		}
		m.toolBlocks = nil
		m.activeSubAgent = nil
		m.updateViewport()

	case agent.UsageEvent:
		m.totalInput = e.InputTokens
		m.totalOutput = e.OutputTokens

	case agent.PermissionRequestEvent:
		m.permDialog = true
		m.permCursor = 0
		m.permToolName = e.ToolName
		m.permDesc = e.Desc
		m.permRespCh = e.ResponseCh
		m.updateViewport()
		return m, nil

	case agent.CompactEvent:
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: "⟳ " + e.Message,
		})
		m.updateViewport()

	case agent.RetryEvent:
		msg := "↻ Retrying: " + e.Reason
		if e.Wait > 0 {
			msg += fmt.Sprintf(" (waiting %s)", e.Wait)
		}
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "system",
			content: msg,
		})
		m.updateViewport()

	case agent.ErrorEvent:
		// 保留错误前已输出的流式文本
		if m.streamBuf != "" {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: m.streamBuf})
			m.streamBuf = ""
		}
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "error",
			content: e.Message,
		})
		commitText := m.renderMessagesRange(m.committedUpTo, len(m.chatMessages))
		m.committedUpTo = len(m.chatMessages)
		m.finishStreaming()
		if commitText != "" {
			return m, tea.Println(commitText)
		}
		return m, nil

	case agent.LoopComplete:
		totalTime := time.Since(m.thinkingStart).Seconds()
		if m.streamBuf != "" {
			// 助手消息由主循环在进入对话历史时落盘，这里只负责上屏
			m.chatMessages = append(m.chatMessages, chatMessage{role: "assistant", content: m.streamBuf})
			m.streamBuf = ""
		}
		m.chatMessages = append(m.chatMessages, chatMessage{
			role:    "thinking",
			content: fmt.Sprintf("✻ %s for %.1fs", m.pastTense(m.thinkingVerb), totalTime),
		})
		commitText := m.renderMessagesRange(m.committedUpTo, len(m.chatMessages))
		m.committedUpTo = len(m.chatMessages)
		m.thinkingDone = 0
		m.finishStreaming()
		if m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Mode == permissions.ModePlan {
			m.planApprovalDialog = true
			m.planApprovalCursor = 0
			m.planApprovalInput = ""
			m.updateViewport()
		}
		pollCmd := m.pollMailbox()
		if commitText != "" {
			return m, tea.Batch(tea.Println(commitText), pollCmd)
		}
		if pollCmd != nil {
			return m, pollCmd
		}
		return m, nil
	}

	return m, m.listenForAgentEvents()
}

func renderToolBlockText(tb toolBlockInfo) string {
	title := toolTitle(tb.toolName, tb.args)
	if tb.isError {
		return fmt.Sprintf("✗ %s (%.1fs)", title, tb.elapsed)
	}
	return fmt.Sprintf("✓ %s (%.1fs)", title, tb.elapsed)
}

func renderSubAgentBlock(sab *subAgentBlock, expanded bool) string {
	var sb strings.Builder

	agentLabel := strings.Title(sab.agentType)
	if agentLabel == "" {
		agentLabel = "Agent"
	}
	header := lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render(
		fmt.Sprintf("● %s(%s)", agentLabel, sab.desc))
	sb.WriteString(header)
	sb.WriteString("\n")

	if sab.done {
		if expanded {
			for _, tu := range sab.toolUses {
				title := toolTitle(tu.toolName, tu.args)
				line := fmt.Sprintf("     %s (%.1fs)", title, tu.elapsed)
				if tu.isError {
					sb.WriteString(toolErrorStyle.Render(line))
				} else {
					sb.WriteString(toolDoneStyle.Render(line))
				}
				sb.WriteString("\n")
			}
		} else {
			summary := fmt.Sprintf("  ⎿  Done (%d tool uses · %.1fs)", sab.toolCount, sab.totalTime)
			sb.WriteString(toolDoneStyle.Render(summary))
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("  (ctrl+o to expand)"))
			sb.WriteString("\n")
		}
	} else {
		n := len(sab.toolUses)
		if n > 0 {
			last := sab.toolUses[n-1]
			lastTitle := toolTitle(last.toolName, last.args)
			sb.WriteString(toolDoneStyle.Render(fmt.Sprintf("  ⎿  %s (%.1fs)", lastTitle, last.elapsed)))
			sb.WriteString("\n")
		}
		if n > 1 {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render(
				fmt.Sprintf("     … +%d tool uses (ctrl+o to expand)", n-1)))
			sb.WriteString("\n")
		}
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("     Running…"))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

func isCollapsibleTool(name string) bool {
	switch name {
	case "ReadFile", "Glob", "Grep", "ToolSearch":
		return true
	}
	return false
}

// isDiffTool 判断该工具的 output 是不是 BuildDiff 生成的带行号 diff 文本。
func isDiffTool(name string) bool {
	return name == "EditFile"
}

// renderDiffLines 把 tools.BuildDiff() 产出的带行号 diff 文本渲染成彩色行：
// "+ " 开头绿色、"- " 开头红色，其余（上下文行/摘要行）走 toolDetailStyle。
func renderDiffLines(output string) string {
	lines := strings.Split(output, "\n")
	rendered := make([]string, len(lines))
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "+ "):
			rendered[i] = diffAddStyle.Render(line)
		case strings.HasPrefix(line, "- "):
			rendered[i] = diffRemoveStyle.Render(line)
		default:
			rendered[i] = toolDetailStyle.Render(line)
		}
	}
	return strings.Join(rendered, "\n")
}

// appendEditDiff 在已渲染好的工具标题行后面追加 EditFile 的 diff 正文（如果有）。
func appendEditDiff(sb *strings.Builder, toolGroup []toolBlockInfo) {
	if len(toolGroup) != 1 {
		return
	}
	tb := toolGroup[0]
	if !isDiffTool(tb.toolName) || tb.output == "" {
		return
	}
	sb.WriteString(renderDiffLines(tb.output))
	sb.WriteString("\n")
}

func renderToolGroupSummary(tools []toolBlockInfo) string {
	var totalElapsed float64
	errors := 0
	for _, tb := range tools {
		totalElapsed += tb.elapsed
		if tb.isError {
			errors++
		}
	}
	n := len(tools)
	if errors > 0 {
		return fmt.Sprintf("● Done (%d tool uses · %d errors · %.1fs)", n, errors, totalElapsed)
	}
	return fmt.Sprintf("● Done (%d tool uses · %.1fs)", n, totalElapsed)
}

func (m *Model) calcViewportHeight(availableHeight int) int {
	contentLines := m.viewport.TotalLineCount()
	if contentLines < 1 {
		contentLines = 1
	}
	if contentLines > availableHeight {
		return availableHeight
	}
	return contentLines
}

func (m *Model) updateViewport() {
	if !m.ready {
		return
	}
	content := m.renderChatContent()
	m.viewport.SetContent(content)

	statusHeight, sepHeight := 1, 1
	inputHeight := m.textarea.Height() + 1
	available := m.height - statusHeight - sepHeight - inputHeight - 1
	if available < 1 {
		available = 1
	}
	m.viewport.Height = m.calcViewportHeight(available)

	if !m.userScrolled {
		m.viewport.GotoBottom()
	}
}

// ── 视图 ──

func (m Model) View() string {
	if !m.ready {
		return ""
	}

	switch m.state {
	case stateProviderSelect:
		return m.renderProviderSelectView()
	case stateChat:
		return m.renderChatView()
	case stateResume:
		return m.renderResumeView()
	}
	return ""
}

func (m Model) renderProviderSelectView() string {
	var sb strings.Builder

	sb.WriteString(m.renderBanner())
	sb.WriteString("\n\n")

	sb.WriteString(selectLabelStyle.Render("Select a Provider"))
	sb.WriteString("\n\n")

	for i, p := range m.providers {
		if i == m.providerCursor {
			sb.WriteString(selectedItemStyle.Render(fmt.Sprintf("  ❯ %s  [%s]", p.Name, p.Model)))
		} else {
			sb.WriteString(normalItemStyle.Render(fmt.Sprintf("    %s  [%s]", p.Name, p.Model)))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m Model) renderTeammateTree() string {
	if m.teamMgr == nil {
		return ""
	}
	progressList := m.teamMgr.GetAllTeammateProgress()
	if len(progressList) == 0 {
		return ""
	}

	cyanStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#00d7ff"))
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#00ff00"))
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#ff0000"))
	yellowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#ffff00"))

	var sb strings.Builder
	sb.WriteString("\n")

	// 队长行
	sb.WriteString("  ┌─ ")
	sb.WriteString(cyanStyle.Render("team-lead"))
	sb.WriteString(": ")
	sb.WriteString(dimStyle.Render(m.thinkingVerb + "…"))
	if m.totalInput+m.totalOutput > 0 {
		sb.WriteString(dimStyle.Render(fmt.Sprintf(" · %s tokens", teams.FormatTokens(int64(m.totalInput+m.totalOutput)))))
	}
	sb.WriteString("\n")

	// 队友行
	for i, p := range progressList {
		isLast := i == len(progressList)-1
		connector := "  ├─ "
		if isLast {
			connector = "  └─ "
		}

		sb.WriteString(connector)
		sb.WriteString(cyanStyle.Render("@" + p.Name))
		sb.WriteString(": ")

		status := p.GetStatus()
		switch status {
		case "completed":
			sb.WriteString(greenStyle.Render("completed"))
		case "failed":
			sb.WriteString(redStyle.Render("failed"))
		case "stopped":
			sb.WriteString(yellowStyle.Render("stopped"))
		case "idle":
			sb.WriteString(dimStyle.Render("idle"))
		default:
			sb.WriteString(dimStyle.Render(p.ActivitySummary() + "..."))
		}

		stats := fmt.Sprintf(" · %d tools · %s tokens", p.GetToolUseCount(), teams.FormatTokens(p.GetTokenCount()))
		sb.WriteString(dimStyle.Render(stats))
		sb.WriteString("\n")
	}

	return sb.String()
}

func (m Model) renderChatView() string {
	var sb strings.Builder

	hasActiveContent := len(m.chatMessages) > m.committedUpTo || m.streaming

	if hasActiveContent {
		bottomLines := 4
		if m.permDialog {
			bottomLines += 3
		}
		vpH := m.height - bottomLines
		if vpH < 1 {
			vpH = 1
		}
		m.viewport.Height = m.calcViewportHeight(vpH)
		sb.WriteString(m.viewport.View())
		sb.WriteString("\n")
	}

	if m.permDialog {
		sb.WriteString(m.renderPermDialog())
	}
	if m.planApprovalDialog {
		sb.WriteString(m.renderPlanApprovalDialog())
	}
	if m.askUserDialog {
		sb.WriteString(m.renderAskUserDialog())
	}
	if m.rewindDialog {
		sb.WriteString(m.renderRewindDialog())
	}
	if m.sandboxDialog {
		sb.WriteString(m.renderSandboxDialog())
	}
	sb.WriteString(m.renderSeparator())
	sb.WriteString("\n")
	if m.planApprovalDialog {
		// 计划审批对话框激活时隐藏输入
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("  Select an option above..."))
	} else {
		sb.WriteString(promptStyle.Render("❯ "))
		sb.WriteString(m.textarea.View())
	}
	sb.WriteString("\n")
	sb.WriteString(m.renderSeparator())
	sb.WriteString("\n")
	if m.slashMenuOpen && len(m.slashMatches) > 0 {
		sb.WriteString(m.renderSlashMenu())
	}
	if m.atMenuOpen && len(m.atMatches) > 0 {
		sb.WriteString(m.renderAtMenu())
	}
	sb.WriteString(m.renderStatusBar())
	return sb.String()
}

func (m Model) renderBanner() string {
	cat := bannerStyle.Render(`   /\_/\   `) + bannerDimStyle.Render("Cody v0.1.0") + "\n" +
		   bannerStyle.Render(`  (=^.^=)  `) + bannerDimStyle.Render(m.getModelName()) + "\n" +
		   bannerStyle.Render(` ( > ^ < ) `) + bannerDimStyle.Render(m.getWorkDir())
	return cat
}

func (m Model) renderSeparator() string {
	line := strings.Repeat("─", m.width)
	return separatorStyle.Render(line)
}

func (m Model) renderStatusBar() string {
	left := "  default"
	if m.ag != nil && m.ag.Checker != nil && m.ag.Checker.Mode != permissions.ModeDefault {
		name, _ := permissionModeInfo(m.ag.Checker.Mode)
		var modeColor lipgloss.Color
		switch m.ag.Checker.Mode {
		case permissions.ModeAcceptEdits:
			modeColor = greenText
		case permissions.ModePlan:
			modeColor = yellowText
		case permissions.ModeBypass:
			modeColor = redText
		default:
			modeColor = dimText
		}
		modeStr := lipgloss.NewStyle().Foreground(modeColor).Render(
			fmt.Sprintf("%s on", name),
		)
		hint := lipgloss.NewStyle().Foreground(dimText).Render(" (shift+tab to cycle)")
		left = statusBarStyle.Render("  ") + modeStr + hint
	} else {
		left = statusBarStyle.Render(left)
	}

	right := ""
	if m.teamMgr != nil {
		activeCount := 0
		for _, p := range m.teamMgr.GetAllTeammateProgress() {
			if p.GetStatus() == "running" {
				activeCount++
			}
		}
		if activeCount > 0 {
			label := fmt.Sprintf("● %d teammate", activeCount)
			if activeCount > 1 {
				label += "s"
			}
			right += lipgloss.NewStyle().Foreground(cyanText).Render(label + " ")
		}
	}
	if m.mcpConnecting {
		right += lipgloss.NewStyle().Foreground(yellowText).Render("MCP connecting… ")
	}
	if m.selectedProvider != nil {
		right += statusItemStyle.Render(m.selectedProvider.Model)
	}

	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 0 {
		gap = 0
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m Model) DumpHistory() string {
	if len(m.chatMessages) == 0 {
		return ""
	}
	var sb strings.Builder

	sb.WriteString(m.renderBanner())
	sb.WriteString("\n\n")

	for _, msg := range m.chatMessages {
		switch msg.role {
		case "user":
			sb.WriteString(promptStyle.Render("❯ "))
			sb.WriteString(lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(msg.content))
			sb.WriteString("\n\n")

		case "assistant":
			sb.WriteString(aiMarkerStyle.Render("● "))
			rendered := m.renderMarkdown(msg.content)
			indented := indentBlock(rendered, "  ")
			sb.WriteString(strings.TrimLeft(indented, " "))
			sb.WriteString("\n\n")

		case "tool", "tool_visible":
			sb.WriteString("  ")
			if strings.HasPrefix(msg.content, "✗") {
				sb.WriteString(toolErrorStyle.Render(msg.content))
			} else {
				sb.WriteString(toolDoneStyle.Render(msg.content))
			}
			sb.WriteString("\n")
			appendEditDiff(&sb, msg.toolGroup)

		case "sub_agent":
			if msg.subAgentBlock != nil {
				sb.WriteString(renderSubAgentBlock(msg.subAgentBlock, false))
			}

		case "system":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")

		case "error":
			sb.WriteString(errorStyle.Render("✖ " + msg.content))
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

func (m Model) renderChatContent() string {
	var sb strings.Builder

	for _, msg := range m.chatMessages[m.committedUpTo:] {
		switch msg.role {
		case "user":
			sb.WriteString(promptStyle.Render("❯ "))
			sb.WriteString(lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(msg.content))
			sb.WriteString("\n\n")

		case "assistant":
			sb.WriteString(aiMarkerStyle.Render("● "))
			rendered := m.renderMarkdown(msg.content)
			indented := indentBlock(rendered, "  ")
			sb.WriteString(strings.TrimLeft(indented, " "))
			sb.WriteString("\n\n")

		case "tool", "tool_visible":
			sb.WriteString("  ")
			if strings.HasPrefix(msg.content, "✗") {
				sb.WriteString(toolErrorStyle.Render(msg.content))
			} else {
				sb.WriteString(toolDoneStyle.Render(msg.content))
			}
			sb.WriteString("\n")
			appendEditDiff(&sb, msg.toolGroup)

		case "tool_collapsed":
			if msg.expanded {
				for _, tb := range msg.toolGroup {
					sb.WriteString("  ")
					text := renderToolBlockText(tb)
					if tb.isError {
						sb.WriteString(toolErrorStyle.Render(text))
					} else {
						sb.WriteString(toolDoneStyle.Render(text))
					}
					sb.WriteString("\n")
				}
			}
			// 折叠时隐藏——不输出

		case "tool_group":
			if msg.expanded {
				for _, tb := range msg.toolGroup {
					sb.WriteString("  ")
					text := renderToolBlockText(tb)
					if tb.isError {
						sb.WriteString(toolErrorStyle.Render(text))
					} else {
						sb.WriteString(toolDoneStyle.Render(text))
					}
					sb.WriteString("\n")
				}
			} else {
				summary := renderToolGroupSummary(msg.toolGroup)
				sb.WriteString(toolDoneStyle.Render("  " + summary))
				sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("  (ctrl+o to expand)"))
				sb.WriteString("\n")
			}

		case "sub_agent":
			if msg.subAgentBlock != nil {
				sb.WriteString(renderSubAgentBlock(msg.subAgentBlock, msg.expanded))
			}

		case "thinking":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")

		case "system":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")

		case "error":
			sb.WriteString(errorStyle.Render("✖ " + msg.content))
			sb.WriteString("\n\n")
		}
	}

	// 活动子代理进度（实时）
	if m.activeSubAgent != nil && !m.activeSubAgent.done {
		sb.WriteString(renderSubAgentBlock(m.activeSubAgent, false))
	}

	// 活动工具块
	for _, tb := range m.toolBlocks {
		if tb.toolName == "Agent" && m.activeSubAgent != nil {
			continue // 上面已作为子代理块渲染
		}
		sb.WriteString(m.renderToolBlock(tb))
		sb.WriteString("\n")
	}

	// 流式文本
	if m.streaming && m.streamBuf != "" {
		sb.WriteString(aiMarkerStyle.Render("● "))
		indented := indentBlock(m.streamBuf, "  ")
		sb.WriteString(streamingTextStyle.Render(strings.TrimLeft(indented, " ")))
		sb.WriteString("\n")
	}

	// 加载动画——agent 运行期间始终排在最后
	if m.streaming {
		elapsed := time.Since(m.thinkingStart).Seconds()
		sb.WriteString("\n")
		sb.WriteString(lipgloss.NewStyle().Foreground(brandPurple).Render(
			fmt.Sprintf("  %s %s…  (%.0fs)", m.spinner.View(), m.thinkingVerb, elapsed),
		))
		sb.WriteString("\n")
		// 队友进度树
		sb.WriteString(m.renderTeammateTree())
	}

	// 即使不在流式传输中也显示队友树（队友可能仍在运行）
	if !m.streaming {
		tree := m.renderTeammateTree()
		if tree != "" {
			sb.WriteString(tree)
		}
	}

	return sb.String()
}

func (m Model) renderMessagesRange(from, to int) string {
	var sb strings.Builder
	for i := from; i < to && i < len(m.chatMessages); i++ {
		msg := m.chatMessages[i]
		switch msg.role {
		case "user":
			sb.WriteString(promptStyle.Render("❯ "))
			sb.WriteString(lipgloss.NewStyle().Foreground(brightText).Bold(true).Render(msg.content))
			sb.WriteString("\n\n")
		case "assistant":
			sb.WriteString(aiMarkerStyle.Render("● "))
			rendered := m.renderMarkdown(msg.content)
			indented := indentBlock(rendered, "    ")
			sb.WriteString(strings.TrimLeft(indented, " "))
			sb.WriteString("\n\n")
		case "tool", "tool_visible":
			sb.WriteString("  ")
			if strings.HasPrefix(msg.content, "✗") {
				sb.WriteString(toolErrorStyle.Render(msg.content))
			} else {
				sb.WriteString(toolDoneStyle.Render(msg.content))
			}
			sb.WriteString("\n")
			appendEditDiff(&sb, msg.toolGroup)
		case "tool_collapsed":
			// 滚动缓冲区之后无法重新展开，因此始终把每个工具内联渲染（带名称 +
			// 参数），而不是折叠整组。
			for _, tb := range msg.toolGroup {
				sb.WriteString("  ")
				text := renderToolBlockText(tb)
				if tb.isError {
					sb.WriteString(toolErrorStyle.Render(text))
				} else {
					sb.WriteString(toolDoneStyle.Render(text))
				}
				sb.WriteString("\n")
			}
		case "tool_group":
			summary := renderToolGroupSummary(msg.toolGroup)
			sb.WriteString(toolDoneStyle.Render("  " + summary))
			sb.WriteString("\n")
		case "sub_agent":
			if msg.subAgentBlock != nil {
				sb.WriteString(renderSubAgentBlock(msg.subAgentBlock, false))
			}
		case "thinking":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")
		case "system":
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(msg.content))
			sb.WriteString("\n\n")
		case "error":
			sb.WriteString(errorStyle.Render("✖ " + msg.content))
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func (m Model) renderToolBlock(tb toolBlockInfo) string {
	title := toolTitle(tb.toolName, tb.args)

	if tb.loading {
		return toolRunningStyle.Render(fmt.Sprintf("● %s …", title))
	}

	if tb.isError {
		return toolErrorStyle.Render(fmt.Sprintf("✗ %s — error (%.1fs)", title, tb.elapsed))
	}

	return toolDoneStyle.Render(fmt.Sprintf("✓ %s (%.1fs)", title, tb.elapsed))
}

var menuActiveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("141"))

func (m Model) renderSlashMenu() string {
	var sb strings.Builder
	for i, cmd := range m.slashMatches {
		desc := cmd.Description
		if len([]rune(desc)) > 30 {
			desc = string([]rune(desc)[:28]) + "…"
		}
		label := fmt.Sprintf("  /%-16s — %s", cmd.Name, desc)
		if i == m.slashCursor {
			sb.WriteString(menuActiveStyle.Render(label) + "\n")
		} else {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render(label) + "\n")
		}
	}
	return sb.String()
}

func (m Model) renderAtMenu() string {
	var sb strings.Builder
	for i, path := range m.atMatches {
		if i == m.atCursor {
			sb.WriteString(menuActiveStyle.Render("  "+path) + "\n")
		} else {
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).Render("  "+path) + "\n")
		}
	}
	return sb.String()
}

func (m Model) renderPermDialog() string {
	var sb strings.Builder

	// 命令头部
	sb.WriteString(permBorderStyle.Render(fmt.Sprintf("  %s command", m.permToolName)))
	sb.WriteString("\n\n")

	// 命令详情
	desc := m.permDesc
	if desc != "" {
		sb.WriteString(lipgloss.NewStyle().Foreground(normalText).PaddingLeft(4).Render(desc))
		sb.WriteString("\n\n")
	}

	// 审批提示
	sb.WriteString(permDimStyle.Render("  This command requires approval"))
	sb.WriteString("\n\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(normalText).PaddingLeft(2).Render("Do you want to proceed?"))
	sb.WriteString("\n")

	// 可选项
	for i, opt := range permOptions {
		prefix := "   "
		style := lipgloss.NewStyle().Foreground(dimText)
		if i == m.permCursor {
			prefix = " ❯ "
			style = lipgloss.NewStyle().Foreground(cyanText)
		}
		label := fmt.Sprintf("%d. %s", i+1, opt.label)
		sb.WriteString(style.Render(prefix+label) + "\n")
	}

	sb.WriteString("\n")
	return sb.String()
}

func (m Model) renderRewindDialog() string {
	if m.rewindPhase == 1 {
		return m.renderRewindOptionsDialog()
	}
	return m.renderRewindSnapshotList()
}

func (m Model) renderRewindSnapshotList() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render("  ⟲ Rewind to checkpoint"))
	sb.WriteString("\n\n")

	maxVisible := 8
	start := 0
	if m.rewindCursor >= maxVisible {
		start = m.rewindCursor - maxVisible + 1
	}
	end := start + maxVisible
	if end > len(m.rewindSnapshots) {
		end = len(m.rewindSnapshots)
	}

	for i := start; i < end; i++ {
		snap := m.rewindSnapshots[i]
		prefix := "   "
		style := lipgloss.NewStyle().Foreground(dimText)
		if i == m.rewindCursor {
			prefix = " ❯ "
			style = lipgloss.NewStyle().Foreground(cyanText)
		}
		ago := time.Since(snap.Timestamp).Truncate(time.Second)
		label := snap.UserText
		if len(label) > 50 {
			label = label[:50] + "…"
		}
		files := len(snap.Backups)
		line := fmt.Sprintf("%s[%d] %s (%s ago, %d file(s))", prefix, i+1, label, ago, files)
		sb.WriteString(style.Render(line) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render("↑/↓ navigate · enter select · esc cancel"))
	sb.WriteString("\n")
	return sb.String()
}

func (m Model) renderRewindOptionsDialog() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(brandPurple).Bold(true).Render("  ⟲ Rewind to checkpoint"))
	sb.WriteString("\n\n")

	snap := m.rewindSnapshots[m.rewindCursor]
	ago := time.Since(snap.Timestamp).Truncate(time.Second)
	label := snap.UserText
	if len(label) > 50 {
		label = label[:50] + "…"
	}
	selected := fmt.Sprintf("  Selected: [%d] %s (%s ago, %d file(s))", m.rewindCursor+1, label, ago, len(snap.Backups))
	sb.WriteString(lipgloss.NewStyle().Foreground(normalText).Render(selected))
	sb.WriteString("\n\n")

	for i, opt := range rewindOptions {
		prefix := "   "
		style := lipgloss.NewStyle().Foreground(dimText)
		if i == m.rewindOptionCursor {
			prefix = " ❯ "
			style = lipgloss.NewStyle().Foreground(cyanText)
		}
		sb.WriteString(style.Render(prefix+opt) + "\n")
	}

	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render("↑/↓ navigate · enter select · esc back"))
	sb.WriteString("\n")
	return sb.String()
}

func (m Model) renderMarkdown(content string) string {
	// 不要使用 WithAutoStyle——它每次都会通过 OSC 11 查询终端背景，而响应会
	// 泄漏进 stdin 并污染输入。显式强制 TrueColor：如果不指定 profile，glamour
	// 会委托给 termenv 自动检测，这在 bubbletea 接管 stdin 时会失败，并回退到
	// 无颜色的 "notty" 样式——markdown 随后会以原始文本打印。
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithColorProfile(termenv.TrueColor),
		glamour.WithWordWrap(m.width-6),
	)
	if err != nil {
		return content
	}
	rendered, err := r.Render(content)
	if err != nil {
		return content
	}
	return strings.TrimSpace(rendered)
}

// ── 辅助函数 ──

func toolTitle(toolName string, args map[string]any) string {
	switch toolName {
	case "ReadFile":
		p, _ := args["file_path"].(string)
		if p != "" {
			return "Read " + filepath.Base(p)
		}
		return "Read"
	case "WriteFile":
		p, _ := args["file_path"].(string)
		content, _ := args["content"].(string)
		lines := strings.Count(content, "\n") + 1
		if p != "" {
			return fmt.Sprintf("Write %s (%d lines)", filepath.Base(p), lines)
		}
		return "Write"
	case "EditFile":
		p, _ := args["file_path"].(string)
		if p != "" {
			return "Edit " + filepath.Base(p)
		}
		return "Edit"
	case "Bash":
		cmd, _ := args["command"].(string)
		if len(cmd) > 50 {
			cmd = cmd[:50] + "…"
		}
		if cmd != "" {
			return fmt.Sprintf("Bash: %s", cmd)
		}
		return "Bash"
	case "Glob":
		pattern, _ := args["pattern"].(string)
		return fmt.Sprintf("Glob: %s", pattern)
	case "Grep":
		pattern, _ := args["pattern"].(string)
		return fmt.Sprintf("Grep: %s", pattern)
	}
	return toolName
}

func indentBlock(text string, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) pastTense(verb string) string {
	if strings.HasSuffix(verb, "ing") {
		stem := strings.TrimSuffix(verb, "ing")
		if strings.HasSuffix(stem, "at") || strings.HasSuffix(stem, "ut") ||
			strings.HasSuffix(stem, "it") || strings.HasSuffix(stem, "et") {
			return stem + "ed"
		}
		if strings.HasSuffix(stem, "e") {
			return stem + "d"
		}
		return stem + "ed"
	}
	return verb
}

func (m Model) getModelName() string {
	if m.selectedProvider != nil {
		return m.selectedProvider.Model
	}
	if len(m.providers) > 0 {
		return m.providers[0].Model
	}
	return "unknown"
}

func (m Model) getWorkDir() string {
	wd, _ := os.Getwd()
	return wd
}

func nextPermissionMode(current permissions.PermissionMode) permissions.PermissionMode {
	switch current {
	case permissions.ModeDefault:
		return permissions.ModeAcceptEdits
	case permissions.ModeAcceptEdits:
		return permissions.ModePlan
	case permissions.ModePlan:
		return permissions.ModeBypass
	case permissions.ModeBypass:
		return permissions.ModeDefault
	default:
		return permissions.ModeDefault
	}
}

func (m Model) handleResume(args string) (tea.Model, tea.Cmd) {
	wd, _ := os.Getwd()
	sessions := session.ListSessions(wd)

	if args != "" {
		return m.doResumeSession(wd, args, sessions)
	}

	if len(sessions) == 0 {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role: "system", content: "No previous sessions found.",
		})
		m.updateViewport()
		return m, nil
	}

	m.resumeSessions = sessions
	m.resumeFiltered = sessions
	m.resumeCursor = 0
	m.resumeSearch = ""
	m.resumeScrollTop = 0
	m.state = stateResume
	return m, nil
}

func (m Model) doResumeSession(wd, targetID string, sessions []session.SessionInfo) (tea.Model, tea.Cmd) {
	var idx int
	if n, _ := fmt.Sscanf(targetID, "%d", &idx); n == 1 && idx >= 1 && idx <= len(sessions) {
		targetID = sessions[idx-1].ID
	}

	msgs := session.LoadSession(wd, strings.TrimSpace(targetID))
	if len(msgs) == 0 {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role: "error", content: fmt.Sprintf("Session '%s' not found or empty.", targetID),
		})
		m.updateViewport()
		return m, nil
	}

	m.chatMessages = nil
	m.committedUpTo = 0
	m.conversation = conversation.NewManager()
	m.sessionID = strings.TrimSpace(targetID)
	// 保持 Agent 的会话日志指针与恢复的会话同步，使稍后的压缩把边界写入
	// 同一个文件（链式恢复）。
	if m.ag != nil {
		m.ag.SetSessionID(m.sessionID)
	}

	// 感知压缩的重建：如果会话包含 compact_boundary，则实时对话是压缩后的
	// 状态——[summary] + 保留的尾部 + 边界之后追加的任何普通消息——而压缩前
	// 的前缀不会重放（它保留在文件中供审计）。没有边界（旧会话）时则原样
	// 重放所有消息。
	boundary, after, compacted := session.FindLastCompactBoundary(msgs)
	var replay []session.Message
	if compacted {
		resumeSummary := "本次会话延续自之前的对话，因上下文空间不足进行了压缩。以下是早期对话的摘要：\n\n" + boundary.Summary
		if len(boundary.Keep) > 0 {
			resumeSummary += "\n\n近期消息已原样保留。"
		}
		replay = append(replay, session.Message{Role: "user", Content: resumeSummary})
		for _, k := range boundary.Keep {
			replay = append(replay, session.Message{
				Role:        k.Role,
				Content:     k.Content,
				ToolUses:    k.ToolUses,
				ToolResults: k.ToolResults,
			})
		}
		replay = append(replay, after...)
	} else {
		replay = msgs
	}

	for _, msg := range replay {
		// 只带工具结果的消息没有文本，不上屏，但要进对话历史保住调用链
		if msg.Content != "" {
			m.chatMessages = append(m.chatMessages, chatMessage{role: msg.Role, content: msg.Content})
		}
		m.conversation.AppendMessages([]conversation.Message{msg.ToConversation()})
	}

	restored := fmt.Sprintf("Session %s restored (%d messages).", strings.TrimSpace(targetID), len(replay))
	if compacted {
		restored = fmt.Sprintf("Session %s restored from compacted state (summary + %d kept + %d newer messages).",
			strings.TrimSpace(targetID), len(boundary.Keep), len(after))
	}
	m.chatMessages = append(m.chatMessages, chatMessage{
		role:    "system",
		content: restored,
	})
	commitText := m.renderMessagesRange(0, len(m.chatMessages))
	m.committedUpTo = len(m.chatMessages)
	m.updateViewport()
	if commitText != "" {
		return m, tea.Println(commitText)
	}
	return m, nil
}

func (m Model) handleRewind() (tea.Model, tea.Cmd) {
	if m.fileHistory == nil {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role: "system", content: "No file history available (fileHistory is nil).",
		})
		m.updateViewport()
		return m, nil
	}
	if !m.fileHistory.HasSnapshots() {
		m.chatMessages = append(m.chatMessages, chatMessage{
			role: "system", content: "No checkpoints to rewind to (0 snapshots).",
		})
		m.updateViewport()
		return m, nil
	}
	m.rewindSnapshots = m.fileHistory.GetSnapshots()
	m.rewindCursor = len(m.rewindSnapshots) - 1
	m.rewindPhase = 0
	m.rewindOptionCursor = 0
	m.rewindDialog = true
	m.updateViewport()
	return m, nil
}

var rewindOptions = []string{
	"Restore code and conversation",
	"Restore conversation only",
	"Restore code only",
	"Never mind",
}

func (m Model) handleRewindKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.rewindPhase == 0 {
		return m.handleRewindPhase0(msg)
	}
	return m.handleRewindPhase1(msg)
}

func (m Model) handleRewindPhase0(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.rewindDialog = false
		m.textarea.Focus()
		m.updateViewport()
		return m, nil
	case "up":
		if m.rewindCursor > 0 {
			m.rewindCursor--
			m.updateViewport()
		}
		return m, nil
	case "down":
		if m.rewindCursor < len(m.rewindSnapshots)-1 {
			m.rewindCursor++
			m.updateViewport()
		}
		return m, nil
	case "enter":
		if m.rewindCursor < len(m.rewindSnapshots) {
			m.rewindPhase = 1
			m.rewindOptionCursor = 0
			m.updateViewport()
		}
		return m, nil
	}
	return m, nil
}

func (m Model) handleRewindPhase1(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.rewindPhase = 0
		m.updateViewport()
		return m, nil
	case "up":
		if m.rewindOptionCursor > 0 {
			m.rewindOptionCursor--
			m.updateViewport()
		}
		return m, nil
	case "down":
		if m.rewindOptionCursor < len(rewindOptions)-1 {
			m.rewindOptionCursor++
			m.updateViewport()
		}
		return m, nil
	case "enter":
		return m.executeRewindOption()
	}
	return m, nil
}

func (m Model) executeRewindOption() (tea.Model, tea.Cmd) {
	snap := m.rewindSnapshots[m.rewindCursor]
	var summary string

	switch m.rewindOptionCursor {
	case 0: // 恢复代码和对话
		changed, err := m.fileHistory.Rewind(m.rewindCursor)
		if err != nil {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: fmt.Sprintf("Rewind failed: %s", err)})
			m.rewindDialog = false
			m.textarea.Focus()
			m.updateViewport()
			return m, nil
		}
		m.conversation.TruncateTo(snap.MessageIndex)
		summary = fmt.Sprintf("⟲ Rewound to checkpoint %d. Restored %d file(s) and conversation.", m.rewindCursor+1, len(changed))
		for _, f := range changed {
			summary += "\n  • " + f
		}
		m.chatMessages = m.chatMessages[:0]
		m.committedUpTo = 0

	case 1: // 仅恢复对话
		m.conversation.TruncateTo(snap.MessageIndex)
		summary = fmt.Sprintf("⟲ Rewound conversation to checkpoint %d. Files unchanged.", m.rewindCursor+1)
		m.chatMessages = m.chatMessages[:0]
		m.committedUpTo = 0

	case 2: // 仅恢复代码
		changed, err := m.fileHistory.Rewind(m.rewindCursor)
		if err != nil {
			m.chatMessages = append(m.chatMessages, chatMessage{role: "error", content: fmt.Sprintf("Rewind failed: %s", err)})
			m.rewindDialog = false
			m.textarea.Focus()
			m.updateViewport()
			return m, nil
		}
		summary = fmt.Sprintf("⟲ Restored %d file(s) to checkpoint %d. Conversation unchanged.", len(changed), m.rewindCursor+1)
		for _, f := range changed {
			summary += "\n  • " + f
		}

	case 3: // 算了
		m.rewindDialog = false
		m.rewindPhase = 0
		m.textarea.Focus()
		m.updateViewport()
		return m, nil
	}

	m.chatMessages = append(m.chatMessages, chatMessage{role: "system", content: summary})
	m.rewindDialog = false
	m.rewindPhase = 0
	m.textarea.Focus()
	m.updateViewport()

	if m.rewindOptionCursor <= 1 {
		return m, tea.Batch(
			func() tea.Msg { return tea.ClearScreen() },
			tea.Println(m.renderBanner()+"\n"+lipgloss.NewStyle().Foreground(dimText).Render(summary)),
		)
	}
	commitText := m.renderMessagesRange(m.committedUpTo-1, len(m.chatMessages))
	m.committedUpTo = len(m.chatMessages)
	if commitText != "" {
		return m, tea.Println(commitText)
	}
	return m, nil
}

func (m Model) handleResumeKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.state = stateChat
		m.textarea.Focus()
		return m, nil
	case "enter":
		if m.resumeCursor < len(m.resumeFiltered) {
			selected := m.resumeFiltered[m.resumeCursor]
			m.state = stateChat
			m.textarea.Focus()
			wd, _ := os.Getwd()
			return m.doResumeSession(wd, selected.ID, m.resumeSessions)
		}
		return m, nil
	case "up":
		if m.resumeCursor > 0 {
			m.resumeCursor--
			if m.resumeCursor < m.resumeScrollTop {
				m.resumeScrollTop = m.resumeCursor
			}
		}
		return m, nil
	case "down":
		if m.resumeCursor < len(m.resumeFiltered)-1 {
			m.resumeCursor++
			maxVisible := m.resumeVisibleCount()
			if m.resumeCursor >= m.resumeScrollTop+maxVisible {
				m.resumeScrollTop = m.resumeCursor - maxVisible + 1
			}
		}
		return m, nil
	case "backspace":
		if len(m.resumeSearch) > 0 {
			m.resumeSearch = m.resumeSearch[:len(m.resumeSearch)-1]
			m.resumeFilterSessions()
		}
		return m, nil
	default:
		if len(msg.String()) == 1 && msg.String() >= " " {
			m.resumeSearch += msg.String()
			m.resumeFilterSessions()
			return m, nil
		}
	}
	return m, nil
}

func (m *Model) resumeFilterSessions() {
	m.resumeFiltered = nil
	for _, s := range m.resumeSessions {
		if session.MatchesSearch(s, m.resumeSearch) {
			m.resumeFiltered = append(m.resumeFiltered, s)
		}
	}
	m.resumeCursor = 0
	m.resumeScrollTop = 0
}

func (m Model) resumeVisibleCount() int {
	// 头部(1) + 搜索框(3) + 项目(1) + 空行(1) + 页脚(2) = 8
	available := m.height - 8
	perItem := 2 // 标题行 + 元数据行
	if available < perItem {
		return 1
	}
	return available / perItem
}

func (m Model) renderResumeView() string {
	var sb strings.Builder

	total := len(m.resumeFiltered)
	current := 0
	if total > 0 {
		current = m.resumeCursor + 1
	}

	// 头部
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(
		fmt.Sprintf("Resume session (%d of %d)", current, total),
	))
	sb.WriteString("\n")

	// 搜索框
	searchText := m.resumeSearch
	if searchText == "" {
		searchText = lipgloss.NewStyle().Foreground(dimText).Render("⌕ Search…")
	} else {
		searchText = "⌕ " + searchText
	}
	boxWidth := m.width - 6
	if boxWidth < 20 {
		boxWidth = 20
	}
	border := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("240")).
		Width(boxWidth).
		PaddingLeft(1)
	sb.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(border.Render(searchText)))
	sb.WriteString("\n")

	// 项目名
	wd, _ := os.Getwd()
	projectName := filepath.Base(wd)
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(4).Render(projectName))
	sb.WriteString("\n\n")

	// 会话列表
	maxVisible := m.resumeVisibleCount()
	if maxVisible > total {
		maxVisible = total
	}

	for i := m.resumeScrollTop; i < m.resumeScrollTop+maxVisible && i < total; i++ {
		s := m.resumeFiltered[i]

		// 标题行
		title := s.FirstMessage
		if title == "" {
			title = "(empty session)"
		}
		maxTitleLen := m.width - 8
		if maxTitleLen > 0 && len(title) > maxTitleLen {
			title = title[:maxTitleLen] + "…"
		}

		prefix := "  "
		if i == m.resumeCursor {
			prefix = "❯ "
			title = lipgloss.NewStyle().Foreground(cyanText).Bold(true).Render(title)
		} else {
			title = lipgloss.NewStyle().Foreground(normalText).Render(title)
		}
		sb.WriteString(lipgloss.NewStyle().PaddingLeft(2).Render(prefix + title))
		sb.WriteString("\n")

		// 元数据行
		var meta []string
		meta = append(meta, session.FormatRelativeTime(s.ModTime))
		if s.GitBranch != "" {
			meta = append(meta, s.GitBranch)
		}
		meta = append(meta, session.FormatFileSize(s.FileSize))
		metaStr := strings.Join(meta, " · ")
		sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(6).Render(metaStr))
		sb.WriteString("\n")

		if i < m.resumeScrollTop+maxVisible-1 && i < total-1 {
			sb.WriteString("\n")
		}
	}

	// 显示滚动指示器
	if total > maxVisible {
		if m.resumeScrollTop+maxVisible < total {
			sb.WriteString("\n")
			sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(2).Render(
				fmt.Sprintf("  ↓ %d more session(s)", total-m.resumeScrollTop-maxVisible),
			))
		}
	}

	// 补白到底部
	rendered := strings.Count(sb.String(), "\n") + 1
	footerHeight := 2
	pad := m.height - rendered - footerHeight
	if pad > 0 {
		sb.WriteString(strings.Repeat("\n", pad))
	}

	// 页脚
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimText).PaddingLeft(4).Render(
		"Type to search · Enter to select · Esc to cancel",
	))

	return sb.String()
}

func (m *Model) historyUp() {
	if len(m.historyEntries) == 0 {
		return
	}
	if m.historyIndex == 0 {
		m.historyDraft = m.textarea.Value()
	}
	if m.historyIndex < len(m.historyEntries) {
		m.historyIndex++
		m.textarea.Reset()
		m.textarea.SetHeight(1)
		m.textarea.SetValue(m.historyEntries[len(m.historyEntries)-m.historyIndex])
	}
}

func (m *Model) historyDown() {
	if m.historyIndex <= 0 {
		return
	}
	m.historyIndex--
	m.textarea.Reset()
	m.textarea.SetHeight(1)
	if m.historyIndex == 0 {
		m.textarea.SetValue(m.historyDraft)
	} else {
		m.textarea.SetValue(m.historyEntries[len(m.historyEntries)-m.historyIndex])
	}
}

func permissionModeInfo(mode permissions.PermissionMode) (string, string) {
	switch mode {
	case permissions.ModeDefault:
		return "Default", "Writes and commands require approval."
	case permissions.ModeAcceptEdits:
		return "Accept Edits", "File edits auto-approved; commands still require approval."
	case permissions.ModePlan:
		return "Plan", "Read-only mode. No writes or commands executed."
	case permissions.ModeBypass:
		return "YOLO", "All tools auto-approved. Use with caution."
	default:
		return string(mode), ""
	}
}

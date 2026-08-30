package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"cody/internal/agent"
	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/permissions"
	"cody/internal/tools"
)

type TaskStatus string

const (
	TaskPending    TaskStatus = "pending"
	TaskRunning    TaskStatus = "running"
	TaskCompleted  TaskStatus = "completed"
	TaskFailed     TaskStatus = "failed"
	TaskCancelled  TaskStatus = "cancelled"
)

type Task struct {
	ID        string
	Name      string
	Status    TaskStatus
	Output    string
	Error     string
	CreatedAt time.Time
	DoneAt    time.Time
	Cancel    context.CancelFunc
}

type TaskManager struct {
	mu    sync.Mutex
	tasks map[string]*Task
	nextID int
	notifications []TaskNotification
}

type TaskNotification struct {
	TaskID string
	Name   string
	Status TaskStatus
	Output string
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		tasks: make(map[string]*Task),
	}
}

func (tm *TaskManager) CreateTask(name string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.nextID++
	id := fmt.Sprintf("task_%d", tm.nextID)
	tm.tasks[id] = &Task{
		ID:        id,
		Name:      name,
		Status:    TaskPending,
		CreatedAt: time.Now(),
	}
	return id
}

func (tm *TaskManager) GetTask(id string) *Task {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.tasks[id]
}

func (tm *TaskManager) ListTasks() []*Task {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	var result []*Task
	for _, t := range tm.tasks {
		result = append(result, t)
	}
	return result
}

func (tm *TaskManager) SetRunning(id string, cancel context.CancelFunc) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok {
		t.Status = TaskRunning
		t.Cancel = cancel
	}
}

func (tm *TaskManager) SetCompleted(id, output string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok {
		t.Status = TaskCompleted
		t.Output = output
		t.DoneAt = time.Now()
		tm.notifications = append(tm.notifications, TaskNotification{
			TaskID: id,
			Name:   t.Name,
			Status: TaskCompleted,
			Output: output,
		})
	}
}

func (tm *TaskManager) SetFailed(id, errMsg string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok {
		t.Status = TaskFailed
		t.Error = errMsg
		t.DoneAt = time.Now()
		tm.notifications = append(tm.notifications, TaskNotification{
			TaskID: id,
			Name:   t.Name,
			Status: TaskFailed,
			Output: errMsg,
		})
	}
}

func (tm *TaskManager) DrainNotifications() []TaskNotification {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	n := tm.notifications
	tm.notifications = nil
	return n
}

func (tm *TaskManager) AdoptRunning(name string, eventCh <-chan agent.AgentEvent, cancel context.CancelFunc) string {
	taskID := tm.CreateTask("adopted: " + truncate(name, 40))
	tm.SetRunning(taskID, cancel)

	go func() {
		var output string
		for ev := range eventCh {
			switch e := ev.(type) {
			case agent.StreamText:
				output += e.Text
			case agent.ErrorEvent:
				tm.SetFailed(taskID, e.Message)
				return
			}
		}
		tm.SetCompleted(taskID, output)
	}()

	return taskID
}

func (tm *TaskManager) FindByName(name string) *Task {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for _, t := range tm.tasks {
		if t.Name == name || strings.HasPrefix(t.Name, name+":") {
			return t
		}
	}
	return nil
}

func (tm *TaskManager) CancelTask(id string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok && t.Cancel != nil {
		t.Cancel()
		t.Status = TaskCancelled
		t.DoneAt = time.Now()
		return true
	}
	return false
}

// SubAgentSpec 捕获 BaseAgentDefinition 中与运行时相关的子集。它是
// 加载层（AgentDefinition）与执行层（runSync / runAsync / runFork）之间的桥梁。
// 内置 agent 跳过文件解析，直接通过 BuiltinSpecs 实例化此结构。
type SubAgentSpec struct {
	Name                 string
	Description          string
	Tools                []string
	DisallowedTools      []string
	SystemPromptOverride string
	MaxTurns             int
	Model                string

	// PermissionMode 在子 agent 运行期间覆盖父 agent 的权限模式。空字符串
	// 表示继承自父 agent。
	PermissionMode string

	// Background 强制该 agent 在生成时作为后台任务运行，
	// 无论调用点是否传入 run_in_background 参数。
	Background bool

	// Isolation 选择文件系统隔离模式；"worktree" 会创建一个临时 git worktree。
	Isolation IsolationMode

	// InitialPrompt 会前置到第一轮用户消息之前。
	InitialPrompt string

	// OmitCodyMd 从该 agent 的 userContext 中去掉 CODY.md 层级。
	OmitCodyMd bool

	// Skills 是子 agent 启动时要预加载的 skill 名称。
	Skills []string

	// Memory 在三种作用域之一中启用持久化记忆。
	Memory AgentMemoryScope

	// McpServers / RequiredMcpServers / Hooks / Effort 向前携带 frontmatter 数据，
	// 以便未来的通道无需再次进行 schema 迁移即可消费它们。
	McpServers         []any
	RequiredMcpServers []string
	Hooks              any
	Effort             any
}

const planAgentSystemPrompt = `You are a software architect and planning specialist.

=== CRITICAL: READ-ONLY MODE - NO FILE MODIFICATIONS ===
You are STRICTLY PROHIBITED from creating, modifying, or deleting any files.
Your role is EXCLUSIVELY to explore code and design implementation plans.

## Your Process

1. **Understand Requirements**: Analyze the user's request carefully.

2. **Explore Thoroughly**:
   - Read files with ReadFile to understand current architecture
   - Use Grep to find patterns, function definitions, and references
   - Use Glob to discover file structure
   - Use Bash ONLY for read-only operations (ls, find, grep, cat, head, tail)
   - NEVER use Bash for: mkdir, touch, rm, cp, mv, git add/commit, npm install

3. **Design Solution**:
   - Create a concrete implementation approach
   - Consider trade-offs and explain your reasoning
   - Follow existing patterns in the codebase

4. **Detail the Plan**:
   - Provide step-by-step implementation strategy
   - Identify file dependencies and sequencing
   - Anticipate potential challenges

## Required Output
End your response with:

### Critical Files for Implementation
List the most critical files for implementing this change:
- path/to/file1 — reason
- path/to/file2 — reason`

var BuiltinSpecs = map[string]SubAgentSpec{
	"general-purpose": {
		Name:        "general-purpose",
		Description: "General-purpose agent for research and multi-step tasks",
		MaxTurns:    200,
	},
	"plan": {
		Name:                 "plan",
		Description:          "Software architect for designing implementation plans. Returns step-by-step plans, identifies critical files, and considers architectural trade-offs.",
		DisallowedTools:      []string{"EditFile", "WriteFile"},
		SystemPromptOverride: planAgentSystemPrompt,
		MaxTurns:             15,
	},
	"explore": {
		Name:            "explore",
		Description:     "Fast read-only search agent for locating code",
		DisallowedTools: []string{"EditFile", "WriteFile"},
		// 省略 MaxTurns → 默认 200（与 general-purpose 相同的回退值）。之前的 30 轮上限
		// 在 LLM 需要发出大量 ToolSearch/Glob/Grep 调用来摸清陌生仓库时会触发，
		// 导致 spawn 在能报告任何有用信息之前就以 "reached maximum iterations" 失败。
		Model: "haiku",
	},
}

func SpawnSubAgent(
	ctx context.Context,
	taskMgr *TaskManager,
	client llm.Client,
	registry *tools.Registry,
	protocol string,
	spec SubAgentSpec,
	taskPrompt string,
	parentChecker *permissions.Checker,
) string {
	taskID := taskMgr.CreateTask(spec.Name + ": " + truncate(taskPrompt, 50))

	subCtx, cancel := context.WithCancel(ctx)
	taskMgr.SetRunning(taskID, cancel)

	subRegistry := FilterToolsForAgent(registry, spec.Tools, spec.DisallowedTools, true)

	subAgent := agent.New(client, subRegistry, protocol)
	subAgent.Checker = deriveSubAgentChecker(parentChecker, spec.PermissionMode)
	if spec.MaxTurns > 0 {
		subAgent.MaxIterations = spec.MaxTurns
	} else {
		subAgent.MaxIterations = 200
	}

	go func() {
		conv := conversation.NewManager()
		if spec.SystemPromptOverride != "" {
			conv.AddSystemReminder(spec.SystemPromptOverride)
		}
		// initialPrompt 会前置到第一轮用户消息之前。
		if spec.InitialPrompt != "" {
			conv.AddUserMessage(spec.InitialPrompt)
		}
		conv.AddUserMessage(taskPrompt)

		var output string
		ch := subAgent.Run(subCtx, conv)
		for ev := range ch {
			switch e := ev.(type) {
			case agent.StreamText:
				output += e.Text
			case agent.PermissionRequestEvent:
				// 后台子 agent 是无头的：自动拒绝，这样 executeSingleTool 不会在 respCh
				// 上永远挂起。
				e.ResponseCh <- agent.PermDeny
			case agent.ErrorEvent:
				taskMgr.SetFailed(taskID, e.Message)
				return
			case agent.LoopComplete:
				// done
			}
		}
		taskMgr.SetCompleted(taskID, output)
	}()

	return taskID
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

package agents

import (
	"strings"

	"cody/internal/tools"
)

// AllAgentDisallowedTools 是任何子 Agent 都拿不到的工具，Agent 定义里的白名单也开不了它们。
// 其中 TaskOutput、ExitPlanMode、EnterPlanMode、Workflow 在当前工具集里并没有对应实现，
// 留着是为了把「子 Agent 不该有哪些能力」这份清单写全，过滤时遇到不认识的名字直接跳过，
// 将来补上同名工具就自动生效。
var AllAgentDisallowedTools = map[string]bool{
	"TaskOutput":      true,
	"ExitPlanMode":    true,
	"EnterPlanMode":   true,
	"Agent":           true,
	"AskUserQuestion": true,
	"TaskStop":        true,
	"Workflow":        true,
}

// CustomAgentDisallowedTools 只是 ALL_AGENT_DISALLOWED_TOOLS 的一份克隆。单独保留一张
// map，这样将来加额外限制时不必改动全局列表。
var CustomAgentDisallowedTools = map[string]bool{
	"TaskOutput":      true,
	"ExitPlanMode":    true,
	"EnterPlanMode":   true,
	"Agent":           true,
	"AskUserQuestion": true,
	"TaskStop":        true,
	"Workflow":        true,
}

// AsyncAgentAllowedTools 异步（后台）agent 只能使用这些工具——不能使用 Agent（不能嵌套
// 孵化）、TaskOutput、ExitPlanMode、TaskStop。本地工具命名映射 FILE_READ → ReadFile、
// FILE_EDIT → EditFile、FILE_WRITE → WriteFile。
// 系统有一个硬编码的后台工具白名单 ASYNC_AGENT_ALLOWED_TOOLS
// async agent 只能用这些工具，不能 spawn Agent
var AsyncAgentAllowedTools = map[string]bool{
	"ReadFile":        true,
	"WebSearch":       true,
	"TodoWrite":       true,
	"Grep":            true,
	"WebFetch":        true,
	"Glob":            true,
	"Bash":            true,
	"EditFile":        true,
	"WriteFile":       true,
	"NotebookEdit":    true,
	"Skill":           true,
	"LoadSkill":       true,
	"SyntheticOutput": true,
	"ToolSearch":      true,
	"EnterWorktree":   true,
	"ExitWorktree":    true,
}

// InProcessTeammateAllowedTools 当子 Agent 以进程内队友（ch15 Agent Teams）的形式被孵化时，
// 除了异步白名单之外，还可以使用这些协作工具，以便管理共享任务列表并向同伴发送消息。
var InProcessTeammateAllowedTools = map[string]bool{
	"TaskCreate":  true,
	"TaskGet":     true,
	"TaskList":    true,
	"TaskUpdate":  true,
	"SendMessage": true,
	"CronCreate":  true,
	"CronDelete":  true,
	"CronList":    true,
}

// TeammateDisallowedTools 队友在协作工具之外额外被挡掉的工具。组建和解散团队
// 由 Lead 负责，队友只管干活和相互协调，不参与团队成员管理。
var TeammateDisallowedTools = []string{"TeamCreate", "TeamDelete"}

func IsMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

func FilterToolsForAgent(reg *tools.Registry, allowedTools, disallowedTools []string, isAsync bool) *tools.Registry {
	return FilterToolsForAgentEx(reg, allowedTools, disallowedTools, isAsync, false, false)
}

// FilterToolsForAgentEx
//
// 按顺序应用的过滤层：1. MCP 工具（mcp__*）——始终放行 2. ALL_AGENT_DISALLOWED_TOOLS——
// 全局禁用（仅限递归 / 主线程） 3. CUSTOM_AGENT_DISALLOWED_TOOLS——仅自定义（非内置）
// agent 生效 4. ASYNC_AGENT_ALLOWED_TOOLS——后台 agent 走白名单；如果 agent 是进程内队友，
// 额外放行 IN_PROCESS_TEAMMATE_ALLOWED_TOOLS 5. Agent 定义的 disallowedTools——定义级黑名单
// 6. Agent 定义的 tools——定义级白名单交集（"*" 表示关闭该层）。
//
// isCustom：从 .cody/agents/ 加载的 agent，而非内置。isInProcessTeammate：通过 ch15 中的
// TeamCreate / SpawnTeammate 孵化。
func FilterToolsForAgentEx(reg *tools.Registry, allowedTools, disallowedTools []string, isAsync, isCustom, isInProcessTeammate bool) *tools.Registry {
	disallowed := make(map[string]bool, len(disallowedTools))
	for _, name := range disallowedTools {
		disallowed[name] = true
	}

	allowed := make(map[string]bool, len(allowedTools))
	hasWhitelist := len(allowedTools) > 0 && !(len(allowedTools) == 1 && allowedTools[0] == "*")
	for _, name := range allowedTools {
		allowed[name] = true
	}

	filtered := tools.NewRegistry()
	for _, t := range reg.ListTools() {
		name := t.Name()

		// 第 1 层：MCP 工具始终放行。
		if IsMCPTool(name) {
			filtered.Register(t)
			continue
		}

		// 第 2 层：全局禁用（对每个子 Agent 生效）。
		if AllAgentDisallowedTools[name] {
			continue
		}

		// 第 3 层：自定义 agent 的额外限制。
		if isCustom && CustomAgentDisallowedTools[name] {
			continue
		}

		// 第 4 层：异步 agent 白名单（含进程内队友扩展）。
		if isAsync && !AsyncAgentAllowedTools[name] {
			if isInProcessTeammate {
				// 进程内队友还可以使用 Agent（仅限同步子 agent，调用处校验）以及
				// 协作工具。
				//系统有一个硬编码的后台工具白名单 ASYNC_AGENT_ALLOWED_TOOLS
				if name == "Agent" || InProcessTeammateAllowedTools[name] {
					filtered.Register(t)
					continue
				}
			}
			continue
		}

		// 第 5 层：定义级禁用。
		if disallowed[name] {
			continue
		}

		// 第 6 层：定义级放行（白名单交集）。
		if hasWhitelist && !allowed[name] {
			continue
		}

		filtered.Register(t)
	}
	return filtered
}


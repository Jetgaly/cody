package teams

import (
	"context"
	"fmt"
	"strings"

	"cody/internal/tools"
)

// TaskStopTool 中止一个在跑的队员。
// Coordinator 派错方向时用它及时止损，不用等队员把错的活干完。
//
// 接的是 TeamManager 而不是后台任务表：coordinator 模式下 Lead 通过 Agent 工具
// 加 team_name 派出去的是队员，由 Team 持有它们的 Cancel，后台任务表里没有它们。
type TaskStopTool struct {
	TeamMgr *TeamManager
}

func (t *TaskStopTool) Name() string                 { return "TaskStop" }
func (t *TaskStopTool) Category() tools.ToolCategory { return tools.CategoryCommand }

func (t *TaskStopTool) Description() string {
	return "Stop a running teammate. Pass the teammate name as it appears in the from= field of a team-notification. " +
		"Use this when you sent a teammate in the wrong direction — for example when the user " +
		"changes requirements after you launched it."
}

func (t *TaskStopTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"teammate": map[string]any{
					"type":        "string",
					"description": "Name of the teammate to stop, exactly as it appears in the from= field of a team-notification",
				},
			},
			"required": []string{"teammate"},
		},
	}
}

func (t *TaskStopTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	name, _ := args["teammate"].(string)
	if name == "" {
		return tools.ToolResult{Output: "Error: teammate is required", IsError: true}
	}
	if t.TeamMgr == nil {
		return tools.ToolResult{Output: "Error: team manager unavailable", IsError: true}
	}

	// 队员名在团队之间可能重名，只在存在该成员的团队里停，避免误杀同名队员
	for _, teamName := range t.TeamMgr.ListTeams() {
		team := t.TeamMgr.GetTeam(teamName)
		if team == nil {
			continue
		}
		member, ok := team.Members[name]
		if !ok {
			continue
		}
		if !member.Active {
			return tools.ToolResult{
				Output: fmt.Sprintf("Teammate '%s' in team '%s' is not running, nothing to stop", name, teamName),
			}
		}
		team.StopMember(name)
		return tools.ToolResult{
			Output: fmt.Sprintf("Teammate '%s' in team '%s' stopped.", name, teamName),
		}
	}

	return tools.ToolResult{
		Output:  fmt.Sprintf("Error: teammate '%s' not found. Known teammates: %s", name, t.knownMembers()),
		IsError: true,
	}
}

// knownMembers 把当前所有队员名列给模型，省得它照着记错的名字反复重试
func (t *TaskStopTool) knownMembers() string {
	var names []string
	for _, teamName := range t.TeamMgr.ListTeams() {
		team := t.TeamMgr.GetTeam(teamName)
		if team == nil {
			continue
		}
		for memberName := range team.Members {
			names = append(names, memberName)
		}
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

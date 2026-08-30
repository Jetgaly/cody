package teams

import (
	"context"
	"fmt"
	"strings"

	"cody/internal/tools"
)

// SendMessageTool 允许 agent 向具名的队友发送消息。
type SendMessageTool struct {
	TeamMgr    *TeamManager
	SenderName string
}

func (t *SendMessageTool) Name() string                 { return "SendMessage" }
func (t *SendMessageTool) Category() tools.ToolCategory { return tools.CategoryCommand }
func (t *SendMessageTool) Description() string {
	return "Send a message to another named agent in the team. The recipient will see it on their next turn."
}

func (t *SendMessageTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to": map[string]any{
					"type":        "string",
					"description": "Name of the recipient agent",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Message content to send. For shutdown_request this is the reason; for plan_approval_response this is your feedback when rejecting.",
				},
				"type": map[string]any{
					"type": "string",
					"enum": []string{
						MsgText, MsgShutdownRequest, MsgShutdownResponse, MsgPlanApprovalResponse,
					},
					"description": "Message kind, defaults to 'text'. Use 'shutdown_request' to ask a teammate to wrap up (it replies with shutdown_response). Use 'plan_approval_response' to answer a teammate's plan, together with 'approve' and, when rejecting, feedback in 'content'.",
				},
				"request_id": map[string]any{
					"type":        "string",
					"description": "Required for plan_approval_response: copy the requestId from the teammate's plan approval request so it knows which plan you are answering.",
				},
				"approve": map[string]any{
					"type":        "boolean",
					"description": "Required for plan_approval_response: true to let the teammate start executing, false to send it back to revise.",
				},
			},
			"required": []string{"to", "content"},
		},
	}
}

func (t *SendMessageTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	to, _ := args["to"].(string)
	content, _ := args["content"].(string)
	msgType, _ := args["type"].(string)
	requestID, _ := args["request_id"].(string)
	approve, hasApprove := args["approve"].(bool)
	if to == "" || content == "" {
		return tools.ToolResult{Output: "Error: 'to' and 'content' are required", IsError: true}
	}

	// 结构化消息走独立通道：它们要带 requestId 和表态，拼进正文的话
	// 收件方还得从自然语言里猜，那就退回到「靠理解措辞来协调」了。
	if msgType != "" && msgType != MsgText {
		return t.sendTyped(to, msgType, requestID, content, approve, hasApprove)
	}

	// 广播：把消息发给发送者所在团队的所有其它成员
	if to == "*" {
		for _, teamName := range t.TeamMgr.ListTeams() {
			team := t.TeamMgr.GetTeam(teamName)
			if team == nil {
				continue
			}
			if _, ok := team.Members[t.SenderName]; !ok {
				continue
			}
			count := 0
			for member := range team.Members {
				if member == t.SenderName {
					continue
				}
				team.SendMessage(t.SenderName, member, content)
				count++
			}
			return tools.ToolResult{Output: fmt.Sprintf("Message broadcast to %d teammate(s).", count)}
		}
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: cannot find team for sender '%s'", t.SenderName),
			IsError: true,
		}
	}

	// Lead 没有注册为 Member（它活在父进程中，只读自己的信箱），
	// 所以通过找到发送者所属的任意团队来给它路由消息。
	if to == LeadName {
		for _, teamName := range t.TeamMgr.ListTeams() {
			team := t.TeamMgr.GetTeam(teamName)
			if team == nil {
				continue
			}
			if _, ok := team.Members[t.SenderName]; ok {
				team.SendMessage(t.SenderName, LeadName, content)
				return tools.ToolResult{
					Output: fmt.Sprintf("Message sent to %s.", LeadName),
				}
			}
		}
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: cannot find team for sender '%s'", t.SenderName),
			IsError: true,
		}
	}

	// 通过全局名称注册表把收件人名字解析成投递用的标识；解析不到就按原名兜底。
	recipient := to
	if resolved := GetNameRegistry().Resolve(to); resolved != "" {
		recipient = resolved
	}

	// 找到这个名字对应的已注册队友，否则回退到
	// 基于文件的信箱。在 tmux/iTerm 模式下，每个队友运行在
	// 独立进程中，Members 里只知道自己，因此
	// 内存查找会漏掉同伴。信箱写入总是能成功，
	// 因为所有进程共享磁盘上同一个收件目录。
	for _, teamName := range t.TeamMgr.ListTeams() {
		team := t.TeamMgr.GetTeam(teamName)
		if team == nil {
			continue
		}
		if _, ok := team.Members[recipient]; ok {
			team.SendMessage(t.SenderName, recipient, content)
			return tools.ToolResult{
				Output: fmt.Sprintf("Message sent to %s.", to),
			}
		}
		// 收件人不在 Members 里，但我们属于这个团队——直接写入
		// 文件信箱，这样外部进程的同伴会在下次轮询时
		// 取到它。
		if _, ok := team.Members[t.SenderName]; ok {
			team.SendMessage(t.SenderName, recipient, content)
			return tools.ToolResult{
				Output: fmt.Sprintf("Message sent to %s.", to),
			}
		}
	}

	return tools.ToolResult{
		Output:  fmt.Sprintf("Error: recipient '%s' not found in any team", to),
		IsError: true,
	}
}

// TeamCreateTool 创建一个新的 agent 团队。
type TeamCreateTool struct {
	TeamMgr *TeamManager
}

func (t *TeamCreateTool) Name() string                 { return "TeamCreate" }
func (t *TeamCreateTool) Category() tools.ToolCategory { return tools.CategoryCommand }
func (t *TeamCreateTool) Description() string {
	return `Create a new team for coordinating multiple agents.

## When to Use

Use this tool proactively whenever:
- The user explicitly asks to use a team, swarm, or group of agents
- The user mentions wanting agents to work together, coordinate, or collaborate
- A task requires sequential or parallel collaboration between multiple agents

When in doubt about whether a task warrants a team, prefer spawning a team.

## Team Workflow

1. **Create a team** with TeamCreate
2. **Spawn teammates** using the Agent tool with team_name and name parameters — this is REQUIRED to create long-running team members
3. Teammates work independently and communicate via **SendMessage**
4. When a teammate finishes, it sends its result to "lead" via SendMessage, then goes idle
5. The lead collects and synthesizes all teammate results

## CRITICAL: Spawning Teammates

To add a member to a team, you MUST pass both team_name and name to the Agent tool:
` + "```" + `
Agent({
  "team_name": "<team name from step 1>",
  "name": "<member name, e.g. reviewer>",
  "prompt": "...",
  "description": "..."
})
` + "```" + `
Without team_name, the agent runs as a one-shot sub-agent that blocks and returns inline — it will NOT be a team member.

## Teammate Idle State

Teammates go idle after every turn — this is completely normal. A teammate going idle after sending a message does NOT mean they are done or unavailable. Sending a message to an idle teammate wakes them up.

## Communication

- Use SendMessage to talk to teammates by name
- Messages from teammates arrive as system reminders at the start of each turn
- Messages are delivered automatically — you do NOT need to manually check your inbox`
}

func (t *TeamCreateTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"team_name": map[string]any{
					"type":        "string",
					"description": "Name for the team",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "What this team will work on",
				},
			},
			"required": []string{"team_name"},
		},
	}
}

func (t *TeamCreateTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	name, _ := args["team_name"].(string)
	if name == "" {
		return tools.ToolResult{Output: "Error: team_name is required", IsError: true}
	}

	// 去重：如果 name 已存在，追加后缀
	baseName := name
	for i := 2; t.TeamMgr.GetTeam(name) != nil; i++ {
		name = fmt.Sprintf("%s-%d", baseName, i)
	}

	// 探测使用什么后端
	mode := detectBackend()
	desc, _ := args["description"].(string)
	// CreateTeamFull 把 lead 和描述一并落进 config.json，之后队友进程和下次会话都能靠 GetTeam 把它捞回来
	team := t.TeamMgr.CreateTeamFull(name, mode, LeadName, desc)
	return tools.ToolResult{
		Output: fmt.Sprintf("Team \"%s\" created (mode: %s). Use Agent tool with team_name=\"%s\" to add teammates.\nDescription: %s",
			team.Name, team.Mode, team.Name, desc),
	}
}

// TeamDeleteTool 删除一个 agent 团队并停止所有成员。
type TeamDeleteTool struct {
	TeamMgr *TeamManager
}

func (t *TeamDeleteTool) Name() string { return "TeamDelete" }

func (t *TeamDeleteTool) Category() tools.ToolCategory { return tools.CategoryCommand }
func (t *TeamDeleteTool) Description() string {
	return "Delete a team, stopping all its members."
}

func (t *TeamDeleteTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"team_name": map[string]any{
					"type":        "string",
					"description": "Name of the team to delete",
				},
			},
			"required": []string{"team_name"},
		},
	}
}

func (t *TeamDeleteTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	name, _ := args["team_name"].(string)
	if name == "" {
		return tools.ToolResult{Output: "Error: team_name is required", IsError: true}
	}

	team := t.TeamMgr.GetTeam(name)
	if team == nil {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: team '%s' not found", name),
			IsError: true,
		}
	}

	memberCount := len(team.Members)
	var memberNames []string
	for n := range team.Members {
		memberNames = append(memberNames, n)
	}

	t.TeamMgr.DeleteTeam(name)
	return tools.ToolResult{
		Output: fmt.Sprintf("Team \"%s\" deleted. Stopped %d member(s): %s", name, memberCount, strings.Join(memberNames, ", ")),
	}
}

// sendTyped 投递结构化消息。它和普通文本走同一个信箱，区别只在于
// 消息上带了 Type / RequestID / Approve 三个字段，收件方按字段判断，不用解析措辞。
func (t *SendMessageTool) sendTyped(to, msgType, requestID, content string, approve, hasApprove bool) tools.ToolResult {
	var msg FileMailMessage
	switch msgType {
	case MsgShutdownRequest:
		msg = NewShutdownRequest(t.SenderName, content)
	case MsgShutdownResponse:
		if !hasApprove {
			return tools.ToolResult{Output: "Error: shutdown_response requires 'approve'", IsError: true}
		}
		msg = NewShutdownResponse(t.SenderName, requestID, approve, content)
	case MsgPlanApprovalResponse:
		if requestID == "" || !hasApprove {
			return tools.ToolResult{
				Output:  "Error: plan_approval_response requires both 'request_id' and 'approve'",
				IsError: true,
			}
		}
		msg = NewPlanApprovalResponse(t.SenderName, requestID, approve, content)
	default:
		return tools.ToolResult{Output: "Error: unsupported message type " + msgType, IsError: true}
	}

	team := t.senderTeam()
	if team == nil {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: cannot find team for sender '%s'", t.SenderName),
			IsError: true,
		}
	}
	if err := team.MailBox.Send(to, msg); err != nil {
		return tools.ToolResult{Output: "Error sending message: " + err.Error(), IsError: true}
	}
	return tools.ToolResult{Output: fmt.Sprintf("%s sent to %s.", msgType, to)}
}

// senderTeam 找到发送者所属的团队。Lead 本身不在 Members 里，
// 它发消息时取自己名下唯一的那个团队。
func (t *SendMessageTool) senderTeam() *Team {
	names := t.TeamMgr.ListTeams()
	for _, name := range names {
		team := t.TeamMgr.GetTeam(name)
		if team == nil {
			continue
		}
		if _, ok := team.Members[t.SenderName]; ok {
			return team
		}
	}
	if t.SenderName == LeadName && len(names) > 0 {
		return t.TeamMgr.GetTeam(names[0])
	}
	return nil
}

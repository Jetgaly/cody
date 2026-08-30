package teams

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"cody/internal/agent"
	"cody/internal/conversation"
	"cody/internal/permissions"
	"cody/internal/planfile"
)

// LeadName 是协调方使用的约定俗成的发送方/接收方标识。队友
// 把空闲通知发到这里，并从 From == LeadName 的消息中读取 Lead 的任务分派。
const LeadName = "lead"

// ShutdownPrefix 将邮箱消息标记为终止队友的请求。Lead 写入
// 这样一条消息来干净地结束成员；runner 在空闲轮询时看到它并退出循环。
const ShutdownPrefix = "[shutdown]"

// IdlePollInterval 是空闲队友扫描收件箱寻找新工作的频率。
const IdlePollInterval = 500 * time.Millisecond

// IsShutdownRequest 通过匹配 shutdown 前缀，报告一条邮箱消息是否要求队友退出。

// CreateIdleNotification 构造队友完成一轮后发送给 Lead 的消息。
// Lead 通过读取这些消息来分派工作。
func CreateIdleNotification(memberName, reason string) FileMailMessage {
	return NewFileMailMessage(memberName, fmt.Sprintf("[idle] %s (reason: %s)", memberName, reason))
}

// RunInProcessTeammate 在当前进程中驱动队友的主循环。它会阻塞直到 ctx
// 被取消或收件箱中出现 shutdown 请求。每次迭代：
//
// 1. waitForNextPromptOrShutdown — 将待处理的邮箱消息折叠成用户提示（或在
// shutdown / 取消时返回）。 2. runAgent — 在共享会话上调用 agent.Run；通过
// eventOut 转发事件。channel 关闭表示一轮结束。 3. sendIdleNotification — 向
// Lead 的收件箱投入一条空闲标记，以便它分派下一个任务。
//
// 初始提示启动第一次迭代；后续迭代的提示来自邮箱。
func RunInProcessTeammate(
	ctx context.Context,
	team *Team,
	member *Member,
	initialPrompt string,
	addendum string,
	eventOut chan<- agent.AgentEvent,
) error {
	if addendum != "" {
		member.Conv.AddSystemReminder(addendum)
	}

	nextPrompt := initialPrompt
	idleReason := "available"

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// 把本轮开始前落入收件箱的消息作为系统提醒折入会话，
		// 让模型把它们视为入站通知，而非用户指令。
		if reminder := InjectPendingMessages(team, member.Name); reminder != "" {
			member.Conv.AddSystemReminder(reminder)
		}

		if nextPrompt != "" {
			member.Conv.AddUserMessage(nextPrompt)
		}
		nextPrompt = ""

		ch := member.AgentRef.Run(ctx, member.Conv)
		for ev := range ch {
			// 更新进度追踪
			if member.Progress != nil {
				switch e := ev.(type) {
				case agent.ToolUseEvent:
					member.Progress.RecordToolUse(e.ToolName, e.Args)
				case agent.UsageEvent:
					member.Progress.RecordTokens(int64(e.InputTokens), int64(e.OutputTokens))
				}
			}
			if eventOut != nil {
				select {
				case eventOut <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if e, ok := ev.(agent.ErrorEvent); ok && e.Message != "" {
				idleReason = "failed"
			}
		}

		if member.Progress != nil {
			if idleReason == "failed" {
				member.Progress.SetStatus("failed")
			} else {
				member.Progress.SetStatus("idle")
			}
		}

		// 计划模式的队友：一轮跑完意味着它调了 ExitPlanMode，计划已经落到磁盘。
		// 把计划交给 Lead 审批，通过了才解除只读限制开始动手。
		if planModeActive(member) {
			approved, feedback, err := requestPlanApproval(ctx, team, member)
			if err != nil {
				return err
			}
			if approved {
				// 批准后切回正常权限，队友可以改文件了
				member.AgentRef.Checker.Mode = permissions.ModeDefault
				nextPrompt = "Lead 已批准你的计划，现在按计划开始执行。"
			} else {
				// 驳回时留在计划模式，带着修改意见重写计划
				nextPrompt = "Lead 驳回了你的计划，修改意见：" + feedback + "\n请据此修订计划后再次提交。"
			}
			continue
		}

		// 通知 Lead 该队友已完成本轮，以便 Lead 决定是否继续派活给它。
		_ = team.MailBox.Send(LeadName, CreateIdleNotification(member.Name, idleReason))
		idleReason = "available"

		// 空闲轮询。先睡眠 IdlePollInterval，然后清空收件箱。遇到 shutdown 消息就停止；否则
		// 构造下一个提示并回到循环。
		prompt, shutdown, err := waitForNextPromptOrShutdown(ctx, team, member.Name)
		if err != nil {
			return err
		}
		if shutdown != nil {
			// 收工前先给 Lead 一个明确答复，让它知道可以回收窗格了。
			// 队友这里一律同意：它已经处在空闲轮询里，手上没有干到一半的活。
			// 真正需要拒绝的场景是干活干到一半被打断，那种情况下队友根本轮询不到这条消息。
			if shutdown.Type == MsgShutdownRequest {
				_ = team.MailBox.Send(LeadName,
					NewShutdownResponse(member.Name, shutdown.RequestID, true, "acknowledged, shutting down"))
			}
			return nil
		}
		nextPrompt = prompt
	}
}

// waitForNextPromptOrShutdown 阻塞直到收件箱至少有一条消息，然后把
// 未读批次转换成下一个用户提示。若任何消息是 shutdown 请求，函数
// 直接返回 shutdown=true 而不构造提示。
// planModeActive 判断队友是否处在计划模式。只有被 Lead 标了 planModeRequired
// 的队友才会进这个模式，普通队友直接干活。
func planModeActive(member *Member) bool {
	return member.AgentRef != nil &&
		member.AgentRef.Checker != nil &&
		member.AgentRef.Checker.Mode == permissions.ModePlan
}

// requestPlanApproval 把队友写好的计划发给 Lead，然后阻塞等待批复。
//
// 队友这时候手上是只读权限，等多久都不会造成破坏，所以这里不设超时：
// 与其超时后自作主张开始改文件，不如一直等着，由用户从 Lead 那边推进。
func requestPlanApproval(ctx context.Context, team *Team, member *Member) (bool, string, error) {
	plan := readPlanForReview(member)
	req := NewPlanApprovalRequest(member.Name, plan)
	if err := team.MailBox.Send(LeadName, req); err != nil {
		return false, "", err
	}
	if member.Progress != nil {
		member.Progress.SetStatus("awaiting plan approval")
	}

	for {
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(IdlePollInterval):
		}

		msgs, err := team.MailBox.ReadUnread(member.Name)
		if err != nil {
			return false, "", err
		}
		for _, m := range msgs {
			// 只认对应这次请求的批复，别的消息留到下一轮再处理
			if m.Type == MsgPlanApprovalResponse && m.RequestID == req.RequestID {
				_ = team.MailBox.MarkAllRead(member.Name)
				return m.Approved(), m.Text, nil
			}
		}
	}
}

// readPlanForReview 读出队友写好的计划全文，交给 Lead 审阅。
func readPlanForReview(member *Member) string {
	workDir := ""
	if member.AgentRef != nil {
		workDir = member.AgentRef.WorkDir
	}
	path := planfile.GetOrCreatePlanPath(workDir)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return "（计划文件为空，队友可能未按要求写入计划）"
	}
	return string(data)
}

func waitForNextPromptOrShutdown(ctx context.Context, team *Team, memberName string) (string, *FileMailMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(IdlePollInterval):
		}

		msgs, err := team.MailBox.ReadUnread(memberName)
		if err != nil {
			return "", nil, err
		}
		if len(msgs) == 0 {
			continue
		}

		var shutdown *FileMailMessage
		var keep []FileMailMessage
		for i, m := range msgs {
			if IsShutdownRequest(m) {
				shutdown = &msgs[i]
				continue
			}
			keep = append(keep, m)
		}
		_ = team.MailBox.MarkAllRead(memberName)

		if shutdown != nil {
			return "", shutdown, nil
		}
		return formatInboundAsPrompt(keep), nil, nil
	}
}

// DrainLeadMailbox 读取每个团队 Lead 收件箱中的所有未读通知，并把它们作为
// system-reminder 字符串返回（每个团队一条）。Lead 的主循环把它挂到
// Agent.NotificationFn 上，这样队友的空闲通知会在每轮开始时呈现给模型。
func DrainLeadMailbox(mgr *TeamManager) []string {
	if mgr == nil {
		return nil
	}
	var notes []string
	for _, name := range mgr.ListTeams() {
		team := mgr.GetTeam(name)
		if team == nil {
			continue
		}
		msgs, err := team.MailBox.ReadUnread(LeadName)
		if err != nil || len(msgs) == 0 {
			continue
		}
		var sb strings.Builder
		sb.WriteString("<team-notification team=\"")
		sb.WriteString(name)
		sb.WriteString("\">\n")
		for _, m := range msgs {
			sb.WriteString("from=")
			sb.WriteString(m.From)
			sb.WriteString(": ")
			sb.WriteString(m.Text)
			sb.WriteString("\n")
		}
		sb.WriteString("</team-notification>")
		notes = append(notes, sb.String())
		_ = team.MailBox.MarkAllRead(LeadName)
	}
	return notes
}

// formatInboundAsPrompt 将未读批次转换成一条用户提示。每条消息都带有
// 发送者标记，方便队友路由回复。与 formatAsTeammateMessage 对应，
// 简化为纯文本而非 XML。
func formatInboundAsPrompt(msgs []FileMailMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("You have new messages from your team:\n\n")
	for _, m := range msgs {
		sb.WriteString(fmt.Sprintf("From %s: %s\n\n", m.From, m.Text))
	}
	return sb.String()
}

// _ 在 conversation 仅通过 Member.Conv 方法被引用时消除未使用 import 的警告。
var _ = conversation.NewManager

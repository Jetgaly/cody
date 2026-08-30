package teams

import (
	"context"
	"fmt"
	"os"
	"strings"

	"cody/internal/agent"
	"cody/internal/llm"
	"cody/internal/permissions"
	"cody/internal/tools"
)

// TeammateSpawnConfig 汇集 SpawnTeammate 需要的所有参数。
// 各字段在不同后端下的适用情况：
// Team / MemberName / Task / Addendum：始终使用。
// Client / Registry / Protocol：仅进程内使用；外部后端
// 会从被拉起进程自身的配置中自行加载这些。
// Workdir：可选的工作目录覆盖。非空时，
// 进程内成员会把 Agent.WorkDir 指向这里，
// tmux/iTerm spawn 也会 cd 到该路径。用于 worktree
// 隔离，避免并发队友争抢文件。
type TeammateSpawnConfig struct {
	Team       *Team
	MemberName string
	Task       string
	Addendum   string

	Client   llm.Client
	Registry *tools.Registry
	Protocol string

	Workdir string

	// Checker 是队友的权限检查器。Lead 派人时标了 plan_mode_required，
	// 这里就是一个 ModePlan 的 checker，队友只能读不能改，直到计划获批。
	Checker *permissions.Checker
}

// SpawnResult 携带 SpawnTeammate 返回的、按后端区分的句柄。进程内 spawn 得到
// 一个事件 channel；tmux/iTerm spawn 得到一个 pane 句柄，同时存到 Member.PaneID 供
// 后续拆除使用。
type SpawnResult struct {
	Mode    TeamMode
	EventCh <-chan agent.AgentEvent // 仅进程内
	PaneID  string                  // 仅 tmux/iTerm
}

// SpawnTeammate 创建一个新的团队成员，并在团队当前选定的后端（Team.Mode）下启动它。
// 它是 Agent 工具 team_name 代码路径使用的唯一入口；具体分发见下方。
//
// 对于外部后端，队友的初始任务会在新进程启动前通过 mailbox 投递，
// 这样队友在第一次空闲轮询时就能看到任务。
func SpawnTeammate(ctx context.Context, cfg TeammateSpawnConfig) (*SpawnResult, error) {
	if cfg.Team == nil {
		return nil, fmt.Errorf("SpawnTeammate: team is required")
	}
	if cfg.MemberName == "" {
		return nil, fmt.Errorf("SpawnTeammate: member name is required")
	}

	// 把成员名字登记到全局名称注册表，供 SendMessage 按名字解析投递
	GetNameRegistry().Register(cfg.MemberName, cfg.MemberName)

	switch cfg.Team.Mode {
	case ModeInProcess:
		ch := StartInProcessMember(
			ctx,
			cfg.Team,
			cfg.MemberName,
			cfg.Client,
			cfg.Registry,
			cfg.Protocol,
			cfg.Task,
			cfg.Addendum,
		)
		// Workdir 作用于刚注册成员的 Agent，使所有 file/Bash 工具都相对
		// 隔离路径解析。
		if m, ok := cfg.Team.Members[cfg.MemberName]; ok && m.AgentRef != nil {
			if cfg.Workdir != "" {
				m.AgentRef.WorkDir = cfg.Workdir
			}
			m.AgentRef.Checker = cfg.Checker
		}
		return &SpawnResult{Mode: ModeInProcess, EventCh: ch}, nil

	case ModeTmux:
		// 外部进程通过 mailbox 领取任务。在 spawn 前把初始任务放进去，
		// 这样新进程在第一次轮询时就能看到工作。
		if cfg.Task != "" {
			_ = cfg.Team.MailBox.Send(cfg.MemberName, FileMailMessage{
				From: LeadName,
				Text: cfg.Task,
			})
		}
		cliCommand, err := BuildTeammateCLI(cfg.Team.Name, cfg.MemberName, cfg.Workdir)
		if err != nil {
			return nil, err
		}
		paneID, err := spawnTmuxTeammate(cfg.Team.Name, cfg.MemberName, cliCommand)
		if err != nil {
			return nil, err
		}
		cfg.Team.recordExternalMember(cfg.MemberName, paneID)
		return &SpawnResult{Mode: ModeTmux, PaneID: paneID}, nil

	case ModeITerm:
		if cfg.Task != "" {
			_ = cfg.Team.MailBox.Send(cfg.MemberName, FileMailMessage{
				From: LeadName,
				Text: cfg.Task,
			})
		}
		cliCommand, err := BuildTeammateCLI(cfg.Team.Name, cfg.MemberName, cfg.Workdir)
		if err != nil {
			return nil, err
		}
		tabID, err := spawnITermTeammate(cfg.Team.Name, cfg.MemberName, cliCommand)
		if err != nil {
			return nil, err
		}
		cfg.Team.recordExternalMember(cfg.MemberName, tabID)
		return &SpawnResult{Mode: ModeITerm, PaneID: tabID}, nil
	}

	return nil, fmt.Errorf("unknown team mode: %s", cfg.Team.Mode)
}

// recordExternalMember 把 tmux/iTerm 拉起的队友注册到内存中的 Members map 里，
// 这样 StopMember 之后能定位它并拆除。外部队友在这边没有 AgentRef ——
// 它们的 LLM 活在被拉起的进程里 —— 所以该条目只是一个 name+handle 桩，
// 供 lead 的协调工具使用。
func (t *Team) recordExternalMember(name, paneID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Members[name] = &Member{
		Name:   name,
		Active: true,
		PaneID: paneID,
	}
}

// BuildTeammateCLI 返回一条 shell 命令，在新终端 pane/tab 中运行时，
// 会以队友模式为指定的 team/member 启动本 cody 二进制。workdir 参数控制
// 被拉起进程的运行位置；传入 "" 则回退到 lead 的当前目录，
// 使 mailbox 路径解析一致。worktree 隔离是非空 workdir 的预期用途。
//
// 输出格式与 cmd/cody/main.go 的 teammate-mode 分支解析的命令行一致。
func BuildTeammateCLI(teamName, memberName, workdir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate cody binary: %w", err)
	}
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	return fmt.Sprintf(
		"cd %s && %s --teammate --team-name %s --agent-name %s",
		shellQuote(workdir),
		shellQuote(exe),
		shellQuote(teamName),
		shellQuote(memberName),
	), nil
}

// shellQuote 包装一个值，以便安全地放进 /bin/sh -c 参数。使用单引号转义
// 是因为 tmux send-keys 和 osascript `write text` 都会通过 shell 解释该字符串。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

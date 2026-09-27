package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"cody/internal/agent"
	"cody/internal/config"
	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/mcp"
	"cody/internal/prompt"
	"cody/internal/session"
	"cody/internal/skills"
	"cody/internal/teams"
	"cody/internal/tools"
	"cody/internal/worktree"
)

// teammateArgs holds the values parsed off the CLI when this process is
// launched as a teammate worker (i.e. spawned by tmux/iTerm from
// teams.BuildTeammateCLI).
type teammateArgs struct {
	teamName   string
	memberName string
}

// parseTeammateFlags returns (args, true) when os.Args carries the
// --teammate flag, signalling teammate-worker mode. Any other value
// returns ok=false and the caller should boot the normal TUI.
//
// Format produced by teams.BuildTeammateCLI:
//
//	mewcode --teammate --team-name <t> --agent-name <n>
//
// The parsing is intentionally minimal: only the three flags this
// worker needs are recognised, and they must come as separate tokens.
func parseTeammateFlags(args []string) (teammateArgs, bool) {
	var out teammateArgs
	if len(args) == 0 || args[0] != "--teammate" {
		return out, false
	}
	i := 1
	for i < len(args) {
		switch args[i] {
		case "--team-name":
			if i+1 < len(args) {
				out.teamName = args[i+1]
				i += 2
				continue
			}
		case "--agent-name":
			if i+1 < len(args) {
				out.memberName = args[i+1]
				i += 2
				continue
			}
		}
		i++
	}
	return out, true
}

// runTeammate boots this process as a worker on an existing team. It
// loads the same config a TUI run would, builds a tools registry, then
// drops into teams.RunInProcessTeammate. The initial task is read from
// the mailbox — the lead writes it there before calling tmux/iTerm
// spawn (see teams.SpawnTeammate).
func runTeammate(args teammateArgs) error {
	if args.teamName == "" || args.memberName == "" {
		return fmt.Errorf("--teammate requires --team-name and --agent-name")
	}

	cfg, err := config.LoadConfig("")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if len(cfg.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	provider := cfg.Providers[0]

	wd, _ := os.Getwd()
	sessionID := session.NewID()

	// Worker processes get SIGINT/SIGTERM forwarded so closing the
	// pane / Ctrl-C in the tab cleanly cancels the loop and lets
	// deferred cleanup run.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Skill 目录既要进系统提示词让模型知道有哪些技能可用，也要挂到 LoadSkill
	// 工具上供模型按名字加载。
	skillCatalog := skills.LoadCatalog(wd)
	env := prompt.DetectEnvironment(wd)
	env.Model = provider.Model
	systemPrompt := prompt.BuildSystemPrompt(env, prompt.BuildOptions{
		SkillSection: buildPrintSkillSection(skillCatalog),
	})

	client, err := llm.NewClient(&provider, systemPrompt)
	if err != nil {
		return fmt.Errorf("create LLM client: %w", err)
	}

	// 队员进程直接从磁盘上的 config.json 把团队捞回来，这样它看到的队友名单
	// 和 Lead 那边是同一份。团队目录在用户主目录下，不受 worktree 换工作目录影响。
	teamMgr := teams.NewTeamManager()
	team := teamMgr.GetTeam(args.teamName)
	if team == nil {
		// 配置还没落盘（例如 Lead 刚建完团队就 spawn），退化成本地构造一份，
		// 邮箱目录按同样的约定拼出来，投递仍然对得上。
		team = teams.NewTeam(args.teamName, teams.ModeInProcess)
		teamMgr.CreateTeamWith(team)
	}

	registry := buildTeammateRegistry(ctx, teammateToolOptions{
		WorkDir:    wd,
		Protocol:   provider.Protocol,
		SessionID:  sessionID,
		TeamMgr:    teamMgr,
		TeamName:   args.teamName,
		MemberName: args.memberName,
		MCPServers: cfg.MCPServers,
	})

	member := team.AddMember(args.memberName, client, registry, provider.Protocol)

	// Skill 工具的宿主由队友自己的 Agent 担任，所以要等 AddMember 建好 Agent
	// 之后再接入。没有 ForkHost，声明 fork 模式的 skill 会退回 inline 执行。
	registry.Register(&skills.LoadSkillTool{Catalog: skillCatalog, Host: member.AgentRef})
	registry.Register(&skills.InstallSkillTool{Catalog: skillCatalog})

	addendum := teams.BuildTeammateAddendum(args.teamName, args.memberName, nil)

	// No initial prompt argument: the lead wrote the first message to
	// the mailbox before spawn, so the loop's first idle poll picks
	// it up. Passing "" prevents RunInProcessTeammate from injecting
	// a duplicate user message.
	fmt.Fprintf(os.Stderr, "[teammate %s/%s] booted, awaiting tasks\n", args.teamName, args.memberName)
	return teams.RunInProcessTeammate(ctx, team, member, "", addendum, streamEventsToStderr())
}

// teammateToolOptions 汇总组装队友工具集需要的外部依赖。
type teammateToolOptions struct {
	WorkDir    string
	Protocol   string
	SessionID  string
	TeamMgr    *teams.TeamManager
	TeamName   string
	MemberName string
	MCPServers []config.MCPServerConfig
}

// buildTeammateRegistry 组装队友工具集：文件与命令工具、工具检索、Worktree
// 切换、MCP 扩展，再加上团队协作工具（按自己的名字发消息，以及读写团队共享
// 任务板）。任务板按团队名解析到同一份 tasks.json，所以队友之间看到的是同一
// 张表。
//
// Agent 不在其中，调用树到队友这一层为止，队友不再往下派子 Agent。
// TeamCreate 与 TeamDelete 也不在其中，组建和解散团队是 Lead 的职责。
//
// Skill 工具需要 Agent 实例充当宿主，由调用方在 Agent 建好之后单独接入。
func buildTeammateRegistry(ctx context.Context, opts teammateToolOptions) *tools.Registry {
	registry := tools.CreateDefaultToolsWithWorkDir(opts.WorkDir).Registry

	registry.Register(&tools.ToolSearchTool{Registry: registry, Protocol: opts.Protocol})
	registry.Register(&tools.SyntheticOutputTool{})

	gitRoot := worktree.FindCanonicalGitRoot(opts.WorkDir)
	registry.Register(&tools.EnterWorktreeTool{SessionID: opts.SessionID, RepoRoot: gitRoot})
	registry.Register(&tools.ExitWorktreeTool{RepoRoot: gitRoot})

	registry.Register(&teams.SendMessageTool{TeamMgr: opts.TeamMgr, SenderName: opts.MemberName})
	registry.Register(&teams.TaskCreateTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName, AgentName: opts.MemberName})
	registry.Register(&teams.TaskGetTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})
	registry.Register(&teams.TaskListTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})
	registry.Register(&teams.TaskUpdateTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})

	if len(opts.MCPServers) > 0 {
		mgr := mcp.NewManager()
		serverConfigs := make([]mcp.ServerConfig, 0, len(opts.MCPServers))
		for _, c := range opts.MCPServers {
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
		mgr.RegisterAllTools(ctx, registry)
	}

	return registry
}

// streamEventsToStderr returns a channel that forwards every agent
// event to stderr in a human-readable form. Worker processes have no
// TUI; this gives the tmux/iTerm pane something visible to show.
func streamEventsToStderr() chan<- agent.AgentEvent {
	ch := make(chan agent.AgentEvent, 32)
	go func() {
		for ev := range ch {
			switch e := ev.(type) {
			case agent.StreamText:
				fmt.Fprint(os.Stderr, e.Text)
			case agent.ToolUseEvent:
				fmt.Fprintf(os.Stderr, "\n[tool %s]\n", e.ToolName)
			case agent.ToolResultEvent:
				summary := strings.TrimSpace(e.Output)
				if len(summary) > 200 {
					summary = summary[:200] + "..."
				}
				fmt.Fprintf(os.Stderr, "[result] %s\n", summary)
			case agent.ErrorEvent:
				fmt.Fprintf(os.Stderr, "[error] %s\n", e.Message)
			case agent.LoopComplete:
				fmt.Fprintf(os.Stderr, "[turn done after %d steps]\n", e.TotalTurns)
			}
		}
	}()
	return ch
}

// _ silences the unused-import warning when conversation is referenced
// only indirectly via Member.Conv.
var _ = conversation.NewManager

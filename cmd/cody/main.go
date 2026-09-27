package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"cody/internal/config"
	"cody/internal/hooks"
	"cody/internal/remote"
	"cody/internal/tui"
)

func main() {
	if args, ok := parseTeammateFlags(os.Args[1:]); ok {
		if err := runTeammate(args); err != nil {
			fmt.Fprintf(os.Stderr, "teammate: %s\n", err)
			os.Exit(1)
		}
		return
	}

	// 解析 -p/--print 和 --remote 模式
	remoteAddr := ""
	var filteredArgs []string
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--remote" {
			remoteAddr = ":18888"
			if i+1 < len(os.Args) && os.Args[i+1][0] != '-' {
				remoteAddr = os.Args[i+1]
				i++
			}
		} else {
			filteredArgs = append(filteredArgs, os.Args[i])
		}
	}

	cfg, err := config.LoadConfig("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}

	validHooks := cfg.Hooks
	if err := hooks.Validate(validHooks); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: hook configuration is invalid, starting with no hooks:\n%s\n", err)
		validHooks = nil
	}

	// -p/--print 模式：非交互式执行，输出结果后退出
	if userPrompt, outputFormat, ok := parsePrintFlags(os.Args[1:]); ok {
		if userPrompt == "" {
			// 从 stdin 读取 prompt
			buf, _ := os.ReadFile("/dev/stdin")
			userPrompt = string(buf)
		}
		if userPrompt == "" {
			fmt.Fprintf(os.Stderr, "Error: -p requires a prompt argument or stdin input\n")
			os.Exit(1)
		}
		if err := runPrint(userPrompt, cfg, validHooks, outputFormat); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
			os.Exit(1)
		}
		return
	}

	if remoteAddr != "" {
		srv := remote.NewServer(cfg.Providers, cfg.MCPServers, validHooks, remoteAddr, cfg.EnableCoordinatorMode, !cfg.ForkEnabled())
		if err := srv.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Remote server error: %s\n", err)
			os.Exit(1)
		}
		return
	}

	m := tui.New(cfg.Providers, cfg.MCPServers, validHooks, cfg.Sandbox)
	m.EnableCoordinatorMode = cfg.EnableCoordinatorMode
	m.ForkDisabled = !cfg.ForkEnabled()
	p := tea.NewProgram(m)

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}


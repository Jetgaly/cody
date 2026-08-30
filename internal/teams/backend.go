package teams

import (
	"os"
	"runtime"
)

// detectBackend：只有已身处 tmux / iTerm2 会话时才用窗格后端，
// 否则回退进程内。判断依据是环境变量——tmux 和 iTerm2 会自动给会话内的进程设上
// TMUX / ITERM_SESSION_ID，用户无需手动配置。
// DetectBackend 暴露给外部包用：Agent 工具在指定的 Team 不存在时会自己建，
// 建的时候要按当前环境选后端。
func DetectBackend() TeamMode { return detectBackend() }

func detectBackend() TeamMode {
	// Windows 护栏：tmux 窗格 spawn 时用 pwsh 执行 POSIX 命令会失败，一律进程内。
	if runtime.GOOS == "windows" {
		return ModeInProcess
	}
	return detectBackendFromEnv()
}

// detectBackendFromEnv 只按环境变量判断，抽出来便于单测（不受运行平台影响）。
func detectBackendFromEnv() TeamMode {
	if os.Getenv("TMUX") != "" {
		return ModeTmux
	}
	if os.Getenv("ITERM_SESSION_ID") != "" {
		return ModeITerm
	}
	return ModeInProcess
}

package teams

import (
	"runtime"
	"testing"
)

// detectBackendFromEnv 只看环境变量，与运行平台无关，可在任意系统上验证。
func TestDetectBackendFromEnv(t *testing.T) {
	t.Run("inside tmux picks tmux", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/sock,1,0")
		t.Setenv("ITERM_SESSION_ID", "")
		if got := detectBackendFromEnv(); got != ModeTmux {
			t.Errorf("got %q, want tmux", got)
		}
	})
	t.Run("inside iterm2 picks iterm", func(t *testing.T) {
		t.Setenv("TMUX", "")
		t.Setenv("ITERM_SESSION_ID", "w0t0p0:ABC")
		if got := detectBackendFromEnv(); got != ModeITerm {
			t.Errorf("got %q, want iterm", got)
		}
	})
	t.Run("tmux wins over iterm", func(t *testing.T) {
		t.Setenv("TMUX", "/tmp/sock,1,0")
		t.Setenv("ITERM_SESSION_ID", "w0t0p0:ABC")
		if got := detectBackendFromEnv(); got != ModeTmux {
			t.Errorf("got %q, want tmux", got)
		}
	})
	t.Run("plain terminal falls back to in-process", func(t *testing.T) {
		t.Setenv("TMUX", "")
		t.Setenv("ITERM_SESSION_ID", "")
		if got := detectBackendFromEnv(); got != ModeInProcess {
			t.Errorf("got %q, want in-process", got)
		}
	})
}

// Windows 上无论是否在 tmux 会话里都必须走进程内（护栏），避免 pwsh spawn 失败。
func TestDetectBackendWindowsGuard(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	got := detectBackend()
	if runtime.GOOS == "windows" {
		if got != ModeInProcess {
			t.Errorf("windows must stay in-process even inside tmux, got %q", got)
		}
	} else {
		// 非 Windows 平台，身处 tmux 会话应选 tmux
		if got != ModeTmux {
			t.Errorf("non-windows inside tmux should pick tmux, got %q", got)
		}
	}
}

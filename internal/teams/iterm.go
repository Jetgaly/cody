package teams

import (
	"fmt"
	"os/exec"
	"strings"
)

// ModeITerm 通过 AppleScript 把每个队友放到独立的 iTerm2 标签页中。
// 仅限 macOS；detectBackend 在设置了 ITERM_SESSION_ID 时会选择它。
const ModeITerm TeamMode = "iterm"

// spawnITermTeammate 打开一个新的 iTerm2 标签页并在其中运行 cliCommand。
// 返回脚本侧的标签标识符（"team-member"），调用方可以
// 在之后用它来定位并关闭。
func spawnITermTeammate(teamName, memberName, cliCommand string) (string, error) {
	tabName := fmt.Sprintf("%s-%s", teamName, memberName)
	// 转义任何内嵌的双引号，保证 AppleScript 字符串字面量仍然合法。
	safeCmd := strings.ReplaceAll(cliCommand, `"`, `\"`)
	safeName := strings.ReplaceAll(tabName, `"`, `\"`)
	script := fmt.Sprintf(`tell application "iTerm2"
  tell current window
    set newTab to create tab with default profile
    tell newTab
      set name to "%s"
      tell current session
        write text "%s"
      end tell
    end tell
  end tell
end tell`, safeName, safeCmd)

	cmd := exec.Command("osascript", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("osascript: %s: %s", err, strings.TrimSpace(string(out)))
	}
	return tabName, nil
}

// stopITermTeammate 关闭 spawnITermTeammate 创建的 iTerm2 标签页。
// 尽力而为：标签页不存在 / 窗口已关闭都不会报错。
func stopITermTeammate(tabName string) {
	safeName := strings.ReplaceAll(tabName, `"`, `\"`)
	script := fmt.Sprintf(`tell application "iTerm2"
  repeat with w in windows
    repeat with t in tabs of w
      if name of t is "%s" then
        tell t to close
      end if
    end repeat
  end repeat
end tell`, safeName)
	exec.Command("osascript", "-e", script).Run()
}


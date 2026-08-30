package teams

import (
	"fmt"
	"os/exec"
	"strings"
)

func spawnTmuxTeammate(teamName, memberName, cliCommand string) (string, error) {
	paneName := fmt.Sprintf("%s-%s", teamName, memberName)

	// 为队员创建一个新的 tmux 窗口（不拆分）
	cmd := exec.Command("tmux", "new-window", "-d", "-n", paneName, cliCommand)
	output, err := cmd.CombinedOutput()

	if err != nil {
		return "", fmt.Errorf("tmux new-window: %s: %s", err, strings.TrimSpace(string(output)))
	}
	return paneName, nil
}

func stopTmuxTeammate(paneName string) {
	exec.Command("tmux", "send-keys", "-t", paneName, "C-c", "").Run()
	exec.Command("tmux", "kill-window", "-t", paneName).Run()
}


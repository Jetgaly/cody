package worktree

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

// gitNoPromptEnv 返回本包派生的每个 git 子进程的基础环境，并追加两个安全开关：
//
// GIT_TERMINAL_PROMPT=0：阻止 git 打开 /dev/tty 进行凭据提示
// （否则会挂起 CLI）。
// GIT_ASKPASS=""：禁用 askpass GUI 程序（通过另一条代码路径达到同样效果）。
//
// 结合 *exec.Cmd 上的 Stdin = nil，这关闭了 git 可能因交互输入而阻塞的每一条通道。
func gitNoPromptEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
}

// runGit 在 dir 内以 stdin 关闭并应用 no-prompt 环境调用 `git <args.>`。
// 返回 stdout、stderr 和退出码（进程未运行时为 -1）。非零退出不会抛错；
// 由调用方根据上下文决定 code != 0 是否算错误。
//
// ctx 传递取消信号：取消 ctx 会杀掉 git 子进程。
func runGit(ctx context.Context, dir string, args ...string) (stdout, stderr string, code int) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitNoPromptEnv()
	cmd.Stdin = nil
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	if err == nil {
		return stdout, stderr, 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return stdout, stderr, ee.ExitCode()
	}
	// 进程启动失败（git 不在 PATH 上、目录不存在等）。
	return stdout, stderr, -1
}


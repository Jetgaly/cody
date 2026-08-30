package commands

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CommandMeta 是基于文件的 prompt 命令的 frontmatter。保存为传统
// /commands/ 文件读取的字段子集：description、argument-hint、aliases。
type CommandMeta struct {
	Description  string   `yaml:"description"`
	ArgumentHint string   `yaml:"argument-hint"`
	Aliases      []string `yaml:"aliases"`
}

// LoadDir 扫描 dir 下的 *.md 文件（递归）并每个文件返回一个 Command。命令名
// 由文件相对 dir 的路径派生，子目录用 ':' 连接，遵循原有
// 命名空间规则（sub/dir/foo.md → "sub:dir:foo"）。解析失败的文件会被静默跳过
// （一个格式错误的用户命令不应导致启动失败）。
//
// 返回的每个命令 Type=TypePrompt，Handler 返回替换 $ARGUMENTS 后的 markdown 正文。
// 若正文没有 $ARGUMENTS 占位符且 args 非空，参数会被追加到 "## User Request" 一节。
func LoadDir(dir string) []*Command {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}

	var cmds []*Command
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".md") {
			return nil
		}
		cmd := parseCommandFile(dir, path)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		return nil
	})
	return cmds
}

// LoadUserCommands 合并两个搜索路径下的文件命令：1. ~/.cody/commands/（用户
// 全局）2. $workDir/.cody/commands/（项目）。
//
// 名称冲突时，后加载的源覆盖先加载的源。
func LoadUserCommands(workDir string) []*Command {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".cody", "commands"))
	}
	dirs = append(dirs,
		filepath.Join(workDir, ".cody", "commands"),
	)

	merged := map[string]*Command{}
	var order []string
	for _, d := range dirs {
		for _, cmd := range LoadDir(d) {
			if _, seen := merged[cmd.Name]; !seen {
				order = append(order, cmd.Name)
			}
			merged[cmd.Name] = cmd
		}
	}

	out := make([]*Command, 0, len(order))
	for _, name := range order {
		out = append(out, merged[name])
	}
	return out
}

// parseCommandFile 读取单个 .md 文件并返回对应的 Command，读取/解析失败时返回 nil。
// 名称由相对路径计算：baseDir 下的 "git/log.md" → "git:log"。
// 名称转为小写以匹配 Parse 中的 /<name> 查找约定。
func parseCommandFile(baseDir, path string) *Command {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return nil
	}
	rel = strings.TrimSuffix(rel, ".md")
	parts := strings.Split(rel, string(filepath.Separator))
	for i, p := range parts {
		parts[i] = strings.ToLower(strings.ReplaceAll(p, " ", "-"))
	}
	name := strings.Join(parts, ":")
	if name == "" {
		return nil
	}

	meta, body := splitFrontmatter(string(data))
	body = strings.TrimSpace(body)
	if meta.Description == "" {
		meta.Description = firstNonHeaderLine(body)
	}

	return &Command{
		Name:        name,
		Description: meta.Description,
		Aliases:     meta.Aliases,
		Type:        TypePrompt,
		ArgPrompt:   meta.ArgumentHint,
		Handler:     promptHandler(body),
	}
}

// splitFrontmatter 将 YAML frontmatter 与 markdown 正文分离。没有 frontmatter
// 或解析失败时返回空的 meta 和原始内容——格式错误的命令文件
// 不应导致启动失败。
func splitFrontmatter(content string) (CommandMeta, string) {
	var meta CommandMeta
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return meta, content
	}
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return meta, content
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &meta); err != nil {
		return CommandMeta{}, content
	}
	return meta, parts[2]
}

// firstNonHeaderLine 返回第一行非空、非标题的行——当 frontmatter
// 没有提供描述时用作描述回退。
func firstNonHeaderLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// promptHandler 返回一个 Handler，渲染命令正文并进行 $ARGUMENTS 替换。
// 没有占位符的正文把 args 追加到 "## User Request" 一节。
func promptHandler(body string) Handler {
	return func(ctx *Context) string {
		if strings.Contains(body, "$ARGUMENTS") {
			return strings.ReplaceAll(body, "$ARGUMENTS", ctx.Args)
		}
		if strings.TrimSpace(ctx.Args) == "" {
			return body
		}
		return body + "\n\n## User Request\n\n" + ctx.Args
	}
}

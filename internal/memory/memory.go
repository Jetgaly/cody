package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Manager 封装双份自动记忆目录（用户级 + 项目级）。
// 它是一个轻量协调者：实际的保存/加载经由 agent 的 Write/Read 工具完成
// （遵循 Cody 的参考架构）。这个结构体存在的目的是给 TUI 一个稳定句柄，
// 用于构建系统提示词以及实现 `/memory` 斜杠命令（list / clear）。
type Manager struct {
	projectRoot string
	userMemDir  string // ~/.cody/memory/ — 用户/反馈类记忆
	memDir      string // <projectRoot>/.cody/memory/ — 项目/参考类记忆
}

// NewManager 为给定的项目根目录创建一个 Manager。同时解析用户级和项目级
// 记忆目录。两者都可能为空（例如未设置 $HOME 时用户级会解析为空）。
func NewManager(projectRoot string) *Manager {
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		abs = projectRoot
	}
	return &Manager{
		projectRoot: abs,
		userMemDir:  GetUserAutoMemPath(),
		memDir:      GetAutoMemPath(abs),
	}
}

// Dir 返回项目级记忆目录（带尾部分隔符）。
// 为只关心项目范围状态的调用方保留。
func (m *Manager) Dir() string {
	return m.memDir
}

// UserDir 返回用户级记忆目录（带尾部分隔符）。
func (m *Manager) UserDir() string {
	return m.userMemDir
}

// EntrypointPath 返回项目级 MEMORY.md 的绝对路径。
func (m *Manager) EntrypointPath() string {
	return filepath.Join(m.memDir, AutoMemEntrypointName)
}

// UserEntrypointPath 返回用户级 MEMORY.md 的绝对路径。
func (m *Manager) UserEntrypointPath() string {
	if m.userMemDir == "" {
		return ""
	}
	return filepath.Join(m.userMemDir, AutoMemEntrypointName)
}

// BuildSystemReminder 返回可直接放入系统提示词的 `# auto memory` 段落。
// 确保两个目录都已存在，这样 agent 无需先执行 mkdir 即可向任一个目录写入。
func (m *Manager) BuildSystemReminder() string {
	if m.memDir == "" && m.userMemDir == "" {
		return ""
	}
	if m.userMemDir != "" {
		_ = EnsureMemoryDirExists(m.userMemDir)
	}
	if m.memDir != "" {
		_ = EnsureMemoryDirExists(m.memDir)
	}
	return BuildMemoryPrompt(autoMemDisplayName, m.userMemDir, m.memDir)
}

// MemoryFile 描述一条已保存的记忆。
type MemoryFile struct {
	Path        string
	Name        string
	Description string
	Type        MemoryType
}

// GetMemories 返回记忆目录中每个记忆文件的一行摘要。
// 供 `/memory list` 斜杠命令使用。顺序是稳定的（按文件名排序）。
func (m *Manager) GetMemories() []string {
	files := m.LoadAll()
	out := make([]string, 0, len(files))
	for _, f := range files {
		typeTag := string(f.Type)
		if typeTag == "" {
			typeTag = "?"
		}
		desc := f.Description
		if desc == "" {
			desc = filepath.Base(f.Path)
		}
		out = append(out, fmt.Sprintf("[%s] %s — %s", typeTag, f.Name, desc))
	}
	return out
}

// LoadAll 扫描用户级和项目级记忆目录中的 *.md 文件（不包括 MEMORY.md），
// 并返回每个文件的已解析 frontmatter。用户级文件在前，然后是项目级文件。
func (m *Manager) LoadAll() []MemoryFile {
	out := loadDir(m.userMemDir)
	out = append(out, loadDir(m.memDir)...)
	return out
}

func loadDir(dir string) []MemoryFile {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var out []MemoryFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == AutoMemEntrypointName || !strings.HasSuffix(name, ".md") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		mf := parseFrontmatter(string(data))
		mf.Path = path
		if mf.Name == "" {
			mf.Name = strings.TrimSuffix(name, ".md")
		}
		out = append(out, mf)
	}
	return out
}

// Clear 删除两个记忆目录中的所有 *.md 文件（包括 MEMORY.md）。
// 供 `/memory clear` 斜杠命令使用。
func (m *Manager) Clear() {
	clearDir(m.userMemDir)
	clearDir(m.memDir)
}

func clearDir(dir string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

var frontmatterRe = regexp.MustCompile(`(?s)\A---\s*\n(.*?)\n---\s*\n`)

// parseFrontmatter 从类 YAML 的 frontmatter 中提取 name/description/type。
// 只读取这三个已知字段；其余内容（包括未知字段以及引号处理等完整 YAML 语义）全部忽略。
// 没有 frontmatter 的文件也能优雅降级——字段为空，正文就是整个文件。
func parseFrontmatter(content string) MemoryFile {
	var mf MemoryFile
	m := frontmatterRe.FindStringSubmatch(content)
	if m == nil {
		return mf
	}
	for _, line := range strings.Split(m[1], "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colon])
		val := strings.TrimSpace(line[colon+1:])
		val = strings.Trim(val, `"'`)
		switch key {
		case "name":
			mf.Name = val
		case "description":
			mf.Description = val
		case "type":
			if t, ok := ParseMemoryType(val); ok {
				mf.Type = t
			}
		}
	}
	return mf
}

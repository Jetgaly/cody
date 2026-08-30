package agents

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentMemoryScope 持久记忆的位置：按用户、按项目、或按 checkout（不纳入版本
// 控制）。
type AgentMemoryScope string

const (
	AgentMemoryScopeUser    AgentMemoryScope = "user"
	AgentMemoryScopeProject AgentMemoryScope = "project"
	AgentMemoryScopeLocal   AgentMemoryScope = "local"
)

// IsolationMode 编码 `isolation` frontmatter 字段。
type IsolationMode string

const (
	IsolationWorktree IsolationMode = "worktree"
	IsolationRemote   IsolationMode = "remote"
)

// AgentDefinition 目前还没有运行时用途的字段（Effort、Skills、McpServers、Hooks、Memory、
// InitialPrompt、OmitCodyMd、RequiredMcpServers）仍然会被解析，这样用户定义在往返过程中不会丢失
// 数据，未来也能在不做另一次 schema 迁移的情况下读取它们。
type AgentDefinition struct {
	AgentType       string   `yaml:"name"`
	WhenToUse       string   `yaml:"description"`
	Tools           []string `yaml:"tools"`
	DisallowedTools []string `yaml:"disallowedTools"`
	Model           string   `yaml:"model"`
	MaxTurns        int      `yaml:"maxTurns"`

	// permissionMode 覆盖父 agent 给这个子 agent 设置的权限模式。合法取值
	// 与 internal/permissions.PermissionMode 一致。
	PermissionMode string `yaml:"permissionMode"`

	// Effort 是给模型的关于任务复杂度的提示（"low" | "medium" | "high" | int）。目前
	// 只存储，还未被消费。
	Effort any `yaml:"effort"`

	// Skills 是子 agent 启动时要预加载的 skill 名称。
	Skills []string `yaml:"skills"`

	// McpServers 是作用域限定于此 agent 的 MCP server 名称或内联配置。以原始 any 存储，这样
	// 未来加载时可以解释字符串引用或内联配置。
	McpServers []any `yaml:"mcpServers"`

	// RequiredMcpServers 是 agent 的准入条件：如果列出的 server 在加载时不可用，该 agent
	// 会被 hasRequiredMcpServers 过滤掉。
	RequiredMcpServers []string `yaml:"requiredMcpServers"`

	// Hooks 是此 agent 启动时注册的、会话作用域的钩子。以原始 YAML 存储；hooks
	// 包会在消费时做类型检查。
	Hooks any `yaml:"hooks"`

	// Memory 启用三种作用域之一的持久记忆。
	Memory AgentMemoryScope `yaml:"memory"`

	// Background 强制此 agent 在被生成时总是作为后台任务运行，无论
	// run_in_background 参数如何。
	Background bool `yaml:"background"`

	// Isolation 为生成（spawn）选择文件系统隔离模式。
	Isolation IsolationMode `yaml:"isolation"`

	// InitialPrompt 会前置到第一个用户回合（斜杠命令可用）。
	InitialPrompt string `yaml:"initialPrompt"`

	// OmitCodyMd 会从这个 agent 的用户上下文中去掉 CODY.md 层级。只读型 agent
	// （Explore、Plan）通过跳过它来节省 token。
	OmitCodyMd bool `yaml:"omitCodyMd"`

	// SystemPrompt 是定义文件的 Markdown 正文。
	SystemPrompt string `yaml:"-"`

	// FilePath / Source / Filename 在加载时填充。
	FilePath string `yaml:"-"`
	Source   string `yaml:"-"`
	Filename string `yaml:"-"`
}

// validPermissionModes 是 Agent 定义里 permission_mode 字段的合法取值，空串表示不覆盖、
// 沿用父 Agent 的模式。
var validPermissionModes = map[string]bool{
	"":                  true,
	"acceptEdits":       true,
	"bypassPermissions": true,
	"default":           true,
	"plan":              true,
}

var validMemoryScopes = map[AgentMemoryScope]bool{
	"":                      true,
	AgentMemoryScopeUser:    true,
	AgentMemoryScopeProject: true,
	AgentMemoryScopeLocal:   true,
}

var validIsolationModes = map[IsolationMode]bool{
	"":                true,
	IsolationWorktree: true,
	IsolationRemote:   true,
}

func ParseAgentFile(path string) (*AgentDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	var def AgentDefinition
	def.FilePath = path

	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			if err := yaml.Unmarshal([]byte(parts[1]), &def); err != nil {
				return nil, fmt.Errorf("parse frontmatter in %s: %w", path, err)
			}
			def.SystemPrompt = strings.TrimSpace(parts[2])
		}
	} else {
		def.SystemPrompt = strings.TrimSpace(content)
	}

	if def.AgentType == "" {
		return nil, fmt.Errorf("agent definition %s: missing required field 'name'", path)
	}
	if def.WhenToUse == "" {
		return nil, fmt.Errorf("agent definition %s: missing required field 'description'", path)
	}

	// 规范化并校验 `model`。与 AgentJsonSchema 一致：只要求“必须是非空字符串”——
	// 实际可用性交给主机的 ModelResolver / LLM router。第三方模型名
	// 如 "glm-5.1" 必须能原样往返。小写 "inherit" 会规范化为 "inherit"（这个哨兵值
	// 表示“使用父级的 client”）；其余值保持原样，便于 router 匹配。
	def.Model = strings.TrimSpace(def.Model)
	if strings.EqualFold(def.Model, "inherit") {
		def.Model = "inherit"
	}

	if !validPermissionModes[def.PermissionMode] {
		return nil, fmt.Errorf("agent definition %s: invalid permissionMode '%s'", path, def.PermissionMode)
	}

	if !validMemoryScopes[def.Memory] {
		return nil, fmt.Errorf("agent definition %s: invalid memory scope '%s'", path, def.Memory)
	}

	if !validIsolationModes[def.Isolation] {
		return nil, fmt.Errorf("agent definition %s: invalid isolation mode '%s'", path, def.Isolation)
	}

	return &def, nil
}

func (d *AgentDefinition) ToSpec() SubAgentSpec {
	return SubAgentSpec{
		Name:                 d.AgentType,
		Description:          d.WhenToUse,
		Tools:                d.Tools,
		DisallowedTools:      d.DisallowedTools,
		SystemPromptOverride: d.SystemPrompt,
		MaxTurns:             d.MaxTurns,
		Model:                d.Model,
		PermissionMode:       d.PermissionMode,
		Background:           d.Background,
		Isolation:            d.Isolation,
		InitialPrompt:        d.InitialPrompt,
		OmitCodyMd:           d.OmitCodyMd,
		Skills:               d.Skills,
		Memory:               d.Memory,
		McpServers:           d.McpServers,
		RequiredMcpServers:   d.RequiredMcpServers,
		Hooks:                d.Hooks,
		Effort:               d.Effort,
	}
}

// HasRequiredMcpServers 返回 true 表示该 agent 没有 MCP 需求，或者每个必需的
// pattern 都能匹配到可用的 server 名称（不区分大小写的子串匹配）。
func (d *AgentDefinition) HasRequiredMcpServers(availableServers []string) bool {
	if len(d.RequiredMcpServers) == 0 {
		return true
	}
	for _, pattern := range d.RequiredMcpServers {
		patLower := strings.ToLower(pattern)
		matched := false
		for _, server := range availableServers {
			if strings.Contains(strings.ToLower(server), patLower) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}


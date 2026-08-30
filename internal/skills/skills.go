package skills

import (
	"strings"
)

type SkillMeta struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	WhenToUse   string   `yaml:"when_to_use"`
	Tags        []string `yaml:"tags"`
	// Mode 选择执行模式。"inline"（默认）把 skill 正文注入当前
	// 对话；"fork" 在具有隔离上下文的子 agent 中运行 skill 正文。
	Mode string `yaml:"mode"`
	// Model 覆盖此 skill 使用的 LLM。为空 = 继承主循环的模型。
	Model string `yaml:"model"`
	// Context 是为了与旧的、使用 `context: fork` 表示 Mode=fork 的 skill 向后兼容而保留。
	// 当值 == "fork" 时按 fork 模式处理。
	Context string `yaml:"context"`
	// ForkContext 控制把多少父对话带入被 fork 的子 agent。
	// 仅在 Mode == "fork" 时才有意义。取值："full"（父对话的 LLM 摘要）、"recent"（最近 5
	// 条消息）、"none"（无父上下文，默认）。
	ForkContext string `yaml:"fork_context"`
}

// IsFork 报告该 skill 是否应以 fork 模式运行。同时检查 Mode 和遗留的 Context
// 字段以实现向后兼容。
func (m SkillMeta) IsFork() bool {
	return m.Mode == "fork" || m.Context == "fork"
}

type Skill struct {
	Meta       SkillMeta
	PromptBody string
	SourceDir  string
	// IsDirectory 标记那些 SourceDir 包含额外资源（references/、scripts/）的 skill。
	// 对于目录型 skill（磁盘上 SKILL.md 旁有配套文件）为 true。
	// 仅对运行时在磁盘上没有真实目录可访问的内嵌 skill 为 false。
	IsDirectory bool
	// BodyLoaded 标记 PromptBody 是否已从磁盘读取。阶段一加载只读取
	// frontmatter；正文保持为空，直到 GetFull 触发读取。
	BodyLoaded bool
}

// Render 返回替换了 $ARGUMENTS 的 skill 正文。如果正文中没有 $ARGUMENTS
// 占位符且 args 非空，args 会被追加到一个 "## User Request" 段落中。
//
// 执行方式由 Meta.Mode 决定，不体现在渲染结果里：inline 的正文直接进主对话，
// fork 的正文由调用方交给隔离子 Agent，两条路径拿到的都是这里渲染出的同一份文本。
func (s *Skill) Render(args string) string {
	body := s.PromptBody
	if strings.Contains(body, "$ARGUMENTS") {
		return strings.ReplaceAll(body, "$ARGUMENTS", args)
	}
	if strings.TrimSpace(args) == "" {
		return body
	}
	return body + "\n\n## User Request\n\n" + args
}


package skills

import (
	"context"
	"fmt"

	"cody/internal/tools"
)

// LoadSkillTool 是按需激活 skill 的入口。它在启动时以渐进式披露的语义
// 注册进主工具注册表：模型在系统提示词里看到 `## Available Skills` 列出的
// 每个 skill 的名称 + 描述，然后以所选名称调用 LoadSkill。
// 完整的 SOP 正文作为工具结果返回，从而以普通消息的形式进入对话。
type LoadSkillTool struct {
	Catalog *Catalog
	Host    SkillHost
	// ForkHost 提供运行隔离子 Agent 的能力，声明了 mode: fork 的 skill 依赖它。
	// 为 nil 时（宿主未接入子 Agent 运行时）回退成 inline，保证工具在任何宿主上都能用。
	ForkHost SkillForkHost
}

func (t *LoadSkillTool) Name() string { return "LoadSkill" }

func (t *LoadSkillTool) Category() tools.ToolCategory { return tools.CategoryRead }

func (t *LoadSkillTool) Description() string {
	return "Activate a Skill by name. Returns the full SOP body so you can follow its " +
		"instructions. Call this when the user's request matches one of the available " +
		"Skills listed in the system prompt. Pass the Skill name without a leading slash."
}

func (t *LoadSkillTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "The Skill name to activate (e.g. \"commit\", \"backend-interview\").",
				},
			},
			"required": []string{"name"},
		},
	}
}

func (t *LoadSkillTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	name, _ := args["name"].(string)
	if name == "" {
		return tools.ToolResult{Output: "name is required", IsError: true}
	}
	if t.Catalog == nil || t.Host == nil {
		return tools.ToolResult{Output: "LoadSkill not wired (Catalog or Host nil)", IsError: true}
	}
	skill, err := t.Catalog.GetFull(name)
	if err != nil && skill == nil {
		return tools.ToolResult{Output: fmt.Sprintf("unknown skill: %s", name), IsError: true}
	}
	if skill.PromptBody == "" {
		return tools.ToolResult{Output: fmt.Sprintf("skill %q has empty body — cannot activate", name), IsError: true}
	}

	// fork 模式：SOP 正文不进主对话，丢给隔离的子 Agent 执行，只把最终结果带回。
	// 这样模型自己加载 skill 和用户敲斜杠命令遵循同一套 mode 语义，声明的隔离
	// 意图在两条路径上都生效。
	if skill.Meta.IsFork() && t.ForkHost != nil {
		result, err := RunFork(ctx, skill, "", t.ForkHost)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("skill %q fork execution failed: %v", name, err),
				IsError: true,
			}
		}
		return tools.ToolResult{Output: result}
	}

	t.Host.ActivateSkill(skill.Meta.Name, skill.PromptBody)

	header := fmt.Sprintf("# Skill: %s\n\n", skill.Meta.Name)
	return tools.ToolResult{
		Output: header + skill.PromptBody,
	}
}

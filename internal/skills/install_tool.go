package skills

import (
	"context"
	"fmt"

	"cody/internal/tools"
)

// InstallSkillTool 让模型按需从用户提供的 skills.sh / github.com URL
// 安装一个新 Skill。
//
// OnInstalled 回调在安装成功后触发，携带新技能的名称；
// TUI 用它重新注册斜杠命令，这样 `/<new-skill>` 无需重启即可生效。
type InstallSkillTool struct {
	Catalog     *Catalog
	OnInstalled func(name string)
	// InstallRoot 供测试时覆盖 ~/.cody/skills。为空则
	// 在调用时从 UserSkillsRoot 推导。
	InstallRoot string
}

func (t *InstallSkillTool) Name() string                 { return "InstallSkill" }

func (t *InstallSkillTool) Category() tools.ToolCategory { return tools.CategoryWrite }

func (t *InstallSkillTool) Description() string {
	return "Download and install a Skill from a URL into the user-global skills directory " +
		"(~/.cody/skills/). Supports skills.sh URLs (https://www.skills.sh/<owner>/<repo>/<name>), " +
		"GitHub tree URLs (https://github.com/<owner>/<repo>/tree/<ref>/<path>), and raw " +
		"SKILL.md URLs. After install the Skill becomes available via /<name> and LoadSkill. " +
		"Call this when the user pastes a Skill URL and asks to install it."
}

func (t *InstallSkillTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The Skill URL to fetch. Examples: \"https://www.skills.sh/anthropics/skills/frontend-design\", \"https://github.com/anthropics/skills/tree/main/skills/pdf\".",
				},
			},
			"required": []string{"url"},
		},
	}
}

func (t *InstallSkillTool) Execute(_ context.Context, args map[string]any) tools.ToolResult {
	rawURL, _ := args["url"].(string)
	if rawURL == "" {
		return tools.ToolResult{Output: "url is required", IsError: true}
	}
	src, err := ParseSkillURL(rawURL)
	if err != nil {
		return tools.ToolResult{Output: err.Error(), IsError: true}
	}

	root := t.InstallRoot
	if root == "" {
		r, err := UserSkillsRoot()
		if err != nil {
			return tools.ToolResult{Output: err.Error(), IsError: true}
		}
		root = r
	}

	report, err := Install(src, root)
	if err != nil {
		return tools.ToolResult{Output: fmt.Sprintf("install failed: %v", err), IsError: true}
	}

	// 刷新 catalog，让新技能的 frontmatter 被索引，
	// 无需重启即可通过 LoadSkill 访问。
	if t.Catalog != nil {
		t.Catalog.Reload(t.Catalog.workDir)
	}
	if t.OnInstalled != nil {
		t.OnInstalled(report.SkillName)
	}

	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Installed skill %q from %s into %s (%d files, %d bytes). Now available — call LoadSkill({name: %q}) or invoke /%s directly.",
			report.SkillName, src.Original, report.TargetDir, report.FileCount, report.TotalBytes, report.SkillName, report.SkillName,
		),
	}
}

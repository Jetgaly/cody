package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// parseFrontmatterOnly 执行第一阶段的加载：只读取技能文件里足以提取 SkillMeta 的部分，
// 让 PromptBody 保持为空。开销足够小，可在启动时为数百个技能运行。
//
// 支持两种布局：
//   - <dir>/skill.yaml（+ 可选的 prompt.md，第一阶段忽略）
//   - <dir>/SKILL.md，带 `---` YAML frontmatter
func parseFrontmatterOnly(dir string) (*Skill, error) {
	yamlPath := filepath.Join(dir, "skill.yaml")
	if data, err := os.ReadFile(yamlPath); err == nil {
		var meta SkillMeta
		if err := yaml.Unmarshal(data, &meta); err != nil {
			return nil, fmt.Errorf("parse skill.yaml: %w", err)
		}
		applyMetaDefaults(&meta, dir, "")
		return &Skill{
			Meta:        meta,
			SourceDir:   dir,
			IsDirectory: true,
			BodyLoaded:  false,
		}, nil
	}

	mdPath := filepath.Join(dir, "SKILL.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return nil, fmt.Errorf("no skill.yaml or SKILL.md: %w", err)
	}
	meta, _ := splitFrontmatter(string(data))
	applyMetaDefaults(&meta, dir, string(data))
	return &Skill{
		Meta:        meta,
		SourceDir:   dir,
		IsDirectory: true,
		BodyLoaded:  false,
	}, nil
}

// loadSkillBody 读取已解析完 frontmatter 的技能的正文。
// 每次技能被调用时（热重载）由 Catalog.GetFull 调用。
// 遇到任何读取/解析错误时，保持现有的 PromptBody 不变，并返回错误，
// 以便调用方回退到缓存的版本。
func loadSkillBody(skill *Skill) error {
	yamlPath := filepath.Join(skill.SourceDir, "skill.yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		promptPath := filepath.Join(skill.SourceDir, "prompt.md")
		body, err := os.ReadFile(promptPath)
		if err != nil {
			return fmt.Errorf("read prompt.md: %w", err)
		}
		skill.PromptBody = string(body)
		skill.BodyLoaded = true
		return nil
	}

	mdPath := filepath.Join(skill.SourceDir, "SKILL.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return fmt.Errorf("read SKILL.md: %w", err)
	}
	_, body := splitFrontmatter(string(data))
	skill.PromptBody = body
	skill.BodyLoaded = true
	return nil
}

// loadSkillFromBytes 从内存字节解析技能（用于 go:embed 内建技能，
// 此时没有磁盘目录可供重新读取）。
func loadSkillFromBytes(name string, mdBytes []byte) (*Skill, error) {
	meta, body := splitFrontmatter(string(mdBytes))
	if meta.Name == "" {
		meta.Name = name
	}
	applyMetaDefaults(&meta, name, string(mdBytes))
	return &Skill{
		Meta:        meta,
		PromptBody:  body,
		SourceDir:   "", // 内嵌资源——没有源目录
		IsDirectory: false,
		BodyLoaded:  true,
	}, nil
}

// splitFrontmatter 将 YAML frontmatter 与 markdown 正文分开。
// 如果不存在 `---` frontmatter，则返回零值 meta。
func splitFrontmatter(content string) (SkillMeta, string) {
	var meta SkillMeta
	body := content

	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			if err := yaml.Unmarshal([]byte(parts[1]), &meta); err == nil {
				body = strings.TrimSpace(parts[2])
			}
		}
	}
	return meta, body
}

// applyMetaDefaults 按照旧的 parseSkillMD 的方式填充 name/description 的缺省值：
//   - 缺少 name → 从目录名派生（转小写 + kebab 风格）
//   - 缺少 description → 取正文中第一行非空且非标题的行
//   - 缺少 Mode → "inline"
//   - 缺少 ForkContext → "none"（仅在 Mode == "fork" 时相关）
func applyMetaDefaults(meta *SkillMeta, dirOrName, body string) {
	if meta.Name == "" {
		base := filepath.Base(dirOrName)
		meta.Name = strings.ToLower(strings.ReplaceAll(base, " ", "-"))
	}
	if meta.Description == "" && body != "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "---") {
				meta.Description = line
				break
			}
		}
	}
	if meta.Mode == "" {
		if meta.Context == "fork" {
			meta.Mode = "fork"
		} else {
			meta.Mode = "inline"
		}
	}
	if meta.IsFork() && meta.ForkContext == "" {
		meta.ForkContext = "none"
	}
}

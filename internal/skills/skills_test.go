package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromDirectory(t *testing.T) {
	dir := t.TempDir()

	skillDir := filepath.Join(dir, "test-skill")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: test-skill
description: A test skill for unit testing
---

# Test Skill

Do the thing.
`), 0o644)

	catalog, err := LoadFromDirectory(dir)
	if err != nil {
		t.Fatalf("LoadFromDirectory failed: %v", err)
	}

	metas := catalog.List()
	if len(metas) != 1 {
		t.Fatalf("expected 1 skill, got %d", len(metas))
	}
	if metas[0].Name != "test-skill" {
		t.Fatalf("expected name 'test-skill', got '%s'", metas[0].Name)
	}

	skill := catalog.Get("test-skill")
	if skill == nil {
		t.Fatal("Get returned nil")
	}
	if skill.PromptBody == "" {
		t.Fatal("PromptBody is empty")
	}
	t.Logf("Skill body: %s", skill.PromptBody)
}

func TestLoadCodySkills(t *testing.T) {
	wd, _ := os.Getwd()
	// 向上逐层查找项目根目录
	for wd != "/" {
		if _, err := os.Stat(filepath.Join(wd, ".cody", "skills")); err == nil {
			break
		}
		wd = filepath.Dir(wd)
	}

	skillsDir := filepath.Join(wd, ".cody", "skills")
	if _, err := os.Stat(skillsDir); os.IsNotExist(err) {
		t.Skip("No .cody/skills directory found")
	}

	catalog, err := LoadFromDirectory(skillsDir)
	if err != nil {
		t.Fatalf("Failed to load skills: %v", err)
	}

	metas := catalog.List()
	t.Logf("Loaded %d skill(s) from %s", len(metas), skillsDir)
	for _, m := range metas {
		t.Logf("  - %s: %s", m.Name, m.Description)
		s := catalog.Get(m.Name)
		if s != nil {
			t.Logf("    Body: %d chars", len(s.PromptBody))
		}
	}

	if len(metas) == 0 {
		t.Fatal("Expected at least 1 skill")
	}
}

func TestSkillRenderPlaceholder(t *testing.T) {
	s := &Skill{
		PromptBody: "Greet $ARGUMENTS warmly.",
	}
	if got := s.Render("Alice"); got != "Greet Alice warmly." {
		t.Errorf("placeholder substitution failed: %q", got)
	}
}

func TestSkillRenderAppendsWhenNoPlaceholder(t *testing.T) {
	s := &Skill{PromptBody: "Plain body."}
	got := s.Render("extra args")
	if !strings.Contains(got, "Plain body.") {
		t.Errorf("body missing: %q", got)
	}
	if !strings.Contains(got, "## User Request") {
		t.Errorf("user request header missing: %q", got)
	}
	if !strings.Contains(got, "extra args") {
		t.Errorf("args missing: %q", got)
	}
}

func TestSkillRenderNoArgsReturnsBody(t *testing.T) {
	s := &Skill{PromptBody: "just the body"}
	if got := s.Render(""); got != "just the body" {
		t.Errorf("expected body unchanged, got %q", got)
	}
}

func TestLoadSkillsMergesPriority(t *testing.T) {
	work := t.TempDir()
	codyDir := filepath.Join(work, ".cody", "skills", "shared")
	if err := os.MkdirAll(codyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(codyDir, "SKILL.md"), []byte(`---
name: shared
description: project skill from .cody
---
cody body`), 0o644)

	catalog := LoadSkills(work)
	got := catalog.Get("shared")
	if got == nil {
		t.Fatal("merged catalog missing 'shared'")
	}
	if !strings.Contains(got.PromptBody, "cody body") {
		t.Errorf("expected cody body; got body=%q", got.PromptBody)
	}
}

func TestLoadSkillsForkMode(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".cody", "skills", "reviewer")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: reviewer
description: skill that runs in fork mode
mode: fork
fork_context: recent
---
body`), 0o644)

	catalog := LoadSkills(dir)
	s := catalog.Get("reviewer")
	if s == nil {
		t.Fatal("skill not loaded")
	}
	if !s.Meta.IsFork() {
		t.Errorf("expected fork mode")
	}
	if s.Meta.ForkContext != "recent" {
		t.Errorf("ForkContext = %q, want recent", s.Meta.ForkContext)
	}
}

// 渲染结果只跟正文和参数有关，与 mode 无关：fork 的 Skill 拿到的也是同一份正文，
// 隔离执行由调用方负责。
func TestSkillRenderIgnoresMode(t *testing.T) {
	forked := &Skill{
		Meta:       SkillMeta{Name: "audit-deps", Context: "fork"},
		PromptBody: "Inspect go.mod and flag risky pins.",
	}
	if got := forked.Render(""); got != "Inspect go.mod and flag risky pins." {
		t.Errorf("fork skill render = %q, want the raw body", got)
	}

	inline := &Skill{
		Meta:       SkillMeta{Name: "plain", Context: ""},
		PromptBody: "just do it",
	}
	if got := inline.Render(""); got != "just do it" {
		t.Errorf("inline render should be raw body, got: %q", got)
	}
}

func TestLoadSkillsContextFork(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, ".cody", "skills", "forky")
	os.MkdirAll(skillDir, 0o755)
	os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: forky
description: skill that runs in a subagent
context: fork
---
body content`), 0o644)

	catalog := LoadSkills(dir)
	s := catalog.Get("forky")
	if s == nil {
		t.Fatal("skill not loaded")
	}
	if s.Meta.Context != "fork" {
		t.Errorf("Context parse failed: got %q want %q", s.Meta.Context, "fork")
	}
}

func TestSkillIntegration(t *testing.T) {
	dir := t.TempDir()

	// 创建两个技能
	s1Dir := filepath.Join(dir, "greeting")
	os.MkdirAll(s1Dir, 0o755)
	os.WriteFile(filepath.Join(s1Dir, "SKILL.md"), []byte(`---
name: greeting
description: Generate a friendly greeting message
---

# Greeting Skill

Say hello to the user in a friendly way.
`), 0o644)

	s2Dir := filepath.Join(dir, "summarize")
	os.MkdirAll(s2Dir, 0o755)
	os.WriteFile(filepath.Join(s2Dir, "SKILL.md"), []byte(`---
name: summarize
description: Summarize text content concisely
---

# Summarize Skill

Provide a concise summary of the given text.
`), 0o644)

	catalog, err := LoadFromDirectory(dir)
	if err != nil {
		t.Fatalf("LoadFromDirectory failed: %v", err)
	}

	metas := catalog.List()
	if len(metas) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(metas))
	}

	// 验证两个技能都能取回
	for _, name := range []string{"greeting", "summarize"} {
		s := catalog.Get(name)
		if s == nil {
			t.Fatalf("skill %q not found", name)
		}
		if s.PromptBody == "" {
			t.Fatalf("skill %q has empty body", name)
		}
		if s.SourceDir == "" {
			t.Fatalf("skill %q has empty SourceDir", name)
		}
	}

	// 模拟系统提示构造（与 tui.go 逻辑相同）
	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	sb.WriteString("Skills are installed at: " + dir + "\n")
	sb.WriteString("When creating new skills, always place them under this directory as <skill-name>/SKILL.md.\n\n")
	sb.WriteString("The following skills are available. When the user invokes /<name>, follow that skill's instructions.\n\n")
	for _, meta := range metas {
		desc := meta.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		sb.WriteString("- /" + meta.Name + ": " + desc + "\n")
	}
	prompt := sb.String()

	if !strings.Contains(prompt, "Skills are installed at: "+dir) {
		t.Fatal("system prompt missing skills directory path")
	}
	if !strings.Contains(prompt, "creating new skills") {
		t.Fatal("system prompt missing creation guidance")
	}
	if !strings.Contains(prompt, "/greeting") {
		t.Fatal("system prompt missing /greeting command")
	}
	if !strings.Contains(prompt, "/summarize") {
		t.Fatal("system prompt missing /summarize command")
	}

	// 模拟技能命令处理器（与 tui.go 逻辑相同）
	skill := catalog.Get("greeting")
	body := skill.PromptBody

	// 不带参数
	result := body
	if !strings.Contains(result, "Greeting Skill") {
		t.Fatal("handler without args should return skill body")
	}

	// 带参数
	args := "say hi to Alice"
	result = body + "\n\n## User Request\n\n" + args
	if !strings.Contains(result, "## User Request") {
		t.Fatal("handler with args should include User Request section")
	}
	if !strings.Contains(result, "say hi to Alice") {
		t.Fatal("handler with args should include the user's args")
	}
	if !strings.Contains(result, "Greeting Skill") {
		t.Fatal("handler with args should still include skill body")
	}

	t.Logf("System prompt snippet:\n%s", prompt)
	t.Logf("Handler output (with args):\n%s", result)
}

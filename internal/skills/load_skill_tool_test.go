package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cody/internal/tools"
)

// writeSkill 在临时目录里落一个 SKILL.md，返回可直接加载的技能根目录。
func writeSkill(t *testing.T, name, frontmatter, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "---\nname: " + name + "\ndescription: test skill\n" + frontmatter + "---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return root
}

// fork 模式下 SOP 正文交给子 Agent，主对话只拿到最终结果。
func TestLoadSkillToolForkRunsSubAgent(t *testing.T) {
	root := writeSkill(t, "audit-deps", "mode: fork\n", "Inspect go.mod and flag risky pins.")
	catalog, err := LoadFromDirectory(root)
	if err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	host := newStubHost(tools.NewRegistry())
	host.subAgentReply = "3 risky pins found"

	tool := &LoadSkillTool{Catalog: catalog, Host: host, ForkHost: host}
	res := tool.Execute(context.Background(), map[string]any{"name": "audit-deps"})

	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	if res.Output != "3 risky pins found" {
		t.Errorf("Output = %q, want the sub-agent's final text", res.Output)
	}
	if strings.Contains(res.Output, "Inspect go.mod") {
		t.Error("SOP body leaked into the main conversation; fork should keep it isolated")
	}
	if !strings.Contains(host.subAgentBody, "Inspect go.mod") {
		t.Errorf("sub-agent did not receive the skill body, got %q", host.subAgentBody)
	}
	if len(host.activated) != 0 {
		t.Errorf("fork skill should not be activated inline, got %v", host.activated)
	}
}

// 宿主没有接入子 Agent 运行时（ForkHost 为 nil）时回退成 inline，工具仍然可用。
func TestLoadSkillToolForkFallsBackWithoutForkHost(t *testing.T) {
	root := writeSkill(t, "audit-deps", "mode: fork\n", "Inspect go.mod and flag risky pins.")
	catalog, err := LoadFromDirectory(root)
	if err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	host := newStubHost(tools.NewRegistry())
	tool := &LoadSkillTool{Catalog: catalog, Host: host}
	res := tool.Execute(context.Background(), map[string]any{"name": "audit-deps"})

	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Inspect go.mod") {
		t.Errorf("fallback should return the SOP body, got %q", res.Output)
	}
	if _, ok := host.activated["audit-deps"]; !ok {
		t.Error("fallback should activate the skill inline")
	}
}

// inline 模式保持原样：返回 SOP 正文并登记激活。
func TestLoadSkillToolInlineReturnsBody(t *testing.T) {
	root := writeSkill(t, "commit", "mode: inline\n", "Write a conventional commit message.")
	catalog, err := LoadFromDirectory(root)
	if err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	host := newStubHost(tools.NewRegistry())
	tool := &LoadSkillTool{Catalog: catalog, Host: host, ForkHost: host}
	res := tool.Execute(context.Background(), map[string]any{"name": "commit"})

	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Output)
	}
	if !strings.Contains(res.Output, "Write a conventional commit message.") {
		t.Errorf("inline should return the SOP body, got %q", res.Output)
	}
	if host.subAgentBody != "" {
		t.Error("inline skill must not spawn a sub-agent")
	}
}

// 老写法 context: fork 与 mode: fork 等价，从别的生态拿来的技能也能正确隔离执行。
func TestLoadSkillToolLegacyContextForkRunsSubAgent(t *testing.T) {
	root := writeSkill(t, "audit-deps", "context: fork\n", "Inspect go.mod and flag risky pins.")
	catalog, err := LoadFromDirectory(root)
	if err != nil {
		t.Fatalf("LoadFromDirectory: %v", err)
	}

	host := newStubHost(tools.NewRegistry())
	host.subAgentReply = "done"

	tool := &LoadSkillTool{Catalog: catalog, Host: host, ForkHost: host}
	res := tool.Execute(context.Background(), map[string]any{"name": "audit-deps"})

	if res.Output != "done" {
		t.Errorf("Output = %q, want the sub-agent's final text", res.Output)
	}
	if host.subAgentBody == "" {
		t.Error("legacy context: fork should also run in a sub-agent")
	}
}

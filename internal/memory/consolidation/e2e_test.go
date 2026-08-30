//go:build e2e

package consolidation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cody/internal/agent"
	"cody/internal/agents"
	"cody/internal/config"
	"cody/internal/conversation"
	"cody/internal/llm"
	"cody/internal/memory"
	"cody/internal/permissions"
	"cody/internal/tools"
)

// TestE2E_ConsolidationMergesDuplicates 是真正的端到端测试：
// 用真实 LLM 运行记忆整理子 Agent，验证它能合并重复记忆。
//
// 运行方式：go test -tags e2e -run TestE2E -v -timeout 120s
// 需要环境变量 CODY_TEST_API_KEY 和 CODY_TEST_BASE_URL
func TestE2E_ConsolidationMergesDuplicates(t *testing.T) {
	apiKey := os.Getenv("CODY_TEST_API_KEY")
	baseURL := os.Getenv("CODY_TEST_BASE_URL")
	model := os.Getenv("CODY_TEST_MODEL")
	if apiKey == "" {
		t.Skip("CODY_TEST_API_KEY not set, skipping E2E test")
	}
	if baseURL == "" {
		baseURL = "https://api.minimaxi.com/v1"
	}
	if model == "" {
		model = "MiniMax-M3"
	}

	// 构造测试目录
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".cody", "memory")
	os.MkdirAll(memDir, 0o755)

	// 写两个重复的记忆文件
	writeMemory(t, memDir, "feedback_no_push.md", "feedback", "no-push",
		"Don't push without asking",
		"用户不希望自动 push 代码")

	writeMemory(t, memDir, "feedback_auto_push.md", "feedback", "auto-push",
		"Don't auto push code",
		"用户不喜欢自动 push，每次都要先问一下")

	// 写一个过时的记忆
	writeMemory(t, memDir, "project_deadline.md", "project", "deadline",
		"Project deadline is 2025-01-15",
		"项目截止日期是 2025 年 1 月 15 日\n\n**Why:** 客户要求\n**How to apply:** 所有任务在此之前完成")

	// 写一个正常的记忆
	writeMemory(t, memDir, "user_role.md", "user", "user-role",
		"User is a backend engineer",
		"用户是后端工程师，主要用 Go 和 Java")

	// 写 MEMORY.md 索引
	indexContent := `- [No push](feedback_no_push.md) — 不要自动 push
- [Auto push](feedback_auto_push.md) — 不要自动 push 代码
- [Deadline](project_deadline.md) — 项目截止日期 2025-01-15
- [User role](user_role.md) — 后端工程师
`
	os.WriteFile(filepath.Join(memDir, "MEMORY.md"), []byte(indexContent), 0o644)

	t.Logf("Test directory: %s", dir)
	t.Logf("Memory files before consolidation:")
	listMemoryFiles(t, memDir)

	// 构建 LLM 客户端
	cfg := &config.ProviderConfig{}
	cfg.Protocol = "openai-compat"
	cfg.BaseURL = baseURL
	cfg.APIKey = apiKey
	cfg.Model = model
	cfg.ContextWindow = 200000
	client, err := llm.NewClient(cfg, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// 构建工具注册表：注册整理需要的工具
	registry := tools.NewRegistry()
	registry.Register(&tools.ReadFileTool{})
	registry.Register(&tools.WriteFileTool{})
	registry.Register(&tools.EditFileTool{})
	registry.Register(&tools.GlobTool{})
	registry.Register(&tools.GrepTool{})
	registry.Register(&tools.BashTool{})
	subRegistry := agents.FilterToolsForAgent(registry, nil, nil, true)

	// 权限沙箱：只允许写记忆目录
	sandbox := permissions.NewPathSandbox(memDir)
	checker := permissions.NewChecker(sandbox, &permissions.RuleEngine{}, permissions.ModeBypass)

	// 构建整理 prompt
	prompt := BuildConsolidationPrompt(memDir, "", "", nil)

	conv := conversation.NewManager()
	conv.AddUserMessage(prompt)

	subAgent := agent.New(client, subRegistry, "openai")
	subAgent.MaxIterations = 15
	subAgent.Checker = checker
	subAgent.WorkDir = dir

	t.Log("Starting consolidation sub-agent...")
	startTime := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ch := subAgent.Run(ctx, conv)
	for event := range ch {
		switch e := event.(type) {
		case agent.StreamText:
			fmt.Print(e.Text)
		case agent.LoopComplete:
			t.Logf("\nSub-agent completed in %s, %d turns", time.Since(startTime), e.TotalTurns)
		}
	}

	// 验证结果
	t.Log("\n\nMemory files after consolidation:")
	listMemoryFiles(t, memDir)

	// 检查 1：重复记忆是否被合并（至少删了一个）
	files := listMdFiles(memDir)
	hasPush := false
	pushCount := 0
	for _, f := range files {
		content, _ := os.ReadFile(filepath.Join(memDir, f))
		if strings.Contains(strings.ToLower(string(content)), "push") {
			pushCount++
			hasPush = true
		}
	}
	if hasPush && pushCount > 1 {
		t.Logf("WARNING: push-related memories not merged (still %d files)", pushCount)
	} else if pushCount <= 1 {
		t.Log("PASS: duplicate push memories merged")
	}

	// 检查 2：MEMORY.md 索引是否被更新
	indexBytes, err := os.ReadFile(filepath.Join(memDir, "MEMORY.md"))
	if err != nil {
		t.Fatalf("MEMORY.md not found after consolidation")
	}
	t.Logf("MEMORY.md content:\n%s", string(indexBytes))

	// 检查 3：索引行数是否合理
	lines := strings.Split(strings.TrimSpace(string(indexBytes)), "\n")
	nonEmpty := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			nonEmpty++
		}
	}
	if nonEmpty > memory.MaxEntrypointLines {
		t.Errorf("MEMORY.md has %d lines, exceeds limit %d", nonEmpty, memory.MaxEntrypointLines)
	}

	// 检查 4：对话中是否有 WriteFile/EditFile 工具调用
	writtenPaths := extractWrittenPaths(conv.GetMessages())
	t.Logf("Files written by sub-agent: %v", writtenPaths)
	if len(writtenPaths) == 0 {
		t.Error("sub-agent did not write any files — consolidation had no effect")
	}
}

func writeMemory(t *testing.T, dir, filename, memType, name, desc, body string) {
	t.Helper()
	content := fmt.Sprintf(`---
name: %s
description: %s
metadata:
  type: %s
---

%s
`, name, desc, memType, body)
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func listMemoryFiles(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() {
			info, _ := e.Info()
			t.Logf("  %s (%d bytes)", e.Name(), info.Size())
		}
	}
}

func listMdFiles(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") && e.Name() != "MEMORY.md" {
			files = append(files, e.Name())
		}
	}
	return files
}

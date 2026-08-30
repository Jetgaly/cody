package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RelevantMemory 是被选中要呈现到主对话中的一条记忆文件。MtimeMs 被
// 一路传下去，这样调用方无需二次 stat 就能渲染新鲜度。
type RelevantMemory struct {
	Path    string
	MtimeMs int64
}

// SelectorFn 是召回选择器所用的侧查询 LLM 调用的抽象。给定
// 系统提示词和用户消息，调用方需要发起一次性模型调用，并
// 返回原始的 assistant 文本。FindRelevantMemories 把错误视为“选择器失败 → 不召回”。
// Cody 的 llm.Client 是流式 + 绑定系统提示词的接口，因此
// 这个回调让调用方能够建立一个专用的侧查询 client，而无需在包级别
// 把 memory → llm 耦合起来。
type SelectorFn func(ctx context.Context, systemPrompt, userMessage string) (string, error)

// SelectMemoriesSystemPrompt 是选择器 agent 的系统提示词。
const SelectMemoriesSystemPrompt = `You are selecting memories that will be useful to Cody as it processes a user's query. You will be given the user's query and a list of available memory files with their filenames and descriptions.

Return a list of filenames for the memories that will clearly be useful to Cody as it processes the user's query (up to 5). Only include memories that you are certain will be helpful based on their name and description.
- If you are unsure if a memory will be useful in processing the user's query, then do not include it in your list. Be selective and discerning.
- If there are no memories in the list that would clearly be useful, feel free to return an empty list.
- If a list of recently-used tools is provided, do not select memories that are usage reference or API documentation for those tools (Cody is already exercising them). DO still select memories containing warnings, gotchas, or known issues about those tools — active use is exactly when those matter.

Respond with valid JSON only, no markdown, in this exact shape: {"selected_memories": ["filename1.md", "filename2.md"]}`

// FindRelevantMemories 同时扫描 userMemDir 和 projectMemDir，让选择器为 query 挑出最多 5 个
// 相关文件名，并返回对应的绝对路径 + mtime。会排除
// MEMORY.md（已加载进系统提示词）。mtime 一路传下去，这样调用方无需二次 stat
// 就能呈现新鲜度。
//
// alreadySurfaced 在选择器调用前过滤掉之前几轮已展示过的路径，这样 5 个名额
// 花在新候选上，而不是重新挑选调用方即将丢弃的文件。
//
// 两个目录都允许为空——只扫描非空的那个。跨两个目录的文件名冲突
// 通过返回的 RelevantMemory 中的 FilePath 来消歧。
//
// 选择器失败是静默的——召回是尽力而为，绝不能阻塞主对话。
// 任何选择器/解析错误都返回空切片 + nil error。
func FindRelevantMemories(
	ctx context.Context,
	query string,
	userMemDir, projectMemDir string,
	recentTools []string,
	alreadySurfaced map[string]struct{},
	selector SelectorFn,
) ([]RelevantMemory, error) {
	if selector == nil {
		return nil, nil
	}
	var all []MemoryHeader
	if userMemDir != "" {
		userScan, err := ScanMemoryFiles(ctx, userMemDir, "user")
		if err != nil {
			return nil, err
		}
		all = append(all, userScan...)
	}
	if projectMemDir != "" {
		projectScan, err := ScanMemoryFiles(ctx, projectMemDir, "project")
		if err != nil {
			return nil, err
		}
		all = append(all, projectScan...)
	}
	memories := make([]MemoryHeader, 0, len(all))
	for _, m := range all {
		if _, ok := alreadySurfaced[m.FilePath]; ok {
			continue
		}
		memories = append(memories, m)
	}
	if len(memories) == 0 {
		return nil, nil
	}

	selectedFilenames, _ := selectRelevantMemories(ctx, query, memories, recentTools, selector)
	byKey := make(map[string]MemoryHeader, len(memories))
	for _, m := range memories {
		byKey[m.FilePath] = m
		// 同时按 Filename 建立索引，因为选择器的输出可能只给文件名。
		if _, exists := byKey[m.Filename]; !exists {
			byKey[m.Filename] = m
		}
	}
	selected := make([]RelevantMemory, 0, len(selectedFilenames))
	for _, fn := range selectedFilenames {
		m, ok := byKey[fn]
		if !ok {
			continue
		}
		selected = append(selected, RelevantMemory{Path: m.FilePath, MtimeMs: m.MtimeMs})
	}
	return selected, nil
}

/*
在 tui.go:2695 和 tui.go:2739 里，每次用户发送消息后，TUI 会先调用 prefetchRelevantMemories(...)，里面再异步调用 find_relevant_memories.go:54 的 FindRelevantMemories(...)。它会基于当前 query、最近用过的工具、用户记忆目录和项目记忆目录，筛出最多 5 条相关记忆。

这些记忆不是直接立刻塞进主回答，而是先放到 ag.MemoryRecallCh，等 agent 执行过程中再由 agent.go:434 非阻塞地取出来，作为 system reminder 注入对话。这样做的目的，是在不拖慢主流程的前提下，把“跟当前任务相关的旧记忆”补给模型。

所以它的典型场景不是启动时加载全量记忆，而是“每一轮用户输入后，做一次轻量召回，辅助当前任务”
*/
func selectRelevantMemories(
	ctx context.Context,
	query string,
	memories []MemoryHeader,
	recentTools []string,
	selector SelectorFn,
) ([]string, error) {
	validFilenames := make(map[string]struct{}, len(memories))
	for _, m := range memories {
		validFilenames[m.Filename] = struct{}{}
	}

	manifest := FormatMemoryManifest(memories)

	// 当 Cody 正在积极使用某个工具（例如 mcp__X__spawn）时，把该工具的参考文档呈现出来
	// 是噪音——对话里已经包含实际用法。否则选择器会按
	// 关键词重叠来匹配（查询里的 "spawn" + 记忆描述里的 "spawn" → 误报）。
	toolsSection := ""
	if len(recentTools) > 0 {
		toolsSection = "\n\nRecently used tools: " + strings.Join(recentTools, ", ")
	}

	userMessage := fmt.Sprintf("Query: %s\n\nAvailable memories:\n%s%s", query, manifest, toolsSection)

	raw, err := selector(ctx, SelectMemoriesSystemPrompt, userMessage)
	if err != nil {
		return nil, nil
	}
	clean := extractJSONObject(raw)
	if clean == "" {
		return nil, nil
	}
	var parsed struct {
		SelectedMemories []string `json:"selected_memories"`
	}
	if err := json.Unmarshal([]byte(clean), &parsed); err != nil {
		return nil, nil
	}
	out := make([]string, 0, len(parsed.SelectedMemories))
	for _, f := range parsed.SelectedMemories {
		if _, ok := validFilenames[f]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// extractJSONObject 返回在 raw 中找到的第一个 {.} 子串；如果它已经以 `{` 开头，
// 则返回修剪后的原始文本。即使提示词要求严格 JSON，也能容忍 JSON 周围
// 有 markdown 围栏或散文。
func extractJSONObject(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	start := strings.Index(trimmed, "{")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(trimmed, "}")
	if end < start {
		return ""
	}
	return trimmed[start : end+1]
}


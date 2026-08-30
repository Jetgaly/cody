// Package toolresult 实现 Layer 1 工具结果预算：超限的工具结果溢写到磁盘，
// 对话历史里只留预览和文件路径。所有处理都发生在结果进入对话历史之前，
// 消息一进历史就是终态，后续轮次发请求时逐字节保持原样，Prompt Cache
// 前缀天然稳定。
package toolresult

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cody/internal/conversation"
)

const (
	// MessageAggregateLimit 单条消息中所有工具结果的总字符数上限。
	// 单条结果的大小由 tools.MaxOutputChars 把关，这里管的是聚合——
	// 一轮并行调多个工具时，每条都没超单条阈值，加起来却能撑爆上下文，
	// 这是单条阈值管不到的场景。
	MessageAggregateLimit = 200000
)

// SpillDir 返回溢写目录：按会话隔离在 .cody/sessions/<会话id>/tool-results
// 下，会话 id 为空（一次性调用、测试）时落到 default。
func SpillDir(workDir, sessionID string) string {
	if sessionID == "" {
		sessionID = "default"
	}
	return filepath.Join(workDir, ".cody", "sessions", sessionID, "tool-results")
}

// ApplyBudget 在一轮工具结果进入对话历史之前执行聚合预算：整批结果的
// 总字符数超过 MessageAggregateLimit 时，从最大的开始逐条溢写到磁盘、
// 就地替换成预览，直到总量回到限额内。
//
// exempt 里的 tool_use_id 不参与溢写：溢写文件的回读结果（再溢写模型就
// 永远看不到全文），以及本轮已经单条溢写过的结果。全是豁免项时接受超额。
func ApplyBudget(results []conversation.ToolResultBlock, exempt map[string]bool, workDir, sessionID string) {
	total := 0
	for i := range results {
		total += len(results[i].Content)
	}
	if total <= MessageAggregateLimit {
		return
	}

	spillDir := SpillDir(workDir, sessionID)

	// 按内容长度降序挑选：先溢写最大的，回到限额内需要动的条数最少。
	order := make([]int, len(results))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return len(results[order[a]].Content) > len(results[order[b]].Content)
	})

	for _, idx := range order {
		if total <= MessageAggregateLimit {
			break
		}
		r := &results[idx]
		if exempt[r.ToolUseID] {
			continue
		}
		if len(r.Content) <= previewSize {
			// 比预览还短的结果，溢写换不回空间。
			continue
		}
		path, err := writeSpill(spillDir, r.ToolUseID, r.Content)
		if err != nil {
			// 写盘失败就保留原文。消息随即定型进历史，不会再有重试。
			continue
		}
		preview := buildSpillPreview(r.Content, path)
		total -= len(r.Content) - len(preview)
		r.Content = preview
	}
}

// IsSpillReadback 判断一次工具调用是不是在读回溢写目录下的文件。
// 这类结果不做溢写：把模型刚读回来的内容再写盘换成预览，模型就永远
// 看不到全文，还会在「读回、溢写」之间打转。
func IsSpillReadback(toolName string, args map[string]any, workDir, sessionID string) bool {
	if toolName != "ReadFile" {
		return false
	}
	raw, _ := args["file_path"].(string)
	if raw == "" {
		return false
	}
	absSpillDir, err := filepath.Abs(SpillDir(workDir, sessionID))
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return false
	}
	return strings.HasPrefix(abs, absSpillDir)
}

// previewSize 是存盘预览的最大字符数，取前 2KB 兼顾可读性和空间占用。
const previewSize = 2000

// buildSpillPreview 构造存盘替换文本，包含前 2KB 预览。相同输入产出
// 逐字节相同的字符串——替换文本进入对话历史后不再改动，格式变更只影响
// 之后新产生的结果。
func buildSpillPreview(content string, path string) string {
	sizeKB := len(content) / 1024
	preview := content
	hasMore := false
	if len(preview) > previewSize {
		preview = preview[:previewSize]
		hasMore = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<persisted-output>\n")
	fmt.Fprintf(&b, "输出太大（%dKB），完整内容已保存到：\n%s\n\n", sizeKB, path)
	fmt.Fprintf(&b, "预览（前 2KB）：\n%s", preview)
	if hasMore {
		b.WriteString("\n...")
	}
	b.WriteString("\n</persisted-output>")
	return b.String()
}

func writeSpill(dir, toolUseID, content string) (string, error) {
	if toolUseID == "" {
		return "", fmt.Errorf("empty tool_use_id")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, toolUseID+".txt")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return path, nil
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", err
	}
	return path, nil
}

// PersistLargeResult 将超大工具输出溢写到磁盘，返回预览文本。
// agent 在工具结果入对话历史时调用。
func PersistLargeResult(workDir, sessionID, toolUseID, content string) string {
	dir := SpillDir(workDir, sessionID)
	path, err := writeSpill(dir, toolUseID, content)
	if err != nil {
		return content
	}
	return buildSpillPreview(content, path)
}

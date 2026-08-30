package tools

import (
	"fmt"
	"strings"
)

const (
	diffContextLines = 3
	// 防止超大文件产出天量 diff 文本拖垮 TUI 渲染和上下文占用
	maxDiffLines = 200
)

// DiffResult 是 BuildDiff 的返回值：带行号的 diff 文本 + 新增/删除行数统计。
type DiffResult struct {
	Text      string
	Additions int
	Removals  int
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// BuildDiff 对比编辑前后的文件内容，生成一段带行号的 diff。
// 利用"编辑只改动中间一小段"的特点，从两端找公共前缀/后缀行，
// 避免跑通用的 LCS/Myers diff 算法（对大文件更快，实现也更简单）。
func BuildDiff(oldContent, newContent string) DiffResult {
	oldLines := strings.Split(oldContent, "\n")
	newLines := strings.Split(newContent, "\n")

	prefixLen := 0
	maxPrefix := min(len(oldLines), len(newLines))
	for prefixLen < maxPrefix && oldLines[prefixLen] == newLines[prefixLen] {
		prefixLen++
	}

	suffixLen := 0
	maxSuffix := maxPrefix - prefixLen
	for suffixLen < maxSuffix &&
		oldLines[len(oldLines)-1-suffixLen] == newLines[len(newLines)-1-suffixLen] {
		suffixLen++
	}

	removedLines := oldLines[prefixLen : len(oldLines)-suffixLen]
	addedLines := newLines[prefixLen : len(newLines)-suffixLen]

	contextStart := max(0, prefixLen-diffContextLines)
	contextBefore := oldLines[contextStart:prefixLen]
	contextEnd := min(len(oldLines), len(oldLines)-suffixLen+diffContextLines)
	contextAfter := oldLines[len(oldLines)-suffixLen : contextEnd]

	var out []string
	oldLineNo := contextStart + 1
	newLineNo := contextStart + 1
	truncated := false

	push := func(prefix string, lineNo int, content string) {
		if len(out) >= maxDiffLines {
			truncated = true
			return
		}
		out = append(out, fmt.Sprintf("%s %4d  %s", prefix, lineNo, content))
	}

	for _, l := range contextBefore {
		push(" ", oldLineNo, l)
		oldLineNo++
		newLineNo++
	}
	for _, l := range removedLines {
		push("-", oldLineNo, l)
		oldLineNo++
	}
	for _, l := range addedLines {
		push("+", newLineNo, l)
		newLineNo++
	}
	for _, l := range contextAfter {
		push(" ", oldLineNo, l)
		oldLineNo++
		newLineNo++
	}

	if truncated {
		out = append(out, fmt.Sprintf("  … (diff truncated at %d lines)", maxDiffLines))
	}

	return DiffResult{
		Text:      strings.Join(out, "\n"),
		Additions: len(addedLines),
		Removals:  len(removedLines),
	}
}

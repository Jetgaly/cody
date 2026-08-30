/*
启动或重建系统提示词时，TUI 会先调用 memdir.go:217 的 LoadAutoMemoryPrompt(wd)，
把自动记忆区整理成一段 memoryContent，见 tui.go:909 到 tui.go:912。
随后在对话真正开始前，conversation.InjectLongTermMemory(...) 
会把这段内容包成一个 <system-reminder> 前缀消息插到历史最前面，
但它有 ltmInjected 保护，只会注入一次，见 con
versation.go:114 到 conversation.go:134

memoryContent 更像是“记忆系统说明 + 两个目录的索引页内容”，
不是“把目录下所有 memory 文件全文预加载”。真正的具体记忆正文，
是后面通过 FindRelevantMemories 按需召回后，再注入到对话里的。
这个注入逻辑在 conversation.go 里是 InjectLongTermMemory(...)，
而 memoryContent 只是传给它的那段长期记忆上下文。
*/
package memory

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryHeader 是一个被扫描记忆文件的元数据。
type MemoryHeader struct {
	Filename    string     // 相对于 memoryDir 的路径
	FilePath    string     // 绝对路径
	Scope       string     // "user" 或 "project"；对不关心此值的调用方为空
	MtimeMs     int64      // 修改时间，自纪元起的毫秒数
	Description string     // frontmatter 描述；不存在则为空
	Type        MemoryType // frontmatter 类型；无法识别则为空
}

// MaxMemoryFiles 限制展示给模型的记忆数量上限。
// FrontmatterMaxLines 限制读取每个文件用于头部解析的行数。
const (
	MaxMemoryFiles      = 200
	FrontmatterMaxLines = 30
)

// ScanMemoryFiles 扫描记忆目录中的 .md 文件，读取其 frontmatter，并返回
// 按最新优先排序的头部列表（上限为 MaxMemoryFiles）。由
// findRelevantMemories（查询时召回）和 extractMemories（预注入清单，使
// 提取代理不必花一整轮去执行 `ls`）共用。
//
// 单趟完成：每个文件的 mtime 与其内容一并读取，因此我们是先读后排序，
// 而不是先 stat 排序再读。单个文件的错误会被静默丢弃（打不开的文件
// 不应拖垮整个扫描）。
func ScanMemoryFiles(ctx context.Context, memoryDir string, scope string) ([]MemoryHeader, error) {
	var mdFiles []string
	walkErr := filepath.WalkDir(memoryDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".md") || name == AutoMemEntrypointName {
			return nil
		}
		mdFiles = append(mdFiles, path)
		return nil
	})
	if walkErr != nil {
		return nil, nil
	}

	results := make([]MemoryHeader, 0, len(mdFiles))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, filePath := range mdFiles {
		if err := ctx.Err(); err != nil {
			break
		}
		wg.Add(1)
		go func(fp string) {
			defer wg.Done()
			hdr, ok := readMemoryHeader(fp, memoryDir)
			if !ok {
				return
			}
			hdr.Scope = scope
			mu.Lock()
			results = append(results, hdr)
			mu.Unlock()
		}(filePath)
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		return results[i].MtimeMs > results[j].MtimeMs
	})
	if len(results) > MaxMemoryFiles {
		results = results[:MaxMemoryFiles]
	}
	return results, nil
}

func readMemoryHeader(filePath, memoryDir string) (MemoryHeader, bool) {
	info, err := os.Stat(filePath)
	if err != nil {
		return MemoryHeader{}, false
	}
	f, err := os.Open(filePath)
	if err != nil {
		return MemoryHeader{}, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var sb strings.Builder
	for i := 0; i < FrontmatterMaxLines && scanner.Scan(); i++ {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}

	mf := parseFrontmatter(sb.String())
	rel, err := filepath.Rel(memoryDir, filePath)
	if err != nil {
		rel = filepath.Base(filePath)
	}
	return MemoryHeader{
		Filename:    rel,
		FilePath:    filePath,
		MtimeMs:     info.ModTime().UnixMilli(),
		Description: mf.Description,
		Type:        mf.Type,
	}, true
}

// FormatMemoryManifest 将记忆头部格式化为文本清单：每个文件一行，形如
// [type] filename (timestamp): description。召回选择器提示词和提取代理
// 提示词都会用到。
func FormatMemoryManifest(memories []MemoryHeader) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range memories {
		if i > 0 {
			b.WriteByte('\n')
		}
		var tag string
		if m.Type != "" {
			tag = fmt.Sprintf("[%s] ", m.Type)
		}
		var scope string
		if m.Scope != "" {
			scope = fmt.Sprintf("[%s-scope] ", m.Scope)
		}
		ts := time.UnixMilli(m.MtimeMs).UTC().Format("2006-01-02T15:04:05.000Z")
		path := m.FilePath
		if path == "" {
			path = m.Filename
		}
		if m.Description != "" {
			fmt.Fprintf(&b, "- %s%s%s (%s): %s", scope, tag, path, ts, m.Description)
		} else {
			fmt.Fprintf(&b, "- %s%s%s (%s)", scope, tag, path, ts)
		}
	}
	return b.String()
}


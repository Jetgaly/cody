package memory

import (
	"fmt"
	"time"
)

// MemoryAgeDays 返回自 mtime 以来经过的天数。向下取整——今天为 0，
// 昨天为 1，更早为 2+。负数输入（未来 mtime、时钟偏差）会被钳制为 0。
func MemoryAgeDays(mtimeMs int64) int {
	d := (time.Now().UnixMilli() - mtimeMs) / 86_400_000
	if d < 0 {
		return 0
	}
	return int(d)
}

// MemoryAge 返回人类可读的年龄字符串。模型不擅长日期运算——原始的
// ISO 时间戳无法像「47 天前」那样触发对过时信息的推理。
func MemoryAge(mtimeMs int64) string {
	d := MemoryAgeDays(mtimeMs)
	if d == 0 {
		return "today"
	}
	if d == 1 {
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", d)
}

// MemoryFreshnessText 为超过 1 天的记忆返回一段纯文本的过时警示。
// 对于新记忆（今天/昨天）返回 ""——对这些记忆发出警告只会是噪音。
//
// 当消费方已经自带包装时使用该函数（例如 messages relevant_memories →
// wrapMessagesInSystemReminder）。
//
// 起因是用户报告过时的代码状态记忆（对已改动代码的 file:line 引用）被
// 当作事实断言——引用反而让过时的说法听起来更权威，而非更不可信。
func MemoryFreshnessText(mtimeMs int64) string {
	d := MemoryAgeDays(mtimeMs)
	if d <= 1 {
		return ""
	}
	return fmt.Sprintf(
		"This memory is %d days old. "+
			"Memories are point-in-time observations, not live state — "+
			"claims about code behavior or file:line citations may be outdated. "+
			"Verify against current code before asserting as fact.",
		d,
	)
}

// MemoryFreshnessNote 返回包装在 <system-reminder> 标签中的逐条记忆过时
// 提示。对 ≤ 1 天的记忆返回 ""。用于那些不自行添加 system-reminder
// 包装的调用方。
func MemoryFreshnessNote(mtimeMs int64) string {
	text := MemoryFreshnessText(mtimeMs)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("<system-reminder>%s</system-reminder>\n", text)
}

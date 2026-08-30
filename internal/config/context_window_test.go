package config

import "testing"

// TestGetContextWindow_ConfigWins 验证第一层：显式的配置值
// 总是优先于其他所有层（拉取值 / 映射表 / 默认值），无论模型名是什么。
func TestGetContextWindow_ConfigWins(t *testing.T) {
	p := &ProviderConfig{Model: "claude-sonnet-4-6", ContextWindow: 12345}
	if got := p.GetContextWindow(); got != 12345 {
		t.Fatalf("config value should win: got %d, want 12345", got)
	}

	// 即使存在拉取到的值，配置仍然优先。
	p.SetFetchedContextWindow(999999)
	if got := p.GetContextWindow(); got != 12345 {
		t.Fatalf("config value should beat fetched value: got %d, want 12345", got)
	}
}

// TestGetContextWindow_FetchedBeatsMapping 验证第二层位于
// 映射表和默认值之上：当配置未设置时，缓存拉取到的值胜出。
func TestGetContextWindow_FetchedBeatsMapping(t *testing.T) {
	p := &ProviderConfig{Model: "claude-sonnet-4-6"} // 映射表会给出 200000
	p.SetFetchedContextWindow(321000)
	if got := p.GetContextWindow(); got != 321000 {
		t.Fatalf("fetched value should beat mapping table: got %d, want 321000", got)
	}

	// 非正的拉取值会被忽略（失败的拉取永远不会污染结果）。
	p2 := &ProviderConfig{Model: "gpt-4o"}
	p2.SetFetchedContextWindow(0)
	if got := p2.GetContextWindow(); got != 128000 {
		t.Fatalf("zero fetched value should be ignored: got %d, want 128000", got)
	}
}

// TestGetContextWindow_MappingTable 验证第三层：内置的
// 模型名 → 窗口大小子串映射为每个模型家族返回期望值，
// 否则回退到保守的默认值。
func TestGetContextWindow_MappingTable(t *testing.T) {
	tests := []struct {
		model string
		want  int
	}{
		// 1m 子串（最具体）——胜过裸的 "claude" 匹配。
		{"claude-sonnet-4-6-1m", 1000000},
		{"claude-opus-4-6-1m", 1000000},
		{"some-model-1m", 1000000},
		// OpenAI 系列。
		{"gpt-4.1", 1000000},
		{"gpt-4.1-mini", 1000000},
		{"gpt-4o", 128000},
		{"gpt-4o-mini", 128000},
		{"gpt-4-turbo", 128000},
		{"o1", 200000},
		{"o1-preview", 200000},
		{"o3-mini", 200000},
		{"o4-mini", 200000},
		{"gpt-3.5-turbo", 16385},
		// Claude 通用系列。
		{"claude-sonnet-4-6", 200000},
		{"claude-3-5-haiku-20241022", 200000},
		// 未知模型 → 保守的默认值。
		{"glm-4.7", 128000},
		{"some-unknown-model", 128000},
		{"", 128000},
	}
	for _, tt := range tests {
		p := &ProviderConfig{Model: tt.model}
		if got := p.GetContextWindow(); got != tt.want {
			t.Errorf("GetContextWindow(model=%q) = %d, want %d", tt.model, got, tt.want)
		}
	}
}

// TestLookupModelContextWindow_NoMatch 确认当没有任何匹配时，
// 原始的映射查找返回 0（而非默认值），这样 GetContextWindow 可以应用
// 自己的 claude/其他模型默认值区分逻辑。
func TestLookupModelContextWindow_NoMatch(t *testing.T) {
	if got := lookupModelContextWindow("totally-unknown"); got != 0 {
		t.Fatalf("expected 0 for unknown model, got %d", got)
	}
}

package llm

type StreamEvent interface{ streamEvent() }

type TextDelta struct{ Text string }
type ThinkingDelta struct{ Text string }
type ThinkingComplete struct {
	Thinking  string
	Signature string
}
type ToolCallStart struct{ ToolName, ToolID string }
type ToolCallDelta struct{ Text string }
type ToolCallComplete struct {
	ToolID    string
	ToolName  string
	Arguments map[string]any
}
type UsageInfo struct {
	InputTokens  int
	OutputTokens int
	// CacheReadTokens 是从 prompt 缓存中取用的输入 token 数量
	// （Anthropic 的 cache_read_input_tokens；OpenAI 的
	// prompt_tokens_details.cached_tokens）。Anthropic 不把这些计入
	// InputTokens，所以真实的 prompt 大小是
	// InputTokens + CacheReadTokens + CacheCreationTokens。
	CacheReadTokens int
	// CacheCreationTokens 是本轮写入 prompt 缓存的输入 token 数量
	// （Anthropic 的 cache_creation_input_tokens）。
	// 不报告该项的提供商为 0。
	CacheCreationTokens int
}

type StreamEnd struct {
	StopReason string
	Usage      UsageInfo
}

func (TextDelta) streamEvent()         {}

func (ThinkingDelta) streamEvent()     {}

func (ThinkingComplete) streamEvent()  {}
func (ToolCallStart) streamEvent()     {}
func (ToolCallDelta) streamEvent()     {}

func (ToolCallComplete) streamEvent()  {}

func (StreamEnd) streamEvent()         {}

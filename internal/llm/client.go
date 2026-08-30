package llm

import (
	"context"
	"fmt"

	"cody/internal/config"
	"cody/internal/conversation"
)

type Client interface {
	Stream(ctx context.Context, conv *conversation.Manager, tools []map[string]any) (<-chan StreamEvent, <-chan error)
	SetSystemPrompt(prompt string)
}

type MaxTokensSetter interface {
	SetMaxOutputTokens(tokens int)
}

func NewClient(cfg *config.ProviderConfig, systemPrompt string) (Client, error) {
	switch cfg.Protocol {
	case "anthropic":
		return newAnthropicClient(cfg, systemPrompt)
	case "openai":
		return newOpenAIClient(cfg, systemPrompt)
	case "openai-compat":
		return newOpenAICompatClient(cfg, systemPrompt)
	default:
		return nil, fmt.Errorf("unknown protocol: %s", cfg.Protocol)
	}
}

// contextWindowFetcher 由能从其提供商处拉取模型上下文窗口的客户端实现。
// 目前只有 Anthropic 客户端这样做。
type contextWindowFetcher interface {
	FetchModelContextWindow(ctx context.Context) int
}

// ResolveContextWindow 执行上下文窗口解析的第 2 层：对于 Anthropic 协议提供商，
// 它从 {base_url}/v1/models/{model} 拉取一次模型的 max_input_tokens，并通过
// SetFetchedContextWindow 缓存到 cfg 上，这样后续 cfg.GetContextWindow() 调用就能
// 直接使用，而无需再次访问网络。
//
// 它完全尽力而为，且从不返回错误：非 Anthropic 提供商、客户端构造失败，或拉取失败/超时
// 都不会改动缓存，让 GetContextWindow 回退到内置映射表/默认值。可在启动时安全调用——
// 不会超过拉取自身的超时时间阻塞，也不会 panic。
func ResolveContextWindow(ctx context.Context, cfg *config.ProviderConfig) {
	// GetContextWindow 中显式配置的值已经优先，因此这里无需拉取。
	// 同理，如果已经缓存过值也直接跳过。
	if cfg == nil || cfg.ContextWindow > 0 {
		return
	}
	if cfg.Protocol != "anthropic" {
		return
	}

	client, err := NewClient(cfg, "")
	if err != nil {
		return
	}
	fetcher, ok := client.(contextWindowFetcher)
	if !ok {
		return
	}
	if window := fetcher.FetchModelContextWindow(ctx); window > 0 {
		cfg.SetFetchedContextWindow(window)
	}
}


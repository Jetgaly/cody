package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cody/internal/config"
	"cody/internal/conversation"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

const anthropicStreamIdleTimeout = 5 * time.Minute

func supportsAdaptiveThinking(model string) bool {
	// claude-opus-4-6、claude-opus-4-7、claude-sonnet-4-6 等。
	// 但不包括 claude-sonnet-4-5（4.5 使用 enabled 模式）
	for _, family := range []string{"claude-opus-4-", "claude-sonnet-4-"} {
		if strings.HasPrefix(model, family) {
			rest := model[len(family):]
			if len(rest) > 0 && rest[0] >= '6' && rest[0] <= '9' {
				return true
			}
		}
	}
	return false
}

type anthropicClient struct {
	client          anthropic.Client
	model           string
	thinking        bool
	systemPrompt    string
	maxOutputTokens int
	contextWindow   int
}

func newAnthropicClient(cfg *config.ProviderConfig, systemPrompt string) (*anthropicClient, error) {
	apiKey := cfg.ResolveAPIKey()
	if apiKey == "" {
		return nil, &AuthenticationError{
			Message: "Anthropic API key not found. Set it in .cody/config.yaml or via ANTHROPIC_API_KEY env var.",
		}
	}

	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(cfg.BaseURL),
	)

	return &anthropicClient{
		client:          client,
		model:           cfg.Model,
		thinking:        cfg.Thinking,
		systemPrompt:    systemPrompt,
		maxOutputTokens: cfg.GetMaxOutputTokens(),
		contextWindow:   cfg.GetContextWindow(),
	}, nil
}

func (c *anthropicClient) SetSystemPrompt(prompt string) {
	c.systemPrompt = prompt
}

func (c *anthropicClient) SetMaxOutputTokens(tokens int) {
	c.maxOutputTokens = tokens
}

// anthropicModelFetchTimeout 约束模型元数据的自动拉取，确保缓慢或不可达的
// 端点永远不会延迟启动。
const anthropicModelFetchTimeout = 3 * time.Second

// FetchModelContextWindow 向 Anthropic 兼容的 /v1/models/{model} 端点查询模型的
// max_input_tokens。这是尽力而为的：在任何错误（非 Anthropic 端点、网络故障、
// 超时、字段缺失）时返回 0，且绝不 panic，也不会阻塞超过 anthropicModelFetchTimeout。
// 调用方把 0 视为“未知”，并回退到下一层 context-window 配置。
func (c *anthropicClient) FetchModelContextWindow(ctx context.Context) (window int) {
	// 硬性防护：这在启动时运行，因此 SDK 中的 panic 或格式错误的响应必须
	// 静默降级，而不是让整个进程崩溃。
	defer func() {
		if recover() != nil {
			window = 0
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, anthropicModelFetchTimeout)
	defer cancel()

	// 尽力而为的启动调用：禁用重试，使不稳定/故障端点能在超时内快速失败，
	// 而不是触发重试风暴。
	info, err := c.client.Models.Get(ctx, c.model, anthropic.ModelGetParams{}, option.WithMaxRetries(0))
	if err != nil || info == nil || info.MaxInputTokens <= 0 {
		return 0
	}
	return int(info.MaxInputTokens)
}

func (c *anthropicClient) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan StreamEvent, <-chan error) {
	events := make(chan StreamEvent, 64)
	errs := make(chan error, 1)

	// 发请求前补齐工具调用与结果的配对：中断、恢复会话、并发交错都可能留下
	// 悬空的 tool_use，缺配对会被 API 直接拒掉。
	// 
	msgs := buildAnthropicMessages(conversation.EnsureToolPairing(conv.GetMessages()))

	var sdkTools []anthropic.ToolUnionParam
	for _, s := range toolSchemas {
		inputSchema, _ := s["input_schema"].(map[string]any)
		props, _ := inputSchema["properties"]
		required, _ := inputSchema["required"].([]string)
		desc, _ := s["description"].(string)
		sdkTools = append(sdkTools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        s["name"].(string),
				Description: param.NewOpt(desc),
				InputSchema: anthropic.ToolInputSchemaParam{
					Properties: props,
					Required:   required,
				},
			},
		})
	}

	go func() {
		defer close(events)
		defer close(errs)

		maxTokens := int64(c.maxOutputTokens)
		// 将 prompt 缓存锚定在最稳定的长前缀上：system prompt。这里标记一次，
		// 再在工具列表上标记一次，以及在下方最后一条用户消息的末尾标记一次——
		// Anthropic 会缓存到每个断点为止，并在下一次请求时重新校验字节一致性。
		// 这些断点之后的 tool_result 内容保持字节稳定，因为 toolresult 预算在
		// 摄取时最终确定每条消息，之后不会再重写历史。
		// 组装消息
		params := anthropic.MessageNewParams{
			Model:     c.model,
			MaxTokens: maxTokens,
			System: []anthropic.TextBlockParam{{
				Text:         c.systemPrompt,
				CacheControl: anthropic.NewCacheControlEphemeralParam(),
			}},
			Messages: msgs,// 组装消息
		}
		markLastUserTailForCache(params.Messages)
		if c.thinking {
			if supportsAdaptiveThinking(c.model) {
				params.Thinking = anthropic.ThinkingConfigParamUnion{
					OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
				}
			} else {
				params.Thinking = anthropic.ThinkingConfigParamUnion{
					OfEnabled: &anthropic.ThinkingConfigEnabledParam{
						BudgetTokens: maxTokens - 1,
					},
				}
			}
		}
		if len(sdkTools) > 0 {
			// 工具 schema 在多次往返之间是稳定的，因此通过标记最后一个工具
			// 来缓存整个工具块几乎不花代价。
			if last := sdkTools[len(sdkTools)-1].OfTool; last != nil {
				last.CacheControl = anthropic.NewCacheControlEphemeralParam()
			}
			params.Tools = sdkTools
		}
		// 发送请求
		stream := c.client.Messages.NewStreaming(ctx, params)
		defer stream.Close()

		var currentToolName, currentToolID, jsonAccum string
		var thinkingAccum, thinkingSignature string
		inThinking := false
		var accMessage anthropic.Message

		// 在单独的 goroutine 中读取 SSE 事件，以便我们能响应 ctx 取消并检测
		// 静默断连。如果底层连接在未发送 FIN/RST 的情况下断开，SDK 的
		// stream.Next() 可能无限阻塞。
		type sseResult struct {
			hasNext bool
		}
		nextCh := make(chan sseResult, 1)

		readNext := func() {
			nextCh <- sseResult{hasNext: stream.Next()}
		}

		idle := time.NewTimer(anthropicStreamIdleTimeout)
		defer idle.Stop()

		go readNext()
		for {
			var res sseResult
			select {
			case <-ctx.Done():
				errs <- &NetworkError{Message: fmt.Sprintf("context cancelled: %v", ctx.Err())}
				return
			case <-idle.C:
				errs <- &NetworkError{Message: fmt.Sprintf("stream idle timeout: no SSE events for %s", anthropicStreamIdleTimeout)}
				return
			case res = <-nextCh:
			}

			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(anthropicStreamIdleTimeout)

			if !res.hasNext {
				break
			}

			event := stream.Current()
			accMessage.Accumulate(event)
			// Anthropic SDK 的 Accumulate 只会从 message_delta 复制 OutputTokens，
			// 但某些提供商（如 MiniMax）还会在那里报告 InputTokens 和缓存字段。
			// 手动补上这些字段。
			if mde, ok := event.AsAny().(anthropic.MessageDeltaEvent); ok {
				// 统计token
				if mde.Usage.InputTokens > 0 {
					accMessage.Usage.InputTokens = mde.Usage.InputTokens
				}
				if mde.Usage.CacheReadInputTokens > 0 {
					accMessage.Usage.CacheReadInputTokens = mde.Usage.CacheReadInputTokens
				}
				if mde.Usage.CacheCreationInputTokens > 0 {
					accMessage.Usage.CacheCreationInputTokens = mde.Usage.CacheCreationInputTokens
				}
			}
			switch ev := event.AsAny().(type) {
			case anthropic.ContentBlockStartEvent:
				switch ev.ContentBlock.Type {
				case "thinking":
					inThinking = true
					thinkingAccum = ""
					thinkingSignature = ""
				case "tool_use":
					currentToolName = ev.ContentBlock.Name
					currentToolID = ev.ContentBlock.ID
					jsonAccum = ""
					events <- ToolCallStart{ToolName: currentToolName, ToolID: currentToolID}
				}
			case anthropic.ContentBlockDeltaEvent:
				switch delta := ev.Delta.AsAny().(type) {
				case anthropic.ThinkingDelta:
					thinkingAccum += delta.Thinking
					events <- ThinkingDelta{Text: delta.Thinking}
				case anthropic.SignatureDelta:
					thinkingSignature = delta.Signature
				case anthropic.TextDelta:
					events <- TextDelta{Text: delta.Text}
				case anthropic.InputJSONDelta:
					// 工具调用的参数是 JSON，模型会分块输出，拼起来才能解析
					jsonAccum += delta.PartialJSON
					events <- ToolCallDelta{Text: delta.PartialJSON}
				}
			case anthropic.ContentBlockStopEvent:
				// 块维度的结束事件，可能是 thinking，也可能是 tool_use
				if inThinking {
					events <- ThinkingComplete{
						Thinking:  thinkingAccum,
						Signature: thinkingSignature,
					}
					inThinking = false
				}
				if currentToolName != "" {
					var args map[string]any
					if jsonAccum != "" {
						json.Unmarshal([]byte(jsonAccum), &args)
					}
					if args == nil {
						args = map[string]any{}
					}
					// 发事件通知工具调用完成，包含工具名、ID和参数
					events <- ToolCallComplete{
						ToolID:    currentToolID,
						ToolName:  currentToolName,
						Arguments: args,
					}
					currentToolName = ""
					currentToolID = ""
					jsonAccum = ""
				}
			}

			go readNext()
		}

		if err := stream.Err(); err != nil {
			errs <- classifyAnthropicError(err)
			return
		}

		stopReason := string(accMessage.StopReason)
		if stopReason == "" {
			stopReason = "end_turn"
		}
		usage := UsageInfo{
			InputTokens:         int(accMessage.Usage.InputTokens),
			OutputTokens:        int(accMessage.Usage.OutputTokens),
			CacheReadTokens:     int(accMessage.Usage.CacheReadInputTokens),
			CacheCreationTokens: int(accMessage.Usage.CacheCreationInputTokens),
		}
		events <- StreamEnd{StopReason: stopReason, Usage: usage}
	}()

	return events, errs
}

// markLastUserTailForCache 给最后一条 user 角色消息的最后一个内容块附加一个
// 临时 cache_control 标记。Anthropic 会缓存到（含）该块为止的前缀；后续请求只要
// 前缀字节一致就能命中缓存。该断点之后的 tool_result 内容保持字节稳定，因为
// toolresult 预算在摄取时最终确定每条消息，之后不会再重写历史。
//
// 原地修改 `messages`。若没有 user 消息，或最后一条 user 消息没有可标记的
// 内容块，则为空操作。
// Anthropic 的缓存机制是基于“前缀缓存”的。
// 也就是说，如果你把一段消息内容标记成可缓存，那么后续请求里，如果前面那一段内容和之前请求几乎一样，模型就可以复用缓存，而不用重新算一遍。
func markLastUserTailForCache(messages []anthropic.MessageParam) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != anthropic.MessageParamRoleUser {
			continue
		}
		blocks := messages[i].Content
		if len(blocks) == 0 {
			return
		}
		last := &blocks[len(blocks)-1]
		switch {
		case last.OfText != nil:
			last.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
		case last.OfToolResult != nil:
			last.OfToolResult.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		return
	}
}

func buildAnthropicMessages(messages []conversation.Message) []anthropic.MessageParam {
	var result []anthropic.MessageParam
	for _, m := range messages {
		if m.Role == "assistant" {
			var blocks []anthropic.ContentBlockParamUnion
			for _, tb := range m.ThinkingBlocks {
				blocks = append(blocks, anthropic.NewThinkingBlock(tb.Signature, tb.Thinking))
			}
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tu := range m.ToolUses {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tu.ToolUseID,
						Name:  tu.ToolName,
						Input: tu.Arguments,
					},
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropic.NewTextBlock(""))
			}
			result = append(result, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: blocks,
			})
		} else if len(m.ToolResults) > 0 {
			var blocks []anthropic.ContentBlockParamUnion
			for _, tr := range m.ToolResults {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: tr.ToolUseID,
						IsError:   param.NewOpt(tr.IsError),
						Content: []anthropic.ToolResultBlockParamContentUnion{{
							OfText: &anthropic.TextBlockParam{Text: tr.Content},
						}},
					},
				})
			}
			result = append(result, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: blocks,
			})
		} else {
			// 合并连续的用户文本消息，以保持用户和助手消息交替出现。
			// 将连续的 user 文本消息合并，以维持 user/assistant 消息交替。
			canMerge := false
			if n := len(result); n > 0 {
				prev := result[n-1]
				if prev.Role == anthropic.MessageParamRoleUser && len(prev.Content) > 0 && prev.Content[0].OfToolResult == nil {
					canMerge = true
				}
			}
			if canMerge {
				result[len(result)-1].Content = append(result[len(result)-1].Content, anthropic.NewTextBlock(m.Content))
			} else {
				result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
			}
		}
	}
	return result
}

func classifyAnthropicError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 413 || strings.Contains(apiErr.Error(), "prompt is too long") {
			return &ContextTooLongError{Message: fmt.Sprintf("Context too long: %s", apiErr.Error())}
		}
		switch apiErr.Type() {
		case anthropic.ErrorTypeAuthenticationError:
			return &AuthenticationError{Message: fmt.Sprintf("Invalid API key: %s", apiErr.Error())}
		case anthropic.ErrorTypeRateLimitError:
			retry := ""
			if apiErr.Response != nil {
				retry = apiErr.Response.Header.Get("Retry-After")
			}
			msg := "Rate limited."
			if retry != "" {
				msg += fmt.Sprintf(" Retry after %ss.", retry)
			} else {
				msg += " Please wait."
			}
			return &RateLimitError{Message: msg, RetryAfter: retry}
		default:
			return &LLMError{Message: fmt.Sprintf("API error (%d): %s", apiErr.StatusCode, apiErr.Error())}
		}
	}
	return &NetworkError{Message: fmt.Sprintf("Network error: %s", err.Error())}
}

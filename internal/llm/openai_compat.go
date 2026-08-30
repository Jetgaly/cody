package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"cody/internal/config"
	"cody/internal/conversation"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
)

const openaiCompatStreamIdleTimeout = 5 * time.Minute

type openaiCompatClient struct {
	client       openai.Client
	model        string
	systemPrompt string
}

func newOpenAICompatClient(cfg *config.ProviderConfig, systemPrompt string) (*openaiCompatClient, error) {
	apiKey := cfg.ResolveAPIKey()
	if apiKey == "" {
		return nil, &AuthenticationError{
			Message: "OpenAI-compatible API key not found. Set it in .cody/config.yaml or via OPENAI_API_KEY env var.",
		}
	}

	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithBaseURL(cfg.BaseURL),
	}
	if os.Getenv("CODY_LLM_DEBUG") != "" {
		opts = append(opts, option.WithDebugLog(log.New(os.Stderr, "[llm] ", log.LstdFlags)))
	}
	client := openai.NewClient(opts...)

	return &openaiCompatClient{
		client:       client,
		model:        cfg.Model,
		systemPrompt: systemPrompt,
	}, nil
}

func (c *openaiCompatClient) SetSystemPrompt(prompt string) {
	c.systemPrompt = prompt
}

func (c *openaiCompatClient) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan StreamEvent, <-chan error) {
	events := make(chan StreamEvent, 64)
	errs := make(chan error, 1)

	// 发请求前补齐工具调用与结果的配对，理由同 Anthropic 分支
	messages := buildChatCompletionMessages(c.systemPrompt, conversation.EnsureToolPairing(conv.GetMessages()))

	var tools []openai.ChatCompletionToolParam
	for _, s := range toolSchemas {
		name, _ := s["name"].(string)
		desc, _ := s["description"].(string)
		params, _ := s["parameters"].(map[string]any)
		tools = append(tools, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        name,
				Description: param.NewOpt(desc),
				Parameters:  shared.FunctionParameters(params),
				Strict:      param.NewOpt(false),
			},
		})
	}

	go func() {
		defer close(events)
		defer close(errs)

		reqParams := openai.ChatCompletionNewParams{
			Model:    c.model,
			Messages: messages,
			StreamOptions: openai.ChatCompletionStreamOptionsParam{
				IncludeUsage: param.NewOpt(true),
			},
		}
		if len(tools) > 0 {
			reqParams.Tools = tools
		}

		stream := c.client.Chat.Completions.NewStreaming(ctx, reqParams)
		defer stream.Close()

		// 跟踪跨多个 chunk 逐步拼装的工具调用。
		// Chat Completions API 会增量发送工具调用信息：
		// 某个 index 的第一个 chunk 携带 ID 和函数名，
		// 后续 chunk 携带参数片段。
		type toolCallAccum struct {
			id       string
			name     string
			argsJSON string
		}
		toolCalls := make(map[int64]*toolCallAccum)
		var reasoningAccum string

		// 在单独的 goroutine 中读取 SSE 事件，以便响应 ctx 取消
		// 并检测静默的连接断开，与 openai Responses 客户端采用相同模式。
		type sseResult struct {
			hasNext bool
		}
		nextCh := make(chan sseResult, 1)

		readNext := func() {
			nextCh <- sseResult{hasNext: stream.Next()}
		}

		idle := time.NewTimer(openaiCompatStreamIdleTimeout)
		defer idle.Stop()

		go readNext()
		for {
			var res sseResult
			select {
			case <-ctx.Done():
				errs <- &NetworkError{Message: fmt.Sprintf("context cancelled: %v", ctx.Err())}
				return
			case <-idle.C:
				errs <- &NetworkError{Message: fmt.Sprintf("stream idle timeout: no SSE events for %s", openaiCompatStreamIdleTimeout)}
				return
			case res = <-nextCh:
			}

			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(openaiCompatStreamIdleTimeout)

			if !res.hasNext {
				break
			}

			chunk := stream.Current()

			// 先处理 choices。大多数 provider（OpenAI 官方）把 choices 与
			// 仅含 usage 的 chunk 分开发送，但有些（如讯飞 MaaS）会把最终的
			// finish_reason chunk 和 usage 放在同一个 SSE 事件里——因此即使
			// 已有 usage，我们也必须运行 choices 处理器，否则工具调用的完成
			// 会被跳过，agent 循环会误以为该轮没有工具调用。
			var finishReason string
			if len(chunk.Choices) > 0 {
				choice := chunk.Choices[0]
				delta := choice.Delta
				finishReason = choice.FinishReason

				if delta.Content != "" {
					events <- TextDelta{Text: delta.Content}
				}

				// DeepSeek/小米等 provider 在 Chat Completions delta 中用非标准字段
				// reasoning_content 传输思考内容，SDK 未直接建模，从 ExtraFields 提取。
				if rc, ok := delta.JSON.ExtraFields["reasoning_content"]; ok && rc.Valid() {
					raw := rc.Raw()
					if len(raw) >= 2 && raw[0] == '"' {
						var text string
						if json.Unmarshal([]byte(raw), &text) == nil && text != "" {
							reasoningAccum += text
							events <- ThinkingDelta{Text: text}
						}
					}
				}

				for _, tc := range delta.ToolCalls {
					acc, exists := toolCalls[tc.Index]
					if !exists {
						acc = &toolCallAccum{}
						toolCalls[tc.Index] = acc
					}
					if tc.ID != "" {
						acc.id = tc.ID
					}
					if tc.Function.Name != "" {
						acc.name = tc.Function.Name
						events <- ToolCallStart{ToolName: acc.name, ToolID: acc.id}
					}
					if tc.Function.Arguments != "" {
						acc.argsJSON += tc.Function.Arguments
						events <- ToolCallDelta{Text: tc.Function.Arguments}
					}
				}

				if finishReason == "tool_calls" || finishReason == "stop" {
					if reasoningAccum != "" {
						events <- ThinkingComplete{Thinking: reasoningAccum}
						reasoningAccum = ""
					}
					for _, acc := range toolCalls {
						var args map[string]any
						if acc.argsJSON != "" {
							json.Unmarshal([]byte(acc.argsJSON), &args)
						}
						if args == nil {
							args = map[string]any{}
						}
						events <- ToolCallComplete{
							ToolID:    acc.id,
							ToolName:  acc.name,
							Arguments: args,
						}
					}
					toolCalls = make(map[int64]*toolCallAccum)
				}
			}

			// Usage（对某些 provider 可能与 finish_reason 在同一 chunk 中，
			// 对另一些则会在末尾单独的 usage-only chunk 中到达）。
			if chunk.JSON.Usage.Valid() && chunk.Usage.PromptTokens != 0 {
				cached := int(chunk.Usage.PromptTokensDetails.CachedTokens)
				input := int(chunk.Usage.PromptTokens) - cached
				if input < 0 {
					input = 0
				}
				stopReason := "end_turn"
				if finishReason == "tool_calls" {
					stopReason = "tool_use"
				}
				events <- StreamEnd{
					StopReason: stopReason,
					Usage: UsageInfo{
						InputTokens:     input,
						OutputTokens:    int(chunk.Usage.CompletionTokens),
						CacheReadTokens: cached,
					},
				}
			} else if finishReason == "stop" || finishReason == "tool_calls" {
				// provider 结束了这一轮但未发送 usage——发出 StreamEnd，
				// 以免 agent 循环一直挂着等待。
				stopReason := "end_turn"
				if finishReason == "tool_calls" {
					stopReason = "tool_use"
				}
				events <- StreamEnd{StopReason: stopReason, Usage: UsageInfo{}}
			}

			go readNext()
		}

		if err := stream.Err(); err != nil {
			errs <- classifyOpenAIError(err)
		}
	}()

	return events, errs
}

// buildChatCompletionMessages 把会话历史转换为 Chat Completions 消息格式。
// 系统提示会成为开头的 system 消息。
// 对于支持 reasoning_content 的 provider（如 DeepSeek、小米），thinking blocks
// 会作为 assistant 消息的 reasoning_content 字段回传。
func buildChatCompletionMessages(systemPrompt string, messages []conversation.Message) []openai.ChatCompletionMessageParamUnion {
	var result []openai.ChatCompletionMessageParamUnion

	// 系统提示作为第一条消息
	if systemPrompt != "" {
		result = append(result, openai.SystemMessage(systemPrompt))
	}

	for _, m := range messages {
		if m.Role == "assistant" {
			// 拼接 thinking blocks 为 reasoning_content，供 DeepSeek 等 provider 使用。
			var reasoning string
			for _, tb := range m.ThinkingBlocks {
				reasoning += tb.Thinking
			}

			if len(m.ToolUses) > 0 {
				assistant := openai.ChatCompletionAssistantMessageParam{}
				if m.Content != "" {
					assistant.Content.OfString = param.NewOpt(m.Content)
				}
				for _, tu := range m.ToolUses {
					argsJSON, _ := json.Marshal(tu.Arguments)
					assistant.ToolCalls = append(assistant.ToolCalls, openai.ChatCompletionMessageToolCallParam{
						ID: tu.ToolUseID,
						Function: openai.ChatCompletionMessageToolCallFunctionParam{
							Name:      tu.ToolName,
							Arguments: string(argsJSON),
						},
					})
				}
				if reasoning != "" {
					assistant.SetExtraFields(map[string]any{"reasoning_content": reasoning})
				}
				result = append(result, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
			} else if m.Content != "" || reasoning != "" {
				assistant := openai.ChatCompletionAssistantMessageParam{}
				if m.Content != "" {
					assistant.Content.OfString = param.NewOpt(m.Content)
				}
				if reasoning != "" {
					assistant.SetExtraFields(map[string]any{"reasoning_content": reasoning})
				}
				result = append(result, openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant})
			}
		} else if len(m.ToolResults) > 0 {
			// 工具结果转换为独立的 tool 消息
			for _, tr := range m.ToolResults {
				result = append(result, openai.ToolMessage(tr.Content, tr.ToolUseID))
			}
		} else {
			// 用户消息
			result = append(result, openai.UserMessage(m.Content))
		}
	}

	return result
}

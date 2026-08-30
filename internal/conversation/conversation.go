package conversation

import (
	"strings"
	"time"
)

type ToolUseBlock struct {
	ToolUseID string
	ToolName  string
	Arguments map[string]any
}

type ToolResultBlock struct {
	ToolUseID string
	Content   string
	IsError   bool
}

type ThinkingBlock struct {
	Thinking  string
	Signature string
}

type Message struct {
	Role           string
	Content        string
	ThinkingBlocks []ThinkingBlock
	ToolUses       []ToolUseBlock
	ToolResults    []ToolResultBlock
}

// 
type Manager struct {
	history        []Message
	ltmInjected    bool
	baselineTokens int
	anchorCount    int
	hasUsage       bool
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) AddUserMessage(content string) {
	m.history = append(m.history, Message{Role: "user", Content: content})
}

func (m *Manager) AddAssistantMessage(content string) {
	m.history = append(m.history, Message{Role: "assistant", Content: content})
}

func (m *Manager) AddToolUseMessage(text, toolUseID, toolName string, arguments map[string]any) {
	m.history = append(m.history, Message{
		Role:    "assistant",
		Content: text,
		ToolUses: []ToolUseBlock{{
			ToolUseID: toolUseID,
			ToolName:  toolName,
			Arguments: arguments,
		}},
	})
}

func (m *Manager) AddAssistantMessageWithTools(text string, toolUses []ToolUseBlock) {
	m.history = append(m.history, Message{
		Role:     "assistant",
		Content:  text,
		ToolUses: toolUses,
	})
}

func (m *Manager) AddAssistantFull(text string, thinking []ThinkingBlock, toolUses []ToolUseBlock) {
	m.history = append(m.history, Message{
		Role:           "assistant",
		Content:        text,
		ThinkingBlocks: thinking,
		ToolUses:       toolUses,
	})
}

func (m *Manager) AddToolResultMessage(toolUseID, content string, isError bool) {
	m.history = append(m.history, Message{
		Role: "user",
		ToolResults: []ToolResultBlock{{
			ToolUseID: toolUseID,
			Content:   content,
			IsError:   isError,
		}},
	})
}

func (m *Manager) AddToolResultsMessage(results []ToolResultBlock) {
	m.history = append(m.history, Message{
		Role:        "user",
		ToolResults: results,
	})
}

func (m *Manager) AddSystemReminder(content string) {
	m.history = append(m.history, Message{
		Role:    "user",
		Content: "<system-reminder>\n" + content + "\n</system-reminder>",
	})
}

func (m *Manager) InjectLongTermMemory(instructions, memories string) {
	if m.ltmInjected {
		return
	}
	var sections []string
	if instructions != "" {
		sections = append(sections, "# codyMd\nCodebase and user instructions are shown below. Be sure to adhere to these instructions. IMPORTANT: These instructions OVERRIDE any default behavior and you MUST follow them exactly as written.\n\n"+instructions)
	}
	if memories != "" {
		sections = append(sections, "# autoMemory\n"+memories)
	}
	if len(sections) == 0 {
		return
	}
	sections = append(sections, "# currentDate\nToday's date is "+time.Now().Format("2006-01-02")+".")
	body := strings.Join(sections, "\n\n")
	wrapped := "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n" +
		body +
		"\n\n      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.\n</system-reminder>"
	m.history = append([]Message{{Role: "user", Content: wrapped}}, m.history...)
	m.ltmInjected = true
}

// AppendMessages 将给定的消息追加到历史记录的末尾。压缩（compaction）用它
// 在摘要之后原样重放最近的尾部消息。
func (m *Manager) AppendMessages(messages []Message) {
	m.history = append(m.history, messages...)
}

func (m *Manager) Len() int {
	return len(m.history)
}

func (m *Manager) TruncateTo(index int) {
	if index < 0 {
		index = 0
	}
	if index > len(m.history) {
		return
	}
	m.history = m.history[:index]
}

func (m *Manager) GetMessages() []Message {
	result := make([]Message, len(m.history))
	copy(result, m.history)
	return result
}

// ReplaceToolResults 就地替换指定消息的 ToolResults 列表。
// 用于 Layer 1 tool-result budget 的 Design A 实现：直接修改原始
// 对话历史，而非生成新的副本。msgIndex 越界时静默忽略。
func (m *Manager) ReplaceToolResults(msgIndex int, newResults []ToolResultBlock) {
	if msgIndex < 0 || msgIndex >= len(m.history) {
		return
	}
	m.history[msgIndex].ToolResults = newResults
}

// RecordUsageAnchor 锚定本轮 API 返回的真实 token 用量。
// 调用时机：assistant 消息已追加到 history 之后。
func (m *Manager) RecordUsageAnchor(input, output, cacheRead, cacheCreation int) {
	baseline := input + cacheRead + cacheCreation + output
	if baseline <= 0 {
		return
	}
	m.baselineTokens = baseline
	m.anchorCount = len(m.history)
	m.hasUsage = true
}

// ClearUsageAnchor 压缩后清零锚点，下次估算回退到全量字符估算。
func (m *Manager) ClearUsageAnchor() {
	m.baselineTokens = 0
	m.anchorCount = 0
	m.hasUsage = false
}

// UsageAnchorState 返回当前锚点状态，供 compact 层读取。
func (m *Manager) UsageAnchorState() (baselineTokens, anchorCount int, hasUsage bool) {
	return m.baselineTokens, m.anchorCount, m.hasUsage
}

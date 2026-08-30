package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cody/internal/conversation"
)

// TypeCompactBoundary 把会话记录标记为压缩边界，而不是普通对话消息。
// 边界记录的 Content 保存一个 JSON blob（见 CompactBoundary），其中携带
// 摘要文本以及压缩时原样保留的近期尾部（keep）。普通消息的 Type 为空
// （omitempty），因此旧会话和普通轮次不受影响。
const TypeCompactBoundary = "compact_boundary"

// ToolUseRecord 是落盘形式的工具调用。这里存的是与协议无关的内部表示，
// 而不是某一家厂商的线格式，因此恢复会话时即使换了 provider 也能还原。
type ToolUseRecord struct {
	ToolUseID string         `json:"tool_use_id"`
	ToolName  string         `json:"tool_name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ToolResultRecord 是落盘形式的工具结果，与 ToolUseRecord 通过 ToolUseID 配对。
type ToolResultRecord struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Message 是会话记录的落盘形式。
type Message struct {
	Role string `json:"role"`
	// Type 区分记录的种类。为空（默认值，从 JSON 中省略）表示普通对话消息；
	// TypeCompactBoundary 表示 Content 是由 SaveCompactBoundary 写入的
	// CompactBoundary JSON blob。
	Type    string `json:"type,omitempty"`
	Content string `json:"content"`
	Ts      int64  `json:"ts"`
	// ToolUses / ToolResults 保存这条消息携带的工具块。两者为空时整个字段从 JSON
	// 中省略，因此不含这两个字段的会话文件依然能正常读出，只是没有工具链。
	ToolUses    []ToolUseRecord    `json:"tool_uses,omitempty"`
	ToolResults []ToolResultRecord `json:"tool_results,omitempty"`
}

// FromConversation 把内存中的对话消息转成落盘形式。
// 思考块不落盘：它的 signature 只在同一轮工具循环内需要回传，跨会话恢复用不上。
func FromConversation(msg conversation.Message) Message {
	rec := Message{
		Role:    msg.Role,
		Content: msg.Content,
		Ts:      time.Now().Unix(),
	}
	for _, tu := range msg.ToolUses {
		rec.ToolUses = append(rec.ToolUses, ToolUseRecord{
			ToolUseID: tu.ToolUseID,
			ToolName:  tu.ToolName,
			Arguments: tu.Arguments,
		})
	}
	for _, tr := range msg.ToolResults {
		rec.ToolResults = append(rec.ToolResults, ToolResultRecord{
			ToolUseID: tr.ToolUseID,
			Content:   tr.Content,
			IsError:   tr.IsError,
		})
	}
	return rec
}

// ToConversation 把落盘记录还原成内存中的对话消息，供 resume 重建历史。
func (m Message) ToConversation() conversation.Message {
	msg := conversation.Message{
		Role:    m.Role,
		Content: m.Content,
	}
	for _, tu := range m.ToolUses {
		msg.ToolUses = append(msg.ToolUses, conversation.ToolUseBlock{
			ToolUseID: tu.ToolUseID,
			ToolName:  tu.ToolName,
			Arguments: tu.Arguments,
		})
	}
	for _, tr := range m.ToolResults {
		msg.ToolResults = append(msg.ToolResults, conversation.ToolResultBlock{
			ToolUseID: tr.ToolUseID,
			Content:   tr.Content,
			IsError:   tr.IsError,
		})
	}
	return msg
}

// KeepMessage 是压缩发生时原样保留下来的一条近期消息。与 Message 一样携带
// 工具块，压缩后恢复会话时这段尾巴才不会缺掉工具调用链。
type KeepMessage struct {
	Role        string             `json:"role"`
	Content     string             `json:"content"`
	ToolUses    []ToolUseRecord    `json:"tool_uses,omitempty"`
	ToolResults []ToolResultRecord `json:"tool_results,omitempty"`
}

// FromConversationKeep 把保留下来的尾巴消息转成落盘形式。
func FromConversationKeep(msg conversation.Message) KeepMessage {
	rec := FromConversation(msg)
	return KeepMessage{
		Role:        rec.Role,
		Content:     rec.Content,
		ToolUses:    rec.ToolUses,
		ToolResults: rec.ToolResults,
	}
}

// CompactBoundary 是存储在 TypeCompactBoundary 记录 Content 中（以 JSON 形式）
// 的结构化负载。Summary 是 LLM 生成的较旧前缀摘要；Keep 是原样保留的近期尾部。
// 恢复会话时，压缩后的状态按如下方式重建：[user 消息 = Summary] + Keep +
// 边界之后追加的任何普通消息。
type CompactBoundary struct {
	Summary string        `json:"summary"`
	Keep    []KeepMessage `json:"keep"`
}

// SaveCompactBoundary 向会话日志追加一条压缩边界记录。该边界是只追加的：
// 原始的前缀消息仍留在文件中，但恢复会话时不会重放（见 FindLastCompactBoundary）。
// 摘要 + keep 会作为 CompactBoundary JSON blob 内联到记录的 Content 中。
func SaveCompactBoundary(workDir, sessionID, summary string, keep []KeepMessage) {
	// blob 是 CompactBoundary 的 JSON 序列化结果，落盘时存入 Message.Content
	
	blob, err := json.Marshal(CompactBoundary{Summary: summary, Keep: keep})
	if err != nil {
		return
	}
	SaveMessage(workDir, sessionID, Message{
		Role:    "system",
		Type:    TypeCompactBoundary,
		Content: string(blob),
		Ts:      time.Now().Unix(),
	})
}

// FindLastCompactBoundary 扫描已加载的记录，找到最后一次压缩边界。
// 它返回解析出的边界、该边界之后追加的普通消息切片，并在找到边界时返回 ok=true。
// 当不存在边界（ok=false）时，调用方应原样重放所有记录
// （向后兼容：旧会话没有边界记录）。
func FindLastCompactBoundary(msgs []Message) (boundary CompactBoundary, after []Message, ok bool) {
	last := -1
	for i, m := range msgs {
		if m.Type == TypeCompactBoundary {
			last = i
		}
	}
	if last < 0 {
		return CompactBoundary{}, nil, false
	}
	if err := json.Unmarshal([]byte(msgs[last].Content), &boundary); err != nil {
		// 边界 blob 损坏——回退到完整重放，以免丢失对话。
		return CompactBoundary{}, nil, false
	}
	for _, m := range msgs[last+1:] {
		if m.Type == TypeCompactBoundary {
			continue // 防御性处理；FindLast 已经定位到最后一个边界
		}
		after = append(after, m)
	}
	return boundary, after, true
}

type SessionInfo struct {
	ID           string
	FirstMessage string
	MessageCount int
	FileSize     int64
	GitBranch    string
	ModTime      time.Time
}

func NewID() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 极少失败；兜底用纳秒低 16 位，仍能避免同秒同进程冲突
		return fmt.Sprintf("%s-%04x", time.Now().Format("20060102-150405"), time.Now().UnixNano()&0xFFFF)
	}
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func sessionsDir(workDir string) string {
	return filepath.Join(workDir, ".cody", "sessions")
}

func SessionFilePath(workDir, id string) string {
	return filepath.Join(sessionsDir(workDir), id+".jsonl")
}

func SaveMessage(workDir, sessionID string, msg Message) {
	dir := sessionsDir(workDir)
	os.MkdirAll(dir, 0o755)

	f, err := os.OpenFile(SessionFilePath(workDir, sessionID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	data, _ := json.Marshal(msg)
	f.Write(data)
	f.Write([]byte("\n"))
}

func LoadSession(workDir, sessionID string) []Message {
	path := SessionFilePath(workDir, sessionID)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var msgs []Message
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		var msg Message
		if json.Unmarshal(scanner.Bytes(), &msg) != nil {
			continue
		}
		// 只带工具结果的消息本身没有文本内容，不能按 Content 是否为空来过滤，
		// 否则整条工具往返都会在恢复会话时被丢掉。
		if msg.Content == "" && len(msg.ToolUses) == 0 && len(msg.ToolResults) == 0 {
			continue
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// maxSessionAgeDays 是会话的最大保留天数，超过此天数的会话会被自动清理。
const maxSessionAgeDays = 30

func ListSessions(workDir string) []SessionInfo {
	dir := sessionsDir(workDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	branch := currentGitBranch(workDir)
	cutoff := time.Now().AddDate(0, 0, -maxSessionAgeDays)

	var sessions []SessionInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		info, err := e.Info()
		if err != nil {
			continue
		}

		// 自动清理超过 30 天的过期会话
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
			continue
		}

		msgs := LoadSession(workDir, id)
		first := ""
		for _, msg := range msgs {
			if msg.Role == "user" {
				first = msg.Content
				break
			}
		}

		sessions = append(sessions, SessionInfo{
			ID:           id,
			FirstMessage: first,
			MessageCount: len(msgs),
			FileSize:     info.Size(),
			GitBranch:    branch,
			ModTime:      info.ModTime(),
		})
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})

	return sessions
}

func currentGitBranch(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func FormatRelativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		weeks := int(d.Hours() / 24 / 7)
		if weeks == 1 {
			return "1 week ago"
		}
		return fmt.Sprintf("%d weeks ago", weeks)
	}
}

func FormatFileSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		kb := float64(bytes) / 1024
		if kb == float64(int(kb)) {
			return fmt.Sprintf("%.0fKB", kb)
		}
		return fmt.Sprintf("%.1fKB", kb)
	default:
		mb := float64(bytes) / 1024 / 1024
		return fmt.Sprintf("%.1fMB", mb)
	}
}

func MatchesSearch(s SessionInfo, query string) bool {
	if query == "" {
		return true
	}
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(s.FirstMessage), q) ||
		strings.Contains(strings.ToLower(s.ID), q)
}

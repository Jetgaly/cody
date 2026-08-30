package teams

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TeamFile 是团队配置在磁盘上的形态，落在 <teamsBaseDir>/<slug>/config.json。
//
// 内存里的 Member 挂着 agent 实例、conversation 和 cancel 函数，这些都没法序列化，
// 所以落盘的是另一份纯元信息的结构。两边靠成员名字对应。
//
// 这份文件解决的是跨进程和跨重启：窗格队员是独立进程，起来之后要知道自己在哪个团队、
// 队友都有谁；用户重启 Cody 之后也得能接着用之前的团队。
type TeamFile struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	CreatedAt   int64            `json:"createdAt"`
	LeadAgentID string           `json:"leadAgentId"`
	Members     []TeamMemberFile `json:"members"`
}

// TeamMemberFile 是单个成员的元信息。isActive 用指针是为了区分三种状态：
// 没有这个字段表示刚注册还没开工，true 表示在跑，false 表示已空闲。
type TeamMemberFile struct {
	AgentID      string `json:"agentId"`
	Name         string `json:"name"`
	AgentType    string `json:"agentType,omitempty"`
	Model        string `json:"model,omitempty"`
	JoinedAt     int64  `json:"joinedAt"`
	WorktreePath string `json:"worktreePath,omitempty"`
	BackendType  string `json:"backendType,omitempty"`
	IsActive     *bool  `json:"isActive,omitempty"`
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// sanitizeTeamName 把团队名压成可以直接当目录名的形式，非字母数字一律换成连字符再转小写。
// 团队名是 LLM 起的，可能带空格、中文和标点，不处理会在不同文件系统上炸出各种问题。
func sanitizeTeamName(name string) string {
	return strings.ToLower(nonAlnum.ReplaceAllString(name, "-"))
}

func teamFilePath(name string) string {
	return filepath.Join(teamDir(name), "config.json")
}

// ReadTeamFile 读取团队配置。文件不存在返回 (nil, nil)，让调用方按「没有这个团队」处理，
// 而不是当成错误往上抛。
func ReadTeamFile(name string) (*TeamFile, error) {
	data, err := os.ReadFile(teamFilePath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var tf TeamFile
	if err := json.Unmarshal(data, &tf); err != nil {
		return nil, err
	}
	return &tf, nil
}

// WriteTeamFile 写入团队配置，目录不存在会一并创建。
func WriteTeamFile(name string, tf *TeamFile) error {
	dir := teamDir(name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(teamFilePath(name), data, 0o644)
}

// snapshot 把内存里的 Team 导出成可落盘的 TeamFile。
// 调用方需要自己持有 t.mu。
func (t *Team) snapshot() *TeamFile {
	tf := &TeamFile{
		Name:        t.Name,
		Description: t.Description,
		CreatedAt:   t.CreatedAt,
		LeadAgentID: t.LeadAgentID,
		Members:     make([]TeamMemberFile, 0, len(t.Members)),
	}
	if tf.CreatedAt == 0 {
		tf.CreatedAt = time.Now().Unix()
	}
	for _, m := range t.Members {
		active := m.Active
		tf.Members = append(tf.Members, TeamMemberFile{
			AgentID:      m.AgentID,
			Name:         m.Name,
			AgentType:    m.AgentType,
			Model:        m.Model,
			JoinedAt:     m.JoinedAt,
			WorktreePath: m.WorktreePath,
			BackendType:  string(t.Mode),
			IsActive:     &active,
		})
	}
	return tf
}

// persist 把当前状态写回磁盘。写失败不影响内存里的团队继续工作，
// 所以这里只吞掉错误：落盘是为了跨进程和跨重启，不是运行时的必要条件。
// 调用方需要自己持有 t.mu。
func (t *Team) persist() {
	_ = WriteTeamFile(t.Name, t.snapshot())
}

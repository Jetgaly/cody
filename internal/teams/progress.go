package teams

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// ToolActivity 描述一次工具调用。
type ToolActivity struct {
	ToolName    string
	Description string
}

// NewToolActivity 创建一个带自动生成描述的 ToolActivity。
func NewToolActivity(toolName string, input map[string]interface{}) ToolActivity {
	return ToolActivity{
		ToolName:    toolName,
		Description: describeActivity(toolName, input),
	}
}

func describeActivity(toolName string, input map[string]interface{}) string {
	getStr := func(key string) string {
		if v, ok := input[key]; ok {
			return fmt.Sprintf("%v", v)
		}
		return ""
	}
	switch toolName {
	case "ReadFile":
		return "Reading " + getStr("file_path")
	case "EditFile":
		return "Editing " + getStr("file_path")
	case "WriteFile":
		return "Writing " + getStr("file_path")
	case "Bash":
		cmd := getStr("command")
		if len(cmd) > 40 {
			cmd = cmd[:40] + "…"
		}
		return "Running " + cmd
	case "Glob":
		return "Searching " + getStr("pattern")
	case "Grep":
		return "Grepping " + getStr("pattern")
	default:
		return toolName
	}
}

// TeammateProgress 跟踪单个队友的实时进度。
// 所有方法都是 goroutine 安全的。
type TeammateProgress struct {
	mu               sync.Mutex
	Name             string
	TeamName         string
	Status           string // "running", "idle", "completed", "failed", "stopped"
	ToolUseCount     int
	TokenCount       int64
	LastActivity     *ToolActivity
	RecentActivities []ToolActivity // 最多 5 条
	SpinnerVerb      string
	StartTime        int64 // unix 毫秒
	LastMessage      string
}

func NewTeammateProgress(name, teamName, spinnerVerb string) *TeammateProgress {
	return &TeammateProgress{
		Name:        name,
		TeamName:    teamName,
		Status:      "running",
		SpinnerVerb: spinnerVerb,
		StartTime:   time.Now().UnixMilli(),
	}
}

func (p *TeammateProgress) RecordToolUse(toolName string, input map[string]interface{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ToolUseCount++
	act := NewToolActivity(toolName, input)
	p.LastActivity = &act
	p.RecentActivities = append(p.RecentActivities, act)
	if len(p.RecentActivities) > 5 {
		p.RecentActivities = p.RecentActivities[1:]
	}
}

func (p *TeammateProgress) RecordTokens(input, output int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.TokenCount = input + output
}

func (p *TeammateProgress) SetStatus(s string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Status = s
}

func (p *TeammateProgress) GetStatus() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Status
}

func (p *TeammateProgress) ActivitySummary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.LastActivity != nil {
		return p.LastActivity.Description
	}
	return p.SpinnerVerb
}

var spinnerVerbs = []string{
	"Accomplishing", "Architecting", "Baking", "Bootstrapping", "Brewing",
	"Calculating", "Churning", "Cogitating", "Composing", "Computing",
	"Concocting", "Contemplating", "Cooking", "Crafting", "Creating",
	"Crunching", "Crystallizing", "Cultivating", "Deliberating", "Enchanting",
	"Envisioning", "Forging", "Generating", "Harmonizing", "Hatching",
	"Ideating", "Imagining", "Incubating", "Inferring", "Manifesting",
	"Newing", "Mulling", "Musing", "Orchestrating", "Pondering",
	"Purring", "Ruminating", "Simmering", "Sketching", "Synthesizing",
	"Thinking", "Tinkering", "Working", "Wrangling",
}

func randomVerb() string {
	return spinnerVerbs[rand.Intn(len(spinnerVerbs))]
}

func (p *TeammateProgress) GetToolUseCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ToolUseCount
}

func (p *TeammateProgress) GetTokenCount() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.TokenCount
}

// FormatTokens 把 token 数量格式化为便于显示的形式。
func FormatTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

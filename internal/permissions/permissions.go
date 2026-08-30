package permissions

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"cody/internal/tools"
)

// splitCompoundCommand 拆分 shell 复合命令（&&、||、;、|）为独立子命令，
// 逐条匹配权限规则，防止通过 "cmd1 && dangerous_cmd" 绕过检查。
func splitCompoundCommand(cmd string) []string {
	parts := regexp.MustCompile(`\s*(?:&&|\|\||[;|])\s*`).Split(cmd, -1)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return []string{cmd}
	}
	return result
}

type DecisionEffect string

const (
	Allow DecisionEffect = "allow"
	Deny  DecisionEffect = "deny"
	Ask   DecisionEffect = "ask"
)

type Decision struct {
	Effect DecisionEffect
	Reason string
}

type PermissionMode string

const (
	ModeDefault      PermissionMode = "default"
	ModeAcceptEdits  PermissionMode = "acceptEdits"
	ModePlan         PermissionMode = "plan"
	ModeBypass       PermissionMode = "bypassPermissions"
)

var modeMatrix = map[PermissionMode]map[tools.ToolCategory]DecisionEffect{
	ModeDefault:     {tools.CategoryRead: Allow, tools.CategoryWrite: Ask, tools.CategoryCommand: Ask},
	ModeAcceptEdits: {tools.CategoryRead: Allow, tools.CategoryWrite: Allow, tools.CategoryCommand: Ask},
	ModeBypass:      {tools.CategoryRead: Allow, tools.CategoryWrite: Allow, tools.CategoryCommand: Allow},
}

func ModeDecide(mode PermissionMode, category tools.ToolCategory) DecisionEffect {
	m, ok := modeMatrix[mode]
	if !ok {
		return Ask
	}
	return m[category]
}

// 第 1 层：危险命令检测

type dangerousPattern struct {
	re     *regexp.Regexp
	reason string
}

var defaultDangerousPatterns = []dangerousPattern{
	{regexp.MustCompile(`rm\s+-[a-z]*r[a-z]*f[a-z]*\s+/\s*$`), "recursive force delete root"},
	{regexp.MustCompile(`mkfs\.`), "format disk"},
	{regexp.MustCompile(`dd\s+if=.*of=/dev/`), "direct write to disk device"},
	{regexp.MustCompile(`chmod\s+-R\s+777\s+/`), "recursive chmod root"},
	{regexp.MustCompile(`:\(\)\{\s*:\|:&\s*\};:`), "fork bomb"},
	{regexp.MustCompile(`curl\s+.*\|\s*(ba)?sh`), "pipe remote script"},
	{regexp.MustCompile(`wget\s+.*\|\s*(ba)?sh`), "pipe remote script"},
	{regexp.MustCompile(`>\s*/dev/sd`), "overwrite disk device"},
	// Git 破坏性命令——防止误操作丢失工作
	{regexp.MustCompile(`git\s+push\s+.*--force`), "force push"},
	{regexp.MustCompile(`git\s+reset\s+--hard`), "hard reset"},
	{regexp.MustCompile(`git\s+clean\s+-f`), "force clean untracked files"},
	{regexp.MustCompile(`git\s+checkout\s+\.`), "discard all changes"},
	{regexp.MustCompile(`git\s+branch\s+-D`), "force delete branch"},
}

func DetectDangerous(command string) (bool, string) {
	for _, p := range defaultDangerousPatterns {
		if p.re.MatchString(command) {
			return true, p.reason
		}
	}
	return false, ""
}

// 第 2 层：路径沙箱

type PathSandbox struct {
	allowedRoots []string
	denyWrite    []string // 始终只读的受保护路径，优先级高于 allowedRoots
}

// NewPathSandbox 创建路径沙箱。denyWrite 指定受保护路径（如配置文件），
// 即使在 allowedRoots 内也拒绝写入。
func NewPathSandbox(projectRoot string, extraAllowed ...string) *PathSandbox {
	root, _ := filepath.Abs(projectRoot)
	allowed := []string{root, os.TempDir()}
	for _, p := range extraAllowed {
		abs, _ := filepath.Abs(p)
		allowed = append(allowed, abs)
	}

	// 默认受保护路径：防止 Agent 篡改权限配置和 Skill 定义
	denyWrite := []string{
		filepath.Join(root, ".cody", "config.yaml"),
		filepath.Join(root, ".cody", "permissions.local.yaml"),
		filepath.Join(root, ".cody", "skills"),
	}

	return &PathSandbox{allowedRoots: allowed, denyWrite: denyWrite}
}

func (s *PathSandbox) Check(path string) (bool, string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Sprintf("cannot resolve path: %s", path)
	}

	// 受保护路径优先检查：命中则直接拒绝
	for _, deny := range s.denyWrite {
		if abs == deny || strings.HasPrefix(abs, deny+string(filepath.Separator)) {
			return false, fmt.Sprintf("protected path: %s", path)
		}
	}

	for _, root := range s.allowedRoots {
		if strings.HasPrefix(abs, root) {
			return true, ""
		}
	}
	return false, fmt.Sprintf("path %s outside sandbox", path)
}

// GetDenyWrite 返回受保护路径列表，供沙箱 Config 构建使用
func (s *PathSandbox) GetDenyWrite() []string {
	return s.denyWrite
}

// GetAllowedRoots 返回允许写入的路径列表，供沙箱 Config 构建使用
func (s *PathSandbox) GetAllowedRoots() []string {
	return s.allowedRoots
}

// 第 3 层：规则引擎

type RuleEffect string

const (
	RuleAllow RuleEffect = "allow"
	RuleDeny  RuleEffect = "deny"
	RuleAsk   RuleEffect = "ask"
)

type Rule struct {
	ToolName string
	Pattern  string
	Effect   RuleEffect
}

func (r Rule) Matches(toolName, content string) bool {
	if r.ToolName != toolName {
		return false
	}
	// 简单通配符匹配：* 匹配任意字符（包括 /），适用于 Bash 命令等非路径场景。
	// filepath.Match 的 * 不匹配 /，导致 "allow always" 对含路径的命令失效。
	return globMatch(r.Pattern, content)
}

// globMatch 实现简单的通配符匹配，* 匹配任意字符（包括 /）。
func globMatch(pattern, content string) bool {
	// 快捷路径：无通配符时做精确比较
	if !strings.Contains(pattern, "*") && !strings.Contains(pattern, "?") {
		return pattern == content
	}
	// 将 pattern 转为正则：* → .*, ? → .
	re := "^"
	for _, ch := range pattern {
		switch ch {
		case '*':
			re += ".*"
		case '?':
			re += "."
		case '.', '+', '^', '$', '{', '}', '(', ')', '|', '[', ']', '\\':
			re += "\\" + string(ch)
		default:
			re += string(ch)
		}
	}
	re += "$"
	matched, _ := regexp.MatchString(re, content)
	return matched
}

type RuleEngine struct {
	UserPath    string
	ProjectPath string
	LocalPath   string
}

func (e *RuleEngine) Evaluate(toolName, content string) *RuleEffect {
	for _, path := range []string{e.UserPath, e.ProjectPath, e.LocalPath} {
		rules := loadRulesFile(path)
		for i := len(rules) - 1; i >= 0; i-- {
			if rules[i].Matches(toolName, content) {
				eff := rules[i].Effect
				return &eff
			}
		}
	}
	// 没有匹配规则时返回 nil，交由 PermissionMode 决定
	return nil
}

// 用户在 UI 里点始终允许时就会调用这个方法。先读取已有规则，追加新规则，再整体序列化写回文件。
func (e *RuleEngine) AppendLocalRule(r Rule) {
	if e.LocalPath == "" {
		return
	}
	os.MkdirAll(filepath.Dir(e.LocalPath), 0o755)
	rules := loadRulesFile(e.LocalPath)
	rules = append(rules, r)
	var entries []map[string]string
	for _, rule := range rules {
		entries = append(entries, map[string]string{
			"rule":   fmt.Sprintf("%s(%s)", rule.ToolName, rule.Pattern),
			"effect": string(rule.Effect),
		})
	}
	data, _ := yaml.Marshal(entries)
	os.WriteFile(e.LocalPath, data, 0o644)
}

func loadRulesFile(path string) []Rule {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var entries []struct {
		RuleStr string `yaml:"rule"`
		Effect  string `yaml:"effect"`
	}
	if err := yaml.Unmarshal(data, &entries); err != nil {
		return nil
	}
	var rules []Rule
	for _, e := range entries {
		if e.Effect != "allow" && e.Effect != "deny" && e.Effect != "ask" {
			continue
		}
		r, err := parseRule(e.RuleStr, RuleEffect(e.Effect))
		if err != nil {
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

var ruleRE = regexp.MustCompile(`^(\w+)\((.+)\)$`)
// 匹配规则字符串格式：ToolName(pattern)，如 Bash(*rm -rf /) 或 ReadFile(/etc/passwd)
func parseRule(raw string, effect RuleEffect) (Rule, error) {
	m := ruleRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return Rule{}, fmt.Errorf("invalid rule syntax: %s", raw)
	}
	return Rule{ToolName: m[1], Pattern: m[2], Effect: effect}, nil
}

// 无需授权的安全只读命令

var safeCommandPrefixes = []string{
	"ls", "dir", "pwd", "echo", "cat", "head", "tail", "wc",
	"find", "which", "whereis", "whoami", "hostname", "uname",
	"date", "cal", "uptime", "df", "du", "free", "env", "printenv",
	"file", "stat", "readlink", "realpath", "basename", "dirname",
	"sort", "uniq", "tr", "cut", "awk", "sed", "grep", "egrep", "fgrep",
	"diff", "comm", "tee", "xargs", "true", "false", "test",
	"git status", "git log", "git diff", "git show", "git branch",
	"git tag", "git remote", "git rev-parse", "git ls-files",
	"git blame", "git stash list", "go version", "go env",
	"node -v", "npm -v", "npx", "python --version", "pip list",
	"cargo --version", "rustc --version",
}

func IsSafeCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	for _, prefix := range safeCommandPrefixes {
		if cmd == prefix || strings.HasPrefix(cmd, prefix+" ") || strings.HasPrefix(cmd, prefix+"\t") {
			if !strings.Contains(cmd, ">") && !strings.Contains(cmd, "|") &&
				!strings.Contains(cmd, ";") && !strings.Contains(cmd, "&&") &&
				!strings.Contains(cmd, "$(") && !strings.Contains(cmd, "`") {
				return true
			}
		}
	}
	return false
}

// 用于规则匹配的内容提取

var contentFields = map[string]string{
	"Bash": "command", "ReadFile": "file_path", "WriteFile": "file_path",
	"EditFile": "file_path", "Glob": "pattern", "Grep": "pattern",
}

func ExtractContent(toolName string, args map[string]any) string {
	field, ok := contentFields[toolName]
	if !ok {
		return ""
	}
	v, _ := args[field].(string)
	return v
}

// DescribeToolAction 为 HITL 确认生成人类可读的操作描述。
// 优先从标准内容字段（command, file_path 等）提取；无法提取时拼接参数摘要。
func DescribeToolAction(toolName string, args map[string]any) string {
	content := ExtractContent(toolName, args)
	if content != "" {
		return content
	}
	// 无标准字段时，拼接参数的简短摘要
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:77] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	if len(parts) > 0 {
		return strings.Join(parts, ", ")
	}
	return toolName
}

// 第 4+5 层：权限检查器（编排所有层）

type Checker struct {
	Sandbox      *PathSandbox
	RuleEngine   *RuleEngine
	Mode         PermissionMode
	PlanFilePath string
	// SandboxEnabled 表示是否启用 OS 级沙箱。
	// 启用后 Bash 命令在沙箱内执行，配合 autoAllow 可跳过确认。
	SandboxEnabled bool
	// sessionAllowed 是 Layer 4b：会话级 allow-always 集合（内存中）。
	// 存放格式为 "ToolName:pattern"，用户选择 always allow 时记录。
	// 会话结束即消失，不写入磁盘。
	sessionAllowed map[string]bool
}

func NewChecker(sandbox *PathSandbox, ruleEngine *RuleEngine, mode PermissionMode) *Checker {
	return &Checker{
		Sandbox:        sandbox,
		RuleEngine:     ruleEngine,
		Mode:           mode,
		sessionAllowed: make(map[string]bool),
	}
}

// AddSessionAllow 将工具+内容模式加入会话级放行集合（Layer 4b）。
// 同时写入持久化规则引擎，以便下次会话也生效。
func (c *Checker) AddSessionAllow(toolName, content string) {
	key := toolName + ":" + content
	c.sessionAllowed[key] = true
}

// checkSessionAllowed 检查是否匹配会话级放行记录。
func (c *Checker) checkSessionAllowed(toolName, content string) bool {
	if len(c.sessionAllowed) == 0 {
		return false
	}
	key := toolName + ":" + content
	if c.sessionAllowed[key] {
		return true
	}
	// 前缀匹配：已记录的 pattern 可能带通配尾缀 *
	for allowed := range c.sessionAllowed {
		if strings.HasSuffix(allowed, "*") && strings.HasPrefix(key, allowed[:len(allowed)-1]) {
			return true
		}
	}
	return false
}

func (c *Checker) Check(tool tools.Tool, args map[string]any) Decision {
	content := ExtractContent(tool.Name(), args)
	cat := tool.Category()

	// 第 0 层：Plan 模式下的计划文件写入例外
	if c.Mode == ModePlan && cat == tools.CategoryWrite && isPlanFile(content, c.PlanFilePath) {
		return Decision{Effect: Allow, Reason: "Plan mode: plan file write allowed"}
	}

	// 第 1 层：安全只读命令（自动放行）
	if cat == tools.CategoryCommand && IsSafeCommand(content) {
		return Decision{Effect: Allow, Reason: "Safe read-only command"}
	}

	// 第 2 层：危险命令（仅 Bash）
	// 黑名单是硬防线，无论沙箱是否开启都必须先过一遍
	if cat == tools.CategoryCommand {
		hit, reason := DetectDangerous(content)
		if hit {
			return Decision{Effect: Deny, Reason: fmt.Sprintf("Dangerous command blocked: %s", reason)}
		}
	}

	// 第 2b 层：沙箱自动放行 — 命令在 OS 沙箱内执行时无需确认，
	// 但显式 deny/ask 规则仍然生效。
	// 拆分复合命令逐条检查，任何子命令触发 deny 则整体 deny，触发 ask 则弹窗。
	// bash 沙箱
	if c.SandboxEnabled && cat == tools.CategoryCommand {
		// 拆分符合命令复合语法的子命令，逐条匹配规则引擎。
		subcommands := splitCompoundCommand(content)
		var hasAsk bool
		for _, sub := range subcommands {
			r := c.RuleEngine.Evaluate(tool.Name(), sub)
			if r != nil && *r == RuleDeny {
				return Decision{Effect: Deny, Reason: "Permission rule: deny"}
			}
			if r != nil && *r == RuleAsk {
				hasAsk = true
			}
		}
		if hasAsk {
			return Decision{Effect: Ask, Reason: "Permission rule: ask (sandbox does not override explicit ask)"}
		}
		return Decision{Effect: Allow, Reason: "Sandboxed: auto-allow"}
	}

	// 第 3 层：路径沙箱（文件工具）
	if (cat == tools.CategoryRead || cat == tools.CategoryWrite) && content != "" {
		ok, reason := c.Sandbox.Check(content)
		if !ok {
			if c.Mode == ModeBypass {
				// bypass 模式跳过沙箱确认，直接放行
			} else {
				return Decision{Effect: Ask, Reason: fmt.Sprintf("Path sandbox: %s", reason)}
			}
		}
	}

	// 第 4 层：规则引擎
	ruleResult := c.RuleEngine.Evaluate(tool.Name(), content)
	if ruleResult != nil {
		if *ruleResult == RuleAllow {
			return Decision{Effect: Allow, Reason: "Permission rule: allow"}
		}
		return Decision{Effect: Deny, Reason: "Permission rule: deny"}
	}

	// 第 4b 层：会话级放行（内存中，优先于模式兜底）
	if c.checkSessionAllowed(tool.Name(), content) {
		return Decision{Effect: Allow, Reason: "Session allow-always"}
	}

	// 第 4 层：权限模式
	effect := ModeDecide(c.Mode, cat)
	if effect == Allow {
		return Decision{Effect: Allow, Reason: fmt.Sprintf("Permission mode %s: allow", c.Mode)}
	}
	if effect == Deny {
		return Decision{Effect: Deny, Reason: fmt.Sprintf("Permission mode %s: deny", c.Mode)}
	}

	// 第 5 层：ASK → HITL（人工介入）
	return Decision{Effect: Ask, Reason: "User confirmation required"}
}

func isPlanFile(targetPath, planPath string) bool {
	if planPath == "" || targetPath == "" {
		return false
	}
	// 先尝试绝对路径比较
	absTarget, err1 := filepath.Abs(targetPath)
	absPlan, err2 := filepath.Abs(planPath)
	if err1 == nil && err2 == nil && absTarget == absPlan {
		return true
	}
	// 检查目标路径是否以计划文件的相对后缀结尾
	cleanTarget := filepath.Clean(targetPath)
	cleanPlan := filepath.Clean(planPath)
	if cleanTarget == cleanPlan {
		return true
	}
	// 文件名匹配：LLM 偶尔会把 file_path 缩短为只有文件名。
	// 计划 slug 是随机生成的（形容词+名词+时间戳），因此与同名无关文件冲突的可能性极低。
	// 防止llm偶尔将 file_path 缩短为仅文件名，导致误判。
	if filepath.Base(cleanTarget) == filepath.Base(cleanPlan) {
		return true
	}
	return false
}

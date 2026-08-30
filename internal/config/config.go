package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"cody/internal/hooks"
)

var envKeyMap = map[string]string{
	"anthropic":     "ANTHROPIC_API_KEY",
	"openai":        "OPENAI_API_KEY",
	"openai-compat": "OPENAI_API_KEY",
}

var validProtocols = map[string]bool{
	"anthropic":     true,
	"openai":        true,
	"openai-compat": true,
}

type ConfigError struct {
	Message string
}

func (e *ConfigError) Error() string { return e.Message }

type ProviderConfig struct {
	Name            string `yaml:"name"`
	Protocol        string `yaml:"protocol"`
	BaseURL         string `yaml:"base_url"`
	Model           string `yaml:"model"`
	APIKey          string `yaml:"api_key"`
	Thinking        bool   `yaml:"thinking"`
	ContextWindow   int    `yaml:"context_window"`
	MaxOutputTokens int    `yaml:"max_output_tokens"`

	// fetchedContextWindow 缓存从 provider 的 /v1/models 端点自动拉取的
	// max_input_tokens（GetContextWindow 的第 2 层）。在客户端初始化时通过
	// SetFetchedContextWindow 填充一次；0 表示 "未获取"。
	// 不是 yaml 字段——它是运行时缓存，不会持久化。
	fetchedContextWindow int
}

// modelContextWindows 将模型名称子串映射到其上下文窗口
// （最大输入 token 数）。从最具体到最通用依次匹配；最先命中的
// 子串获胜。这些值只是合理的起点——它们可能随模型更新/改名而
// 偏移。当某个值不正确时，可在配置中设置
// `context_window` 覆盖（它拥有最高优先级）。
var modelContextWindows = []struct {
	substr string
	window int
}{
	{"1m", 1000000},      // 也覆盖 "-1m" 后缀（例如 claude-...-1m）
	{"gpt-4.1", 1000000}, // GPT-4.1 系列提供 1M 上下文窗口
	{"gpt-4o", 128000},
	{"gpt-4-turbo", 128000},
	{"o1", 200000}, // OpenAI 推理模型 o1 / o3 / o4
	{"o3", 200000},
	{"o4", 200000},
	{"gpt-3.5", 16385},
	{"claude", 200000},
}

// SetFetchedContextWindow 记录从 provider 自动拉取的上下文窗口
// （第 2 层）。非正值会被忽略，这样失败的拉取永远不会
// 污染缓存。在客户端初始化时每个 provider 调用一次。
func (p *ProviderConfig) SetFetchedContextWindow(window int) {
	if window > 0 {
		p.fetchedContextWindow = window
	}
}

// lookupModelContextWindow 通过子串匹配返回内置映射表中给定模型的
// 上下文窗口（第 3 层），没有匹配时返回 0。
func lookupModelContextWindow(model string) int {
	m := strings.ToLower(model)
	for _, e := range modelContextWindows {
		if strings.Contains(m, e.substr) {
			return e.window
		}
	}
	return 0
}

// GetContextWindow 通过四层回退解析模型的上下文窗口，
// 优先级从高到低：
//
//  1. 配置提供的 context_window（> 0）——显式覆盖，总是优先。
//  2. 从 provider 的 /v1/models 端点自动拉取的值，通过
//     SetFetchedContextWindow 缓存（只有 Anthropic 协议的 provider 才会设置；
//     拉取失败/缺失时为 0 并跳过）。
//  3. 内置的 model-name → window 映射表（子串匹配）。
//  4. 保守默认值（claude → 200000，否则 → 128000）。
func (p *ProviderConfig) GetContextWindow() int {
	if p.ContextWindow > 0 {
		return p.ContextWindow
	}
	if p.fetchedContextWindow > 0 {
		return p.fetchedContextWindow
	}
	if w := lookupModelContextWindow(p.Model); w > 0 {
		return w
	}
	if strings.Contains(p.Model, "claude") {
		return 200000
	}
	return 128000
}

func (p *ProviderConfig) GetMaxOutputTokens() int {
	if p.MaxOutputTokens > 0 {
		return p.MaxOutputTokens
	}
	if p.Thinking {
		return 64000
	}
	return 8192
}

func (p *ProviderConfig) ResolveAPIKey() string {
	if p.APIKey != "" {
		return p.APIKey
	}
	envVar := envKeyMap[p.Protocol]
	if envVar == "" {
		return ""
	}
	return os.Getenv(envVar)
}

type MCPServerConfig struct {
	Name      string            `yaml:"name"`
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	URL       string            `yaml:"url"`
	Transport string            `yaml:"transport"`
	Headers   map[string]string `yaml:"headers"`
	Env       map[string]string `yaml:"env"`
}

// SandboxConfig 控制 OS 级沙箱的配置
type SandboxConfig struct {
	Enabled        bool `yaml:"enabled"`         // 是否启用沙箱
	AutoAllow      bool `yaml:"auto_allow"`       // 沙箱内命令是否自动放行
	NetworkEnabled bool `yaml:"network_enabled"`  // 是否允许网络访问
}

type AppConfig struct {
	Providers             []ProviderConfig  `yaml:"providers"`
	PermissionMode        string            `yaml:"permission_mode"`
	MCPServers            []MCPServerConfig `yaml:"mcp_servers"`
	Hooks                 []hooks.Hook      `yaml:"hooks"`
	Sandbox               SandboxConfig     `yaml:"sandbox"`
	EnableCoordinatorMode bool              `yaml:"enable_coordinator_mode"`

	// EnableFork 控制省略 subagent_type 时是否走 fork。用指针是因为它默认开着，
	// 普通 bool 分不清「配置里没写」和「配置里写了 false」，后者就永远关不掉。
	EnableFork *bool `yaml:"enable_fork"`
}

// ForkEnabled 返回 fork 是否可用。配置里没写就是开着。
func (c *AppConfig) ForkEnabled() bool {
	return c.EnableFork == nil || *c.EnableFork
}

func loadSingleFile(path string) (*AppConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %s: %w", path, err)
	}
	var cfg AppConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("Failed to parse config %s: %s", path, err)}
	}
	return &cfg, nil
}

func mergeConfig(base, override *AppConfig) *AppConfig {
	if override.Providers != nil {
		base.Providers = override.Providers
	}
	if override.PermissionMode != "" {
		base.PermissionMode = override.PermissionMode
	}
	if len(override.MCPServers) > 0 {
		byName := make(map[string]int)
		for i, s := range base.MCPServers {
			byName[s.Name] = i
		}
		for _, s := range override.MCPServers {
			if idx, ok := byName[s.Name]; ok {
				base.MCPServers[idx] = s
			} else {
				base.MCPServers = append(base.MCPServers, s)
				byName[s.Name] = len(base.MCPServers) - 1
			}
		}
	}
	base.Hooks = append(base.Hooks, override.Hooks...)
	// 沙箱配置：后加载的配置覆盖先前的
	if override.Sandbox.Enabled {
		base.Sandbox = override.Sandbox
	}
	if override.EnableCoordinatorMode {
		base.EnableCoordinatorMode = true
	}
	if override.EnableFork != nil {
		base.EnableFork = override.EnableFork
	}
	return base
}

func validateProviders(cfg *AppConfig) error {
	if len(cfg.Providers) == 0 {
		return &ConfigError{Message: "At least one provider must be configured"}
	}
	requiredFields := []string{"name", "protocol", "base_url", "model"}
	for i, p := range cfg.Providers {
		var missing []string
		values := map[string]string{
			"name":     p.Name,
			"protocol": p.Protocol,
			"base_url": p.BaseURL,
			"model":    p.Model,
		}
		for _, f := range requiredFields {
			if values[f] == "" {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			return &ConfigError{
				Message: fmt.Sprintf("Provider #%d: missing fields: %s", i+1, strings.Join(missing, ", ")),
			}
		}
		if !validProtocols[p.Protocol] {
			return &ConfigError{
				Message: fmt.Sprintf("Provider #%d: invalid protocol '%s', must be one of: anthropic, openai, openai-compat", i+1, p.Protocol),
			}
		}
	}
	return nil
}

func LoadConfig(path string) (*AppConfig, error) {
	if path != "" {
		cfg, err := loadSingleFile(path)
		if err != nil {
			return nil, err
		}
		if err := validateProviders(cfg); err != nil {
			return nil, err
		}
		return cfg, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return nil, &ConfigError{Message: fmt.Sprintf("Failed to get working directory: %s", err)}
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".cody", "config.yaml"),
		filepath.Join(wd, ".cody", "config.yaml"),
		filepath.Join(wd, ".cody", "config.local.yaml"),
	}

	var merged *AppConfig
	for _, path := range candidates {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		layer, err := loadSingleFile(path)
		if err != nil {
			return nil, err
		}
		if merged == nil {
			merged = layer
		} else {
			merged = mergeConfig(merged, layer)
		}
	}

	if merged == nil {
		return nil, &ConfigError{Message: "No config file found. Expected .cody/config.yaml in project or ~/.cody/config.yaml"}
	}

	if err := validateProviders(merged); err != nil {
		return nil, err
	}
	return merged, nil
}

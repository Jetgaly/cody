package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Catalog 是内存中所有已加载 skill 的注册表。Phase-1 条目只包含 frontmatter
// （PromptBody 为空、BodyLoaded 为 false）；GetFull 在每次调用时触发对 body 的
// phase-2 读取（热重载）。
type Catalog struct {
	skills      map[string]*Skill
	sources     map[string]string // skill 名称 → "builtin" | "user" | "project" | 绝对路径
	workDir     string            // 记住工作目录，以便 Reload 重新扫描相同的三个层级
	hasReload   bool
	dirModTimes map[string]time.Time // skill 目录路径 → 上次已知的修改时间
}

func NewCatalog() *Catalog {
	return &Catalog{
		skills:      make(map[string]*Skill),
		sources:     make(map[string]string),
		dirModTimes: make(map[string]time.Time),
	}
}

// Register 在 catalog 中新增（或覆盖）一个 skill。来源标签会由 /skills 显示。
func (c *Catalog) Register(s *Skill, source string) {
	c.skills[s.Meta.Name] = s
	c.sources[s.Meta.Name] = source
}

// Get 返回 phase-1 的 skill（仅 frontmatter）。如果 catalog 以 phase-1 模式加载，
// PromptBody 可能为空。
func (c *Catalog) Get(name string) *Skill {
	return c.skills[name]
}

// GetFull 返回已加载 body 的 skill。对于磁盘型 skill，body 在每次调用时都会被
// 重新读取（热重载）。对于内嵌的内置 skill，body 已在内存中，属于缓存命中。
// 读取失败时保留之前缓存的 body，并返回错误。
func (c *Catalog) GetFull(name string) (*Skill, error) {
	skill, ok := c.skills[name]
	if !ok {
		return nil, fmt.Errorf("unknown skill: %s", name)
	}
	if skill.SourceDir == "" {
		// 内嵌 skill —— body 在启动时已加载，无需刷新。
		return skill, nil
	}
	if err := loadSkillBody(skill); err != nil {
		// 如果存在则保留之前缓存的 body；是否向上抛出错误或继续往下走由调用方决定。
		if skill.PromptBody == "" {
			return nil, err
		}
		return skill, err
	}
	return skill, nil
}

// List 返回每个已加载 skill 的元数据。顺序是 map 迭代顺序（未排序）——
// 需要稳定顺序的调用方应按 Name 排序。
func (c *Catalog) List() []SkillMeta {
	result := make([]SkillMeta, 0, len(c.skills))
	for _, s := range c.skills {
		result = append(result, s.Meta)
	}
	return result
}

// Source 返回 skill 的来源标签（"builtin"、"user"、"project" 或路径）。
// 如果 skill 未加载则返回 ""。
func (c *Catalog) Source(name string) string {
	return c.sources[name]
}

// Reload 重新扫描全部三个层级（builtin + user + project）并就地重建 catalog。
// 由 `/skills reload` 和测试使用。
func (c *Catalog) Reload(workDir string) {
	fresh := LoadCatalog(workDir)
	c.skills = fresh.skills
	c.sources = fresh.sources
	c.workDir = fresh.workDir
	c.dirModTimes = fresh.dirModTimes
}

// NeedsReload 检查 skill 目录的修改时间自上次加载后是否发生变化。
// 修改时间变化表示有 skill 被新增或移除（已有 skill 内的文件编辑已由
// GetFull 的每次调用重新读取来处理）。
func (c *Catalog) NeedsReload() bool {
	for dir, recorded := range c.dirModTimes {
		info, err := os.Stat(dir)
		if err != nil {
			if recorded.IsZero() {
				continue
			}
			return true // 目录已消失
		}
		if !info.ModTime().Equal(recorded) {
			return true
		}
	}
	// 检查自上次加载以来是否有新目录被创建
	dirs := skillDirPaths(c.workDir)
	for _, dir := range dirs {
		if _, tracked := c.dirModTimes[dir]; !tracked {
			if info, err := os.Stat(dir); err == nil && !info.ModTime().IsZero() {
				return true
			}
		}
	}
	return false
}

// snapshotDirModTimes 记录所有 skill 目录当前的修改时间。
func (c *Catalog) snapshotDirModTimes() {
	c.dirModTimes = make(map[string]time.Time)
	for _, dir := range skillDirPaths(c.workDir) {
		info, err := os.Stat(dir)
		if err != nil {
			c.dirModTimes[dir] = time.Time{}
			continue
		}
		c.dirModTimes[dir] = info.ModTime()
	}
}

// skillDirPaths 返回用户全局与项目的 skill 目录路径。
func skillDirPaths(workDir string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".cody", "skills"))
	}
	if workDir != "" {
		dirs = append(dirs, filepath.Join(workDir, ".cody", "skills"))
	}
	return dirs
}

// LoadCatalog 通过合并三个层级来构建 phase-1 catalog，后面的来源按名称覆盖前面的来源
// （project 覆盖 user，user 覆盖 builtin）：
//  1. internal/skills/builtins/*（通过 go:embed 内嵌，优先级最低）
//  2. ~/.cody/skills/         （用户全局）
//  3. $workDir/.cody/skills/  （项目，优先级最高）
//
// 此阶段只读取 frontmatter；在调用 GetFull 之前 PromptBody 保持为空。
// 单个 skill 的解析失败会被静默跳过——一个坏文件不能拖垮整个 catalog。
func LoadCatalog(workDir string) *Catalog {
	c := NewCatalog()
	c.workDir = workDir

	// 第 1 层：内嵌内置 skill
	for _, s := range LoadBuiltins() {
		c.Register(s, "builtin")
	}

	// 第 2 层：用户全局
	if home, err := os.UserHomeDir(); err == nil {
		loadTierInto(c, filepath.Join(home, ".cody", "skills"), "user")
	}

	// 第 3 层：项目
	loadTierInto(c, filepath.Join(workDir, ".cody", "skills"), "project")

	c.snapshotDirModTimes()
	return c
}

// LoadFromDirectory 把 dir 的每个子目录当作一个 skill 加载。供测试以及
// 只需加载单个层级的临时调用方使用。body 会被立即读取（不做两阶段拆分），
// 这样现有会访问 skill.PromptBody 的测试代码仍能正常工作。
func LoadFromDirectory(dir string) (*Catalog, error) {
	c := NewCatalog()
	loadTierEager(c, dir, dir)
	return c, nil
}

// LoadSkills 是为向后兼容而保留的旧版两层加载器，兼容仍然急切预加载 body 的代码。
// 新的调用方应使用 LoadCatalog + GetFull。顺序：用户全局 → 项目。
func LoadSkills(workDir string) *Catalog {
	c := NewCatalog()
	c.workDir = workDir
	if home, err := os.UserHomeDir(); err == nil {
		loadTierEager(c, filepath.Join(home, ".cody", "skills"), "user")
	}
	loadTierEager(c, filepath.Join(workDir, ".cody", "skills"), "project")
	return c
}

// loadTierInto 遍历单个层级，并把每个子目录注册为 phase-1 skill。
// 单个条目的错误会被吞掉。
func loadTierInto(c *Catalog, dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skill, err := parseFrontmatterOnly(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		c.Register(skill, source)
	}
}

// loadTierEager 与 loadTierInto 类似，但还会读取 body。供旧的
// LoadSkills / LoadFromDirectory 使用，以保持旧行为。
func loadTierEager(c *Catalog, dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		skill, err := parseFrontmatterOnly(path)
		if err != nil {
			continue
		}
		_ = loadSkillBody(skill)
		c.Register(skill, source)
	}
}

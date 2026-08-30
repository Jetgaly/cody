package memory

import (
	"os"
	"path/filepath"
	"strings"
)

// AutoMemEntrypointName 是每个项目的记忆索引文件名。
const AutoMemEntrypointName = "MEMORY.md"

// GetAutoMemPath 返回给定项目根目录的自动记忆目录路径。
// 形如：<projectRoot>/.cody/memory/
//
// 保留尾部路径分隔符，以便基于前缀的路径匹配（例如沙箱中的
// `HasPrefix` 检查）能正常工作，而不会误匹配 `…/memoryxyz`。
//
// Cody 将记忆与其他项目本地状态一起放在 .cody/ 下，
// 这样记录会在 IDE 中显示，编辑器也能直接打开它们。
//
// 解析顺序：
//  1. CODY_REMOTE_MEMORY_DIR 环境变量——按原样使用（用于
//     CI/容器等记忆应存放在别处的场景的逃生通道）
//  2. <projectRoot>/.cody/memory
func GetAutoMemPath(projectRoot string) string {
	if override := os.Getenv("CODY_REMOTE_MEMORY_DIR"); override != "" {
		return strings.TrimRight(override, string(filepath.Separator)) + string(filepath.Separator)
	}
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		abs = projectRoot
	}
	return filepath.Join(abs, ".cody", "memory") + string(filepath.Separator)
}

// GetAutoMemEntrypoint 返回自动记忆目录内 MEMORY.md 的路径。
func GetAutoMemEntrypoint(projectRoot string) string {
	dir := GetAutoMemPath(projectRoot)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, AutoMemEntrypointName)
}

// IsAutoMemPath 检查绝对路径是否位于项目级或用户级自动记忆目录之一。
// 路径沙箱用它来放行对任一记忆目录的写入。
func IsAutoMemPath(absolutePath, projectRoot string) bool {
	abs := filepath.Clean(absolutePath)
	if dir := GetAutoMemPath(projectRoot); dir != "" {
		if strings.HasPrefix(abs+string(filepath.Separator), dir) {
			return true
		}
	}
	if dir := GetUserAutoMemPath(); dir != "" {
		if strings.HasPrefix(abs+string(filepath.Separator), dir) {
			return true
		}
	}
	return false
}

// GetUserAutoMemPath 返回用户级自动记忆目录：~/.cody/memory/。
// 用于 type=user / type=feedback 类型的记忆，这些记忆会跨项目跟随用户
// （例如编码偏好）。如果无法解析主目录则返回 ""。
//
// 保留尾部路径分隔符，以便基于前缀的路径匹配。
func GetUserAutoMemPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".cody", "memory") + string(filepath.Separator)
}

// GetUserAutoMemEntrypoint 返回 ~/.cody/memory/MEMORY.md 的路径。
func GetUserAutoMemEntrypoint() string {
	dir := GetUserAutoMemPath()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, AutoMemEntrypointName)
}

// IsUserAutoMemPath 检查绝对路径是否位于用户级记忆目录内。
// 用于需要区分用户作用域与项目作用域的地方（沙箱已同时接受两者；
// 这里是为了路由）。
func IsUserAutoMemPath(absolutePath string) bool {
	dir := GetUserAutoMemPath()
	if dir == "" {
		return false
	}
	abs := filepath.Clean(absolutePath)
	return strings.HasPrefix(abs+string(filepath.Separator), dir)
}

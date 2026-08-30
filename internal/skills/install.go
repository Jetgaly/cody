package skills

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SkillSource 描述从哪里拉取技能。始终归一化为
// GitHub Contents API 路径，因为 skills.sh 只是一个指向 GitHub 树的注册表。
type SkillSource struct {
	Owner    string
	Repo     string
	Ref      string // 分支或标签；未指定时默认为 "main"
	Subpath  string // 技能目录在仓库内的路径（不以 / 结尾）
	Name     string // 技能名称（== Subpath 的最后一段）
	Original string // 用户提供的 URL，用于错误信息
}

// ParseSkillURL 接受三种 URL 形式：
//
//  1. https://www.skills.sh/<owner>/<repo>/<skill-name>
//     — 假定技能位于仓库中的 "skills/<skill-name>"（anthropics/skills 约定）
//  2. https://github.com/<owner>/<repo>/tree/<ref>/<subpath>
//     — 直接子树 URL；最后一段是技能名称
//  3. https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<subpath>/SKILL.md
//     — raw 文件 URL；将父目录视为技能子路径
//
// 返回完全解析后的 SkillSource，若 URL 不符合上述三种形式则返回错误。
func ParseSkillURL(raw string) (*SkillSource, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("only http(s) URLs are supported")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")

	switch u.Host {
	case "www.skills.sh", "skills.sh":
		if len(parts) < 3 {
			return nil, fmt.Errorf("skills.sh URL must be /<owner>/<repo>/<skill-name>")
		}
		return &SkillSource{
			Owner:    parts[0],
			Repo:     parts[1],
			Ref:      "main",
			Subpath:  "skills/" + strings.Join(parts[2:], "/"),
			Name:     parts[len(parts)-1],
			Original: raw,
		}, nil

	case "github.com":
		// 期望格式：/<owner>/<repo>/tree/<ref>/<...subpath>
		if len(parts) < 5 || parts[2] != "tree" {
			return nil, fmt.Errorf("github.com URL must be /<owner>/<repo>/tree/<ref>/<subpath>")
		}
		sub := strings.Join(parts[4:], "/")
		return &SkillSource{
			Owner:    parts[0],
			Repo:     parts[1],
			Ref:      parts[3],
			Subpath:  sub,
			Name:     parts[len(parts)-1],
			Original: raw,
		}, nil

	case "raw.githubusercontent.com":
		// 期望格式：/<owner>/<repo>/<ref>/<...subpath>/SKILL.md
		if len(parts) < 4 {
			return nil, fmt.Errorf("raw.githubusercontent.com URL too short")
		}
		// 去掉末尾文件名，使 Subpath 以技能目录结尾。
		subParts := parts[3:]
		if last := subParts[len(subParts)-1]; strings.Contains(last, ".") {
			subParts = subParts[:len(subParts)-1]
		}
		if len(subParts) == 0 {
			return nil, fmt.Errorf("raw URL missing skill subpath")
		}
		return &SkillSource{
			Owner:    parts[0],
			Repo:     parts[1],
			Ref:      parts[2],
			Subpath:  strings.Join(subParts, "/"),
			Name:     subParts[len(subParts)-1],
			Original: raw,
		}, nil
	}
	return nil, fmt.Errorf("unsupported host %q (try skills.sh or github.com)", u.Host)
}

// contentEntry 保存我们关心的 GitHub Contents API 响应的子集。
// Type 为 "file" | "dir" | "symlink" | "submodule"；我们只处理文件和目录。
type contentEntry struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"`
	DownloadURL string `json:"download_url"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding"`
	Size        int    `json:"size"`
}

// installLimits 限制我们从远程源拉取的数据量。
// 单技能安装很小（SKILL.md + 可能几个参考文件）；
// 更大的数据量很可能意味着 URL 写错了或是恶意来源。
const (
	maxFileSize     = 1 << 20 // 每个文件 1 MiB
	maxTotalSize    = 8 << 20 // 每个技能 8 MiB
	maxFileCount    = 64
	maxRecursionDepth = 4
	httpTimeout     = 30 * time.Second
)

// fetcher 集中处理 HTTP 调用，使测试可以替换底层 client，
// 并确保我们应用一致的请求头和超时。
type fetcher struct {
	client *http.Client
	apiBase string // "https://api.github.com" — 测试时可覆盖
}

func newFetcher() *fetcher {
	return &fetcher{
		client:  &http.Client{Timeout: httpTimeout},
		apiBase: "https://api.github.com",
	}
}

func (f *fetcher) listContents(src *SkillSource, subpath string) ([]contentEntry, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		f.apiBase, src.Owner, src.Repo, subpath, url.QueryEscape(src.Ref))
	req, _ := http.NewRequest("GET", endpoint, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "cody-install-skill")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contents API: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		// 触发了限流；把响应体展示出来，让用户看到 GitHub 的错误信息。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("github API forbidden (rate-limited?): %s", strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github API returned %d for %s", resp.StatusCode, endpoint)
	}

	// 该端点对目录返回数组，对文件返回单个对象。
	// 解码为通用值后分派处理。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFileSize))
	if err != nil {
		return nil, fmt.Errorf("read contents response: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("github returned empty body")
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var entries []contentEntry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("parse dir listing: %w", err)
		}
		return entries, nil
	}
	var single contentEntry
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, fmt.Errorf("parse file metadata: %w", err)
	}
	return []contentEntry{single}, nil
}

// fetchBlob 下载单个文件的字节。优先使用内联的 base64
// `content` 字段（比再次往返请求更便宜），
// 对二进制 / 大于 1MB 的文件则回退到 download_url。
func (f *fetcher) fetchBlob(e contentEntry) ([]byte, error) {
	if e.Size > maxFileSize {
		return nil, fmt.Errorf("file %s too large: %d bytes (max %d)", e.Path, e.Size, maxFileSize)
	}
	if e.Encoding == "base64" && e.Content != "" {
		clean := strings.ReplaceAll(e.Content, "\n", "")
		out, err := base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, fmt.Errorf("decode base64 for %s: %w", e.Path, err)
		}
		return out, nil
	}
	if e.DownloadURL == "" {
		return nil, fmt.Errorf("no download_url for %s", e.Path)
	}
	req, _ := http.NewRequest("GET", e.DownloadURL, nil)
	req.Header.Set("User-Agent", "cody-install-skill")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: status %d", e.DownloadURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxFileSize))
}

// InstallReport 汇总一次 Install 调用做了什么，由工具返回用于展示，并被测试消费。
type InstallReport struct {
	SkillName  string
	TargetDir  string
	FileCount  int
	TotalBytes int64
}

// Install 将 src 处的技能拉取到 installRoot/<src.Name>/ 下。installRoot
// 预期是用户全局技能层（~/.cody/skills/），以便跨项目复用安装结果。
//
// 写入在目录级别是原子的：我们先将内容暂存到同级临时目录，
// 再 rename 到目标位置。部分失败不会改动 installRoot。
func Install(src *SkillSource, installRoot string) (*InstallReport, error) {
	return installWith(newFetcher(), src, installRoot)
}

func installWith(f *fetcher, src *SkillSource, installRoot string) (*InstallReport, error) {
	if src == nil {
		return nil, fmt.Errorf("nil source")
	}
	if err := validateSkillName(src.Name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(installRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create install root: %w", err)
	}
	staging, err := os.MkdirTemp(installRoot, ".install-"+src.Name+"-*")
	if err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	cleanupStaging := func() { _ = os.RemoveAll(staging) }

	report := &InstallReport{SkillName: src.Name}
	if err := walkAndDownload(f, src, src.Subpath, staging, report, 0); err != nil {
		cleanupStaging()
		return nil, err
	}
	if !hasSkillManifest(staging) {
		cleanupStaging()
		return nil, fmt.Errorf("downloaded tree missing SKILL.md or skill.yaml — not a skill?")
	}

	final := filepath.Join(installRoot, src.Name)
	if _, err := os.Stat(final); err == nil {
		// 覆盖已有安装：先删除旧目录。用户显式要求安装——
		// 假定他们想要最新版本。
		if err := os.RemoveAll(final); err != nil {
			cleanupStaging()
			return nil, fmt.Errorf("remove old install: %w", err)
		}
	}
	if err := os.Rename(staging, final); err != nil {
		cleanupStaging()
		return nil, fmt.Errorf("promote staging dir: %w", err)
	}
	report.TargetDir = final
	return report, nil
}

// walkAndDownload 在 localDir 下复现 GitHub 上的目录树，
// 并按安装限制统计文件数与字节数。
func walkAndDownload(f *fetcher, src *SkillSource, subpath, localDir string, report *InstallReport, depth int) error {
	if depth > maxRecursionDepth {
		return fmt.Errorf("install tree too deep (>%d levels)", maxRecursionDepth)
	}
	entries, err := f.listContents(src, subpath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if report.FileCount >= maxFileCount {
			return fmt.Errorf("install file count limit (%d) reached", maxFileCount)
		}
		// `Name` 是叶子名称——即使 GitHub API 本身不会产生 "../"，也要防止路径穿越。
		if strings.Contains(e.Name, "..") || strings.ContainsAny(e.Name, "/\\") {
			return fmt.Errorf("suspicious entry name: %q", e.Name)
		}
		target := filepath.Join(localDir, e.Name)
		switch e.Type {
		case "file":
			data, err := f.fetchBlob(e)
			if err != nil {
				return err
			}
			if int64(report.TotalBytes)+int64(len(data)) > maxTotalSize {
				return fmt.Errorf("install total size limit (%d bytes) reached", maxTotalSize)
			}
			if err := os.WriteFile(target, data, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", target, err)
			}
			report.FileCount++
			report.TotalBytes += int64(len(data))
		case "dir":
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := walkAndDownload(f, src, e.Path, target, report, depth+1); err != nil {
				return err
			}
		default:
			// 静默跳过 symlink / submodule——它们不应该出现在格式良好的技能中。
		}
	}
	return nil
}

// hasSkillManifest 校验暂存的树在根目录包含 SKILL.md 或
// skill.yaml。事前防护 "URL 指向了错误的子目录" 这种错误。
func hasSkillManifest(dir string) bool {
	for _, name := range []string{"SKILL.md", "skill.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// validateSkillName 允许 kebab-case 和 snake_case；禁止路径
// 穿越、以点开头，以及任何会让 shell 感到意外的字符。
func validateSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("empty skill name")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("skill name cannot start with '.'")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("skill name %q contains invalid char %q (use a-z 0-9 - _)", name, r)
		}
	}
	return nil
}

// UserSkillsRoot 返回 ~/.cody/skills，如有需要会创建父目录，
// 调用方不必重复这套流程。
func UserSkillsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	root := filepath.Join(home, ".cody", "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type TreeNode struct {
	Path string `json:"path"`
	Type string `json:"type"` // "file" or "dir"
	Size int64  `json:"size"`
}

func (m *Manager) FetchTree(ctx context.Context, providerType, gitURL, ref string, auth *Auth) ([]TreeNode, error) {
	switch providerType {
	case "github":
		return fetchGitHubTree(ctx, m.client, gitURL, ref, auth)
	case "gitlab":
		return fetchGitLabTree(ctx, m.client, gitURL, ref, auth)
	case "gitea":
		return fetchGiteaTree(ctx, m.client, gitURL, ref, auth)
	default:
		return nil, fmt.Errorf("tree listing not supported for %s", providerType)
	}
}

func fetchGitHubTree(ctx context.Context, client *http.Client, gitURL, ref string, auth *Auth) ([]TreeNode, error) {
	owner, repo, err := parseGitHubURL(gitURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, ref)
	
	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	if auth != nil && auth.Token != "" {
		req.Header.Set("Authorization", "token "+auth.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("github api returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	nodes := make([]TreeNode, 0, len(result.Tree))
	for _, item := range result.Tree {
		if item.Type == "blob" {
			nodes = append(nodes, TreeNode{
				Path: item.Path,
				Type: "file",
				Size: item.Size,
			})
		}
	}

	return nodes, nil
}

func fetchGitLabTree(ctx context.Context, client *http.Client, gitURL, ref string, auth *Auth) ([]TreeNode, error) {
	host, projectPath, err := parseGitLabURL(gitURL)
	if err != nil {
		return nil, err
	}

	encodedProject := url.PathEscape(projectPath)
	apiURL := fmt.Sprintf("https://%s/api/v4/projects/%s/repository/tree?ref=%s&recursive=true&per_page=1000",
		host, encodedProject, url.QueryEscape(ref))

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	if auth != nil && auth.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", auth.Token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gitlab api returned %d: %s", resp.StatusCode, string(body))
	}

	var result []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	nodes := make([]TreeNode, 0, len(result))
	for _, item := range result {
		if item.Type == "blob" {
			nodes = append(nodes, TreeNode{
				Path: item.Path,
				Type: "file",
				Size: 0,
			})
		}
	}

	return nodes, nil
}

func fetchGiteaTree(ctx context.Context, client *http.Client, gitURL, ref string, auth *Auth) ([]TreeNode, error) {
	host, owner, repo, err := parseGiteaURL(gitURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("https://%s/api/v1/repos/%s/%s/git/trees/%s?recursive=true",
		host, owner, repo, ref)

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	if auth != nil && auth.Token != "" {
		req.Header.Set("Authorization", "token "+auth.Token)
	} else if auth != nil && auth.Username != "" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gitea api returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"tree"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	nodes := make([]TreeNode, 0, len(result.Tree))
	for _, item := range result.Tree {
		if item.Type == "blob" {
			nodes = append(nodes, TreeNode{
				Path: item.Path,
				Type: "file",
				Size: item.Size,
			})
		}
	}

	return nodes, nil
}

func FilterRenderableFiles(nodes []TreeNode) []TreeNode {
	// 文本文件扩展名
	renderableExts := map[string]bool{
		// 文档
		".md": true, ".markdown": true, ".mdx": true,
		".txt": true, ".text": true,
		".rst": true, ".asciidoc": true, ".adoc": true,
		
		// 标记语言
		".html": true, ".htm": true, ".xhtml": true,
		".xml": true, ".svg": true,
		
		// 配置文件
		".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".conf": true, ".cfg": true,
		
		// 脚本
		".sh": true, ".bash": true, ".zsh": true, ".fish": true,
		".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".jsx": true, ".tsx": true,
		".py": true, ".rb": true, ".pl": true, ".php": true,
		".lua": true, ".vim": true,
		
		// 编程语言
		".c": true, ".h": true, ".cpp": true, ".cc": true, ".cxx": true, ".hpp": true,
		".go": true, ".rs": true, ".java": true, ".kt": true, ".scala": true,
		".cs": true, ".vb": true, ".fs": true,
		".swift": true, ".m": true, ".mm": true,
		
		// Web
		".css": true, ".scss": true, ".sass": true, ".less": true,
		".vue": true, ".svelte": true,
		
		// 数据
		".csv": true, ".tsv": true, ".sql": true,
		
		// 其他
		".log": true, ".env": true, ".gitignore": true, ".dockerignore": true,
		".dockerfile": true, ".makefile": true,
	}
	
	// 已知的二进制文件扩展名（需要排除）
	binaryExts := map[string]bool{
		// 图片
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true, ".ico": true, ".webp": true, ".svg": false,
		
		// 视频/音频
		".mp4": true, ".avi": true, ".mov": true, ".mp3": true, ".wav": true, ".flac": true,
		
		// 压缩包
		".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".7z": true, ".rar": true,
		
		// 可执行文件
		".exe": true, ".dll": true, ".so": true, ".dylib": true, ".app": true,
		
		// 文档
		".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
		
		// 其他
		".bin": true, ".dat": true, ".db": true, ".sqlite": true,
	}

	filtered := make([]TreeNode, 0, len(nodes))
	for _, node := range nodes {
		ext := strings.ToLower(getExt(node.Path))
		
		// 有扩展名的文件：检查是否在可渲染列表里
		if ext != "" {
			if renderableExts[ext] {
				filtered = append(filtered, node)
			}
			continue
		}
		
		// 无扩展名文件：默认当作文本文件，除非文件名看起来像二进制
		filename := getFilename(node.Path)
		
		// 检查文件名是否在已知二进制列表里（比如 a.out 这种）
		if binaryExts[strings.ToLower(filename)] {
			continue
		}
		
		// 所有其他无扩展名文件都当作文本文件
		filtered = append(filtered, node)
	}
	return filtered
}

func getExt(path string) string {
	idx := strings.LastIndex(path, ".")
	if idx == -1 {
		return ""
	}
	// 检查最后一个点之后是否有斜杠（说明点是目录名的一部分）
	if strings.Contains(path[idx:], "/") {
		return ""
	}
	return path[idx:]
}

func getFilename(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx == -1 {
		return path
	}
	return path[idx+1:]
}

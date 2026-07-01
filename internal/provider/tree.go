package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

type TreeNode struct {
	Path string `json:"path"`
	Type string `json:"type"` // "file" or "dir"
	Size int64  `json:"size"`
}

func (m *Manager) FetchTree(ctx context.Context, providerType, gitURL, ref string, auth *Auth) ([]TreeNode, error) {
	base := providerBase{allow: m.allow, deny: m.deny, maxSize: m.maxSize}
	switch providerType {
	case "github":
		return fetchGitHubTree(ctx, m.client, base, gitURL, ref, auth)
	case "gitlab":
		return fetchGitLabTree(ctx, m.client, base, gitURL, ref, auth)
	case "gitea":
		return fetchGiteaTree(ctx, m.client, base, gitURL, ref, auth)
	default:
		return nil, fmt.Errorf("tree listing not supported for %s", providerType)
	}
}

func fetchGitHubTree(ctx context.Context, client *http.Client, b providerBase, gitURL, ref string, auth *Auth) ([]TreeNode, error) {
	owner, repo, err := parseGitHubURL(gitURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees/%s?recursive=1", owner, repo, ref)
	if err := b.checkSSRF(apiURL); err != nil {
		return nil, err
	}

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

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuthRequired
	}
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

func fetchGitLabTree(ctx context.Context, client *http.Client, b providerBase, gitURL, ref string, auth *Auth) ([]TreeNode, error) {
	scheme, host, projectPath, err := parseGitLabURL(gitURL)
	if err != nil {
		return nil, err
	}

	encodedProject := url.PathEscape(projectPath)
	apiURL := fmt.Sprintf("%s://%s/api/v4/projects/%s/repository/tree?ref=%s&recursive=true&per_page=1000",
		scheme, host, encodedProject, url.QueryEscape(ref))
	if err := b.checkSSRF(apiURL); err != nil {
		return nil, err
	}

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

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuthRequired
	}
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

func fetchGiteaTree(ctx context.Context, client *http.Client, b providerBase, gitURL, ref string, auth *Auth) ([]TreeNode, error) {
	scheme, host, owner, repo, err := parseGiteaURL(gitURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("%s://%s/api/v1/repos/%s/%s/git/trees/%s?recursive=true",
		scheme, host, owner, repo, ref)
	if err := b.checkSSRF(apiURL); err != nil {
		return nil, err
	}

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

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuthRequired
	}
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

// FilterRenderableFiles 用 renderable 谓词过滤文件树，只保留可预览的文件。
// 谓词由调用方传入（通常为 render.Renderer.IsRenderable），保证扩展名判定唯一来源。
func FilterRenderableFiles(nodes []TreeNode, renderable func(path string) bool) []TreeNode {
	filtered := make([]TreeNode, 0, len(nodes))
	for _, node := range nodes {
		if node.Type != "file" {
			continue
		}
		if renderable(node.Path) {
			filtered = append(filtered, node)
		}
	}
	return filtered
}

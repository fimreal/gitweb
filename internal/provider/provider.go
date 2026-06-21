package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
)

var ErrNotFound = errors.New("file not found")

type Auth struct {
	Type     string
	Token    string
	Username string
	Password string
}

type Provider interface {
	Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error)
}

type Manager struct {
	client *http.Client
	sf     singleflight.Group
}

func NewManager(timeout time.Duration, httpProxy, httpsProxy string) *Manager {
	transport := &http.Transport{}
	
	// 配置代理
	if httpProxy != "" || httpsProxy != "" {
		transport.Proxy = func(req *http.Request) (*url.URL, error) {
			if req.URL.Scheme == "https" && httpsProxy != "" {
				return url.Parse(httpsProxy)
			}
			if httpProxy != "" {
				return url.Parse(httpProxy)
			}
			return nil, nil
		}
	}
	
	return &Manager{
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}
}

func (m *Manager) Fetch(ctx context.Context, providerType, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	key := fmt.Sprintf("%s:%s:%s", gitURL, ref, filepath)
	
	v, err, _ := m.sf.Do(key, func() (interface{}, error) {
		var p Provider
		switch providerType {
		case "github":
			p = &GitHubProvider{client: m.client}
		case "gitlab":
			p = &GitLabProvider{client: m.client}
		case "gitea":
			p = &GiteaProvider{client: m.client}
		default:
			p = &GenericProvider{client: m.client}
		}
		return p.Fetch(ctx, gitURL, ref, filepath, auth)
	})
	
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

type GitHubProvider struct {
	client *http.Client
}

func (p *GitHubProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	owner, repo, err := parseGitHubURL(gitURL)
	if err != nil {
		return nil, err
	}

	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, ref, filepath)
	
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}

	if auth != nil && auth.Token != "" {
		req.Header.Set("Authorization", "token "+auth.Token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func parseGitHubURL(gitURL string) (owner, repo string, err error) {
	u, err := url.Parse(gitURL)
	if err != nil {
		return "", "", err
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", errors.New("invalid github url")
	}

	owner = parts[0]
	repo = parts[1]
	
	// 如果 URL 包含 /tree/ 或 /blob/，只取前两部分（owner/repo）
	// 例如：github.com/owner/repo/tree/main/path -> owner, repo
	repo = strings.TrimSuffix(repo, ".git")
	
	return owner, repo, nil
}

// 从浏览器 URL 中提取标准 Git URL 和 ref
// 例如：https://github.com/owner/repo/tree/main/path -> https://github.com/owner/repo, main
func normalizeGitHubURL(inputURL string) (gitURL, ref string, err error) {
	u, err := url.Parse(inputURL)
	if err != nil {
		return "", "", err
	}
	
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", errors.New("invalid github url")
	}
	
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	gitURL = fmt.Sprintf("https://%s/%s/%s", u.Host, owner, repo)
	
	// 检查是否包含 /tree/{ref} 或 /blob/{ref}
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
		ref = parts[3]
	} else {
		ref = "main" // 默认分支
	}
	
	return gitURL, ref, nil
}

type GitLabProvider struct {
	client *http.Client
}

func (p *GitLabProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	host, projectPath, err := parseGitLabURL(gitURL)
	if err != nil {
		return nil, err
	}

	encodedPath := url.PathEscape(filepath)
	encodedProject := url.PathEscape(projectPath)
	apiURL := fmt.Sprintf("https://%s/api/v4/projects/%s/repository/files/%s/raw?ref=%s",
		host, encodedProject, encodedPath, url.QueryEscape(ref))

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, err
	}

	if auth != nil && auth.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", auth.Token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gitlab returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func parseGitLabURL(gitURL string) (host, projectPath string, err error) {
	u, err := url.Parse(gitURL)
	if err != nil {
		return "", "", err
	}

	host = u.Host
	projectPath = strings.Trim(u.Path, "/")
	projectPath = strings.TrimSuffix(projectPath, ".git")
	return host, projectPath, nil
}

type GiteaProvider struct {
	client *http.Client
}

func (p *GiteaProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	host, owner, repo, err := parseGiteaURL(gitURL)
	if err != nil {
		return nil, err
	}

	rawURL := fmt.Sprintf("https://%s/%s/%s/raw/branch/%s/%s", host, owner, repo, ref, filepath)

	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}

	if auth != nil && auth.Token != "" {
		req.Header.Set("Authorization", "token "+auth.Token)
	} else if auth != nil && auth.Username != "" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gitea returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func parseGiteaURL(gitURL string) (host, owner, repo string, err error) {
	u, err := url.Parse(gitURL)
	if err != nil {
		return "", "", "", err
	}

	host = u.Host
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", "", errors.New("invalid gitea url")
	}

	owner = parts[0]
	repo = strings.TrimSuffix(parts[1], ".git")
	return host, owner, repo, nil
}

type GenericProvider struct {
	client *http.Client
}

func (p *GenericProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	return nil, errors.New("generic provider not yet implemented")
}

// IdentifyProvider 识别 Git URL 的提供商类型
func (m *Manager) IdentifyProvider(gitURL string) string {
	if strings.Contains(gitURL, "github.com") {
		return "github"
	}
	if strings.Contains(gitURL, "gitlab.com") || strings.Contains(gitURL, "gitlab") {
		return "gitlab"
	}
	if strings.Contains(gitURL, "gitea") {
		return "gitea"
	}
	return ""
}

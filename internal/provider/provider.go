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

var (
	ErrNotFound     = errors.New("file not found")
	ErrTooLarge     = errors.New("file too large")
	ErrAuthRequired = errors.New("authentication required")
)

// providerBase 各 provider 共享的配置：SSRF 白/黑名单与单文件大小上限。
type providerBase struct {
	allow   []string
	deny    []string
	maxSize int64
}

// checkSSRF 校验目标 URL 的 host 是否允许访问。
func (b providerBase) checkSSRF(rawURL string) error {
	if !allowedURL(rawURL, b.allow, b.deny) {
		return fmt.Errorf("target host not allowed (SSRF protection): %s", rawURL)
	}
	return nil
}

// readBody 读取响应体，受 maxSize 限制，超出返回 ErrTooLarge。
func (b providerBase) readBody(resp *http.Response) ([]byte, error) {
	if b.maxSize > 0 {
		r := io.LimitReader(resp.Body, b.maxSize+1)
		data, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > b.maxSize {
			return nil, ErrTooLarge
		}
		return data, nil
	}
	return io.ReadAll(resp.Body)
}

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
	client  *http.Client
	sf      singleflight.Group
	allow   []string
	deny    []string
	maxSize int64
}

func NewManager(timeout time.Duration, httpProxy, httpsProxy string, allow, deny []string, maxSize int64) *Manager {
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
		allow:   allow,
		deny:    deny,
		maxSize: maxSize,
	}
}

func (m *Manager) Fetch(ctx context.Context, providerType, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	key := fmt.Sprintf("%s:%s:%s:%s", providerType, gitURL, ref, filepath)

	v, err, _ := m.sf.Do(key, func() (interface{}, error) {
		var p Provider
		base := providerBase{allow: m.allow, deny: m.deny, maxSize: m.maxSize}
		switch providerType {
		case "github":
			p = &GitHubProvider{client: m.client, providerBase: base}
		case "gitlab":
			p = &GitLabProvider{client: m.client, providerBase: base}
		case "gitea":
			p = &GiteaProvider{client: m.client, providerBase: base}
		default:
			p = &GenericProvider{client: m.client, providerBase: base}
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
	providerBase
}

func (p *GitHubProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	owner, repo, err := parseGitHubURL(gitURL)
	if err != nil {
		return nil, err
	}

	// raw.githubusercontent.com 始终走 https，与仓库所在 host 的 scheme 无关
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repo, ref, filepath)
	if err := p.checkSSRF(rawURL); err != nil {
		return nil, err
	}

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

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuthRequired
	}
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github returned %d", resp.StatusCode)
	}

	return p.readBody(resp)
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

// NormalizeGitHubURL 从浏览器 URL 中提取标准 Git URL 和 ref。
// 例如：https://github.com/owner/repo/tree/main/path -> https://github.com/owner/repo, main
// 全项目唯一的权威实现，registry/server 复用，避免三处重复。
func NormalizeGitHubURL(inputURL string) (gitURL, ref string, err error) {
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
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	gitURL = fmt.Sprintf("%s://%s/%s/%s", scheme, u.Host, owner, repo)
	
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
	providerBase
}

func (p *GitLabProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	scheme, host, projectPath, err := parseGitLabURL(gitURL)
	if err != nil {
		return nil, err
	}

	encodedPath := url.PathEscape(filepath)
	encodedProject := url.PathEscape(projectPath)
	apiURL := fmt.Sprintf("%s://%s/api/v4/projects/%s/repository/files/%s/raw?ref=%s",
		scheme, host, encodedProject, encodedPath, url.QueryEscape(ref))
	if err := p.checkSSRF(apiURL); err != nil {
		return nil, err
	}

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

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuthRequired
	}
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gitlab returned %d", resp.StatusCode)
	}

	return p.readBody(resp)
}

func parseGitLabURL(gitURL string) (scheme, host, projectPath string, err error) {
	u, err := url.Parse(gitURL)
	if err != nil {
		return "", "", "", err
	}

	scheme = u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host = u.Host
	projectPath = strings.Trim(u.Path, "/")
	projectPath = strings.TrimSuffix(projectPath, ".git")
	if projectPath == "" {
		return "", "", "", errors.New("invalid gitlab url")
	}
	return scheme, host, projectPath, nil
}

type GiteaProvider struct {
	client *http.Client
	providerBase
}

func (p *GiteaProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	scheme, host, owner, repo, err := parseGiteaURL(gitURL)
	if err != nil {
		return nil, err
	}

	rawURL := fmt.Sprintf("%s://%s/%s/%s/raw/branch/%s/%s", scheme, host, owner, repo, ref, filepath)
	if err := p.checkSSRF(rawURL); err != nil {
		return nil, err
	}

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

	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, ErrAuthRequired
	}
	if resp.StatusCode == 404 {
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gitea returned %d", resp.StatusCode)
	}

	return p.readBody(resp)
}

func parseGiteaURL(gitURL string) (scheme, host, owner, repo string, err error) {
	u, err := url.Parse(gitURL)
	if err != nil {
		return "", "", "", "", err
	}

	scheme = u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	host = u.Host
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", "", "", errors.New("invalid gitea url")
	}

	owner = parts[0]
	repo = strings.TrimSuffix(parts[1], ".git")
	return scheme, host, owner, repo, nil
}

type GenericProvider struct {
	client *http.Client
	providerBase
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

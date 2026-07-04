package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

var (
	ErrNotFound     = errors.New("file not found")
	ErrTooLarge     = errors.New("file too large")
	ErrAuthRequired = errors.New("authentication required")
)

// probeTimeout 限制单次 provider 探测的耗时，避免死 host 拖慢注册。
const probeTimeout = 5 * time.Second

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

// hasAuth 报告 auth 是否携带了可用凭据（token 或账号密码）。
// 用于区分 404 的两种语义：GitHub/GitLab/Gitea 对私有仓库在无凭据时统一返回 404
// （不泄露仓库存在性）而非 401，因此无凭据的 404 应视为「需要鉴权」并触发前端凭据输入；
// 携带凭据仍 404 则视为真实的 not-found（文件/分支不存在或凭据无权限）。
func hasAuth(a *Auth) bool {
	return a != nil && (a.Token != "" || a.Username != "")
}

type Provider interface {
	Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error)
}

type Manager struct {
	client     *http.Client
	sf         singleflight.Group
	allow      []string
	deny       []string
	maxSize    int64
	probeCache sync.Map // host(string) -> providerType(string)；只缓存正例
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
		if !hasAuth(auth) {
			// 无凭据时 GitHub 对私有仓库返回 404（不泄露存在性），视为需要鉴权
			return nil, ErrAuthRequired
		}
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
		if !hasAuth(auth) {
			return nil, ErrAuthRequired
		}
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
		if !hasAuth(auth) {
			return nil, ErrAuthRequired
		}
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

// IdentifyProvider 识别 Git URL 的提供商类型。
// 已知关键字（github.com / gitlab.com / 含 gitea / gitlab 子串）走快速路径，
// 不发网络请求；自托管实例（域名不含关键字）回退到 API 探测：
//   GET <scheme>://<host>/api/v1/version  → 200 即 Gitea
//   GET <scheme>://<host>/api/v4/version  → 200 即 GitLab
// 探测正例按 host 缓存，避免对同一自托管站点重复探测；探测走 m.client（代理/超时生效），
// 且每个探测 URL 先过 SSRF 校验。均未命中返回 ""。
func (m *Manager) IdentifyProvider(gitURL string) string {
	// 快速路径：关键字匹配，零网络开销
	if strings.Contains(gitURL, "github.com") {
		return "github"
	}
	if strings.Contains(gitURL, "gitlab.com") || strings.Contains(gitURL, "gitlab") {
		return "gitlab"
	}
	if strings.Contains(gitURL, "gitea") {
		return "gitea"
	}

	// 回退：API 探测自托管实例
	scheme, host, ok := probeHost(gitURL)
	if !ok {
		return ""
	}
	if v, ok := m.probeCache.Load(host); ok {
		return v.(string)
	}

	// singleflight 去重：同一 host 的并发探测只发一次
	v, err, _ := m.sf.Do("probe:"+host, func() (interface{}, error) {
		if v, ok := m.probeCache.Load(host); ok { // 二次查缓存，前一个 flight 可能刚写入
			return v.(string), nil
		}
		pt := m.probeProvider(scheme, host)
		if pt != "" {
			m.probeCache.Store(host, pt)
		}
		return pt, nil
	})
	if err != nil {
		return ""
	}
	return v.(string)
}

// probeHost 从 gitURL 解析 scheme 与 host（含端口），scheme 缺省 https。
// 与 parseGiteaURL/parseGitLabURL 解耦：只需 scheme+host，对裸域名也成立。
func probeHost(gitURL string) (scheme, host string, ok bool) {
	u, err := url.Parse(gitURL)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	scheme = u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme, u.Host, true
}

// SsrfHint 检测 gitURL 的 host 是否因解析到私网 IP 而被 SSRF 规则拦截
// （且未被 allow 列表放行）。命中则返回给用户的可操作提示，否则返回 ""。
// 用于把 "unsupported git provider" 这种含糊错误具体化。
func (m *Manager) SsrfHint(gitURL string) string {
	_, host, ok := probeHost(gitURL)
	if !ok {
		return ""
	}
	// 复用 SSRF 判定：若 allow 已放行就不会进这里（provider 已识别成功）。
	// 这里只看「host 解析到私网且未在 allow」这一种情况。
	h := hostOnly(host)
	if ip := net.ParseIP(h); ip != nil {
		if isPrivateIP(ip) {
			return "host is a private IP; start server with --allow-host " + h + " (or config fetch.allow_hosts) to trust this self-hosted instance"
		}
		return ""
	}
	ips, err := net.LookupIP(h)
	if err != nil {
		return ""
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return "host resolves to a private IP; start server with --allow-host " + h + " (or config fetch.allow_hosts) to trust this self-hosted instance"
		}
	}
	return ""
}

// probeProvider 依次探测 Gitea(/api/v1/version) 与 GitLab(/api/v4/version)。
// 每个探测 URL 先过 SSRF 校验，再用 m.client 发送（复用代理与超时）。
// 命中 200 即返回 "gitea"/"gitlab"；均未命中（含 SSRF 拦截、超时、非 200）返回 ""。
func (m *Manager) probeProvider(scheme, host string) string {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	probes := []struct {
		typ string
		end string
	}{
		{"gitea", fmt.Sprintf("%s://%s/api/v1/version", scheme, host)},
		{"gitlab", fmt.Sprintf("%s://%s/api/v4/version", scheme, host)},
	}
	for _, p := range probes {
		if !allowedURL(p.end, m.allow, m.deny) {
			continue // SSRF 拦截：跳过该探测
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.end, nil)
		if err != nil {
			continue
		}
		resp, err := m.client.Do(req)
		if err != nil {
			continue // 超时/连接失败：试下一个
		}
		io.Copy(io.Discard, resp.Body) // 排空以便连接复用
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return p.typ
		}
	}
	return ""
}

package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"golang.org/x/time/rate"
)

var (
	ErrNotFound     = errors.New("file not found")
	ErrTooLarge     = errors.New("file too large")
	ErrAuthRequired = errors.New("authentication required")
	ErrRateLimited  = errors.New("rate limit exceeded for this host")
)

// probeTimeout 限制单次 provider 探测的耗时，避免死 host 拖慢注册。
const probeTimeout = 5 * time.Second

// 公网 GitHub 的默认地址。企业版（GHE）由 fetch.github_base_url 覆盖，
// 此时 API 走 <base>/api/v3、raw 走 <base>/{owner}/{repo}/raw/{ref}/{path}。
const (
	defaultGitHubAPIBase = "https://api.github.com"
	defaultGitHubRawBase = "https://raw.githubusercontent.com"
)

// providerBase 各 provider 共享的配置：SSRF 白/黑名单与单文件大小上限。
type providerBase struct {
	allow   []string
	deny    []string
	maxSize int64
	// githubBase 非空时表示 GitHub 企业版/自建实例的站点根（不含末尾 /），
	// githubBaseHost 是它的 host。只有 git_url 指向该 host 的仓库才走企业版地址，
	// 同一实例配置下访问公网 github.com 仓库仍走公网默认，互不串台。
	githubBase     string
	githubBaseHost string
	// identity 强制上游不做传输编码。Go 的 client 默认带 Accept-Encoding: gzip，
	// CDN（如 raw.githubusercontent.com）于是改用 chunked 且**不回 content-length** ——
	// 流式进度条的分母当场消失。实测同一文件：带 gzip 时长度未知，identity 时精确。
	identity bool
}

// useGitHubEnterprise 报告仓库所在 host 是否就是配置的 GitHub 企业版实例。
func (b providerBase) useGitHubEnterprise(gitHost string) bool {
	return b.githubBase != "" && b.githubBaseHost != "" && strings.EqualFold(gitHost, b.githubBaseHost)
}

// githubRaw 生成 GitHub raw 文件 URL。公网与 GHE 的路径形态不同：
// 公网是 raw.githubusercontent.com/{owner}/{repo}/{ref}/{path}，
// GHE 是 {base}/{owner}/{repo}/raw/{ref}/{path}。
func (b providerBase) githubRaw(gitHost, owner, repo, ref, filepath string) string {
	if b.useGitHubEnterprise(gitHost) {
		return fmt.Sprintf("%s/%s/%s/raw/%s/%s", b.githubBase, owner, repo, ref, filepath)
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s", defaultGitHubRawBase, owner, repo, ref, filepath)
}

// githubAPI 生成 GitHub REST API URL（path 以 / 开头）。
func (b providerBase) githubAPI(gitHost, path string) string {
	if b.useGitHubEnterprise(gitHost) {
		return b.githubBase + "/api/v3" + path
	}
	return defaultGitHubAPIBase + path
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

// mapStatus 把上游状态码翻译成包内哨兵错误。缓冲版 Fetch 与流式版 FetchStream 必须共用
// 这一份映射：两条路径对同一个 404 给出不同语义，前端的凭据弹窗就会时灵时不灵。
func mapStatus(resp *http.Response, providerName string, auth *Auth) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrAuthRequired
	case http.StatusNotFound:
		if !hasAuth(auth) {
			return ErrAuthRequired
		}
		return ErrNotFound
	case http.StatusOK:
		return nil
	}
	return fmt.Errorf("%s returned %d", providerName, resp.StatusCode)
}

// applyAuth 是三种 provider 凭据头差异的唯一实现点：GitHub/Gitea 用
// `Authorization: token`，GitLab 用 `PRIVATE-TOKEN`，Gitea 另接受 HTTP Basic。
// provider 的取文件、文件树、分支列表都只调这一个入口，新增平台时不必再逐处改头。
func applyAuth(req *http.Request, providerType string, auth *Auth) {
	if auth == nil {
		return
	}
	switch providerType {
	case "github":
		if auth.Token != "" {
			req.Header.Set("Authorization", "token "+auth.Token)
		}
	case "gitlab":
		if auth.Token != "" {
			req.Header.Set("PRIVATE-TOKEN", auth.Token)
		}
	case "gitea":
		if auth.Token != "" {
			req.Header.Set("Authorization", "token "+auth.Token)
		} else if auth.Username != "" {
			req.SetBasicAuth(auth.Username, auth.Password)
		}
	}
}

// do 发请求但不做状态码判定，返回未读响应体，供需要自行处理非 200 的调用方使用
// （如 Gitea 的 ref 类型探测要在一个 404 之后继续下一个候选）。
func (b providerBase) do(ctx context.Context, client *http.Client, providerName, rawURL string, auth *Auth) (*http.Response, error) {
	if err := b.checkSSRF(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	if b.identity {
		req.Header.Set("Accept-Encoding", "identity")
	}
	applyAuth(req, providerName, auth)
	return client.Do(req)
}

// open 发请求并完成状态码判定，把**尚未读取**的响应体交给调用方：缓冲版在它上面调
// readBody，流式版直接把 Body 接走边收边转发。非 200 时 open 已负责 Close。
func (b providerBase) open(ctx context.Context, client *http.Client, providerName, rawURL string, auth *Auth) (*http.Response, error) {
	resp, err := b.do(ctx, client, providerName, rawURL, auth)
	if err != nil {
		return nil, err
	}
	if err := mapStatus(resp, providerName, auth); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
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

// AuthFingerprint 把凭据折叠成不可逆的短指纹，是按凭据隔离共享路径的唯一实现：
// Fetch 的 singleflight 合并键与 server 层内容缓存键都用它。键里只放截断 sha256，
// 原文不进键、也不进日志；匿名（nil）得到固定标记 "anon"，与任何凭据互不相同。
func AuthFingerprint(a *Auth) string {
	if a == nil {
		return "anon"
	}
	h := sha256.New()
	// 逐字段加 \x00 分隔，避免「字段拼接相同但切分不同」的歧义。
	for _, part := range []string{a.Type, a.Token, a.Username, a.Password} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

type Manager struct {
	client         *http.Client
	sf             singleflight.Group
	allow          []string
	deny           []string
	maxSize        int64
	githubBase     string   // GitHub 企业版站点根；空 = 只用公网 github.com
	githubBaseHost string   // githubBase 的 host，用于判断仓库是否属于该实例
	probeCache     sync.Map // host(string) -> providerType(string)；只缓存正例

	// giteaRefKinds 缓存 Gitea ref 探测结果：host/owner/repo:ref -> branch|tag|commit。
	// 同一站点的每个文件都要取，不缓存会让 tag/commit ref 的站点每个文件多打两次上游。
	giteaRefKinds sync.Map

	// rateLimiters: 按 host 分桶的令牌桶限流器。
	// 每个 host 独立限流，避免单平台被过度请求，也避免某平台限流影响其他平台。
	// 默认 100 req/min（约 6000 req/hour），可通过配置调整。
	rateLimit    int      // 每分钟允许的请求数
	rateBurst    int      // 令牌桶突发容量（默认与 rateLimit 相同）
	rateLimiters sync.Map // host(string) -> *rate.Limiter
}

func NewManager(timeout time.Duration, httpProxy, httpsProxy string, allow, deny []string, maxSize int64, rateLimit int, githubBaseURL string) *Manager {
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

	// 默认限流 100 req/min
	if rateLimit <= 0 {
		rateLimit = 100
	}

	githubBase := strings.TrimRight(githubBaseURL, "/")
	githubBaseHost := ""
	if githubBase != "" {
		if u, err := url.Parse(githubBase); err == nil {
			githubBaseHost = u.Host
		}
	}

	return &Manager{
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		allow:          allow,
		deny:           deny,
		maxSize:        maxSize,
		githubBase:     githubBase,
		githubBaseHost: githubBaseHost,
		rateLimit:      rateLimit,
		rateBurst:      rateLimit, // burst 与 rate 相同，允许短时间内集中请求
	}
}

// base 构造各 provider 共享的配置。identity 只在流式路径开启。
func (m *Manager) base(identity bool) providerBase {
	return providerBase{
		allow:          m.allow,
		deny:           m.deny,
		maxSize:        m.maxSize,
		githubBase:     m.githubBase,
		githubBaseHost: m.githubBaseHost,
		identity:       identity,
	}
}

func (m *Manager) newGiteaProvider(base providerBase) *GiteaProvider {
	return &GiteaProvider{client: m.client, providerBase: base, refKinds: &m.giteaRefKinds}
}

// getLimiter 返回指定 host 的 rate limiter，按需创建。
func (m *Manager) getLimiter(host string) *rate.Limiter {
	if v, ok := m.rateLimiters.Load(host); ok {
		return v.(*rate.Limiter)
	}

	limiter := rate.NewLimiter(rate.Limit(m.rateLimit)/60, m.rateBurst) // rate.Limit 是每秒请求数
	m.rateLimiters.Store(host, limiter)
	return limiter
}

func (m *Manager) Fetch(ctx context.Context, providerType, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	// 先检查 host 限流
	host := extractHost(gitURL)
	if host != "" {
		limiter := m.getLimiter(host)
		if !limiter.Allow() {
			return nil, ErrRateLimited
		}
	}

	// 合并键必须混入凭据指纹：singleflight 会把同 key 的并发请求共享同一次上游
	// 结果，不含凭据时匿名请求可能复用带凭据请求的上游结果，读到私有内容（与
	// server 层缓存键同一隔离边界）。相同凭据的并发仍正常合并。
	key := fmt.Sprintf("%s:%s:%s:%s:%s", providerType, gitURL, ref, filepath, AuthFingerprint(auth))

	v, err, _ := m.sf.Do(key, func() (interface{}, error) {
		base := m.base(false)
		switch providerType {
		case "github":
			return (&GitHubProvider{client: m.client, providerBase: base}).Fetch(ctx, gitURL, ref, filepath, auth)
		case "gitlab":
			return (&GitLabProvider{client: m.client, providerBase: base}).Fetch(ctx, gitURL, ref, filepath, auth)
		case "gitea":
			return m.newGiteaProvider(base).Fetch(ctx, gitURL, ref, filepath, auth)
		default:
			return nil, fmt.Errorf("unsupported provider type: %s", providerType)
		}
	})

	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// Stream 是一次尚未读取的上游响应。Body 必须由调用方 Close。
type Stream struct {
	Body io.ReadCloser
	// ContentLength 是上游声明的字节数；未知时为 -1（net/http 的约定），不是 0。
	ContentLength int64
}

// FetchStream 与 Fetch 共用同一套定位、SSRF、限流与状态码映射，区别只在于不把响应读进
// 内存 —— 需要边收边把真实字节进度报给浏览器时走这条。
// 刻意绕开 singleflight：一份 Body 只能被一个读者消费，合流会把空 body 交给后来者。
func (m *Manager) FetchStream(ctx context.Context, providerType, gitURL, ref, filepath string, auth *Auth) (*Stream, error) {
	// 先检查 host 限流
	host := extractHost(gitURL)
	if host != "" {
		limiter := m.getLimiter(host)
		if !limiter.Allow() {
			return nil, ErrRateLimited
		}
	}

	base := m.base(true)
	var (
		resp *http.Response
		err  error
	)
	switch providerType {
	case "github":
		resp, err = (&GitHubProvider{client: m.client, providerBase: base}).openFetch(ctx, gitURL, ref, filepath, auth)
	case "gitlab":
		resp, err = (&GitLabProvider{client: m.client, providerBase: base}).openFetch(ctx, gitURL, ref, filepath, auth)
	case "gitea":
		resp, err = m.newGiteaProvider(base).openFetch(ctx, gitURL, ref, filepath, auth)
	default:
		return nil, fmt.Errorf("unsupported provider type: %s", providerType)
	}
	if err != nil {
		return nil, err
	}

	// 缓冲版是读满 maxSize 才报错；流式版把响应头先发出去之后就再也改不了状态码，
	// 所以上游既然报了长度，就在写头之前拦下来，让 server 还能正常回 413。
	if base.maxSize > 0 && resp.ContentLength > base.maxSize {
		resp.Body.Close()
		return nil, ErrTooLarge
	}
	// 我们手工带的 Accept-Encoding: identity 不在 Go 的透明解压保护范围内：
	// 上游若无视它仍回压缩字节，原样泵给浏览器就是一份静默损坏的文件。宁可报错。
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		resp.Body.Close()
		return nil, fmt.Errorf("%s streamed with Content-Encoding=%s, refusing to relay compressed bytes", providerType, enc)
	}

	var body io.ReadCloser = resp.Body
	// 缓冲版由 readBody 一次性卡住 maxSize；流式版把字节直接泵给客户端，没有这道闸门的
	// 话，一个谎报或干脆不给 Content-Length 的上游就能借本服务无限外发。
	if base.maxSize > 0 {
		body = &maxSizeReader{ReadCloser: resp.Body, limit: base.maxSize}
	}
	return &Stream{Body: body, ContentLength: resp.ContentLength}, nil
}

// maxSizeReader 在流式路径上复刻 maxSize 闸门：累计放行超过上限后，后续读一律
// ErrTooLarge。调用方据此停止转发，并且绝不写缓存。
type maxSizeReader struct {
	io.ReadCloser
	limit int64
	read  int64
}

func (r *maxSizeReader) Read(p []byte) (int, error) {
	if r.read > r.limit {
		return 0, ErrTooLarge
	}
	n, err := r.ReadCloser.Read(p)
	r.read += int64(n)
	return n, err
}

type GitHubProvider struct {
	client *http.Client
	providerBase
}

func (p *GitHubProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	resp, err := p.openFetch(ctx, gitURL, ref, filepath, auth)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return p.readBody(resp)
}

// openFetch 定位上游并取回未读响应体。URL 构造与凭据头是 provider 差异，其余共用 open。
func (p *GitHubProvider) openFetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) (*http.Response, error) {
	owner, repo, err := parseGitHubURL(gitURL)
	if err != nil {
		return nil, err
	}
	_, gitHost, _ := probeHost(gitURL)

	// 仓库所在 host 等于配置的 GHE 实例时走 <base>/{owner}/{repo}/raw/{ref}/{path}，
	// 否则走公网 raw.githubusercontent.com（始终 https，与仓库 host 的 scheme 无关）。
	rawURL := p.providerBase.githubRaw(gitHost, owner, repo, ref, filepath)
	return p.providerBase.open(ctx, p.client, "github", rawURL, auth)
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
	resp, err := p.openFetch(ctx, gitURL, ref, filepath, auth)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return p.readBody(resp)
}

func (p *GitLabProvider) openFetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) (*http.Response, error) {
	scheme, host, projectPath, err := parseGitLabURL(gitURL)
	if err != nil {
		return nil, err
	}

	encodedPath := url.PathEscape(filepath)
	encodedProject := url.PathEscape(projectPath)
	apiURL := fmt.Sprintf("%s://%s/api/v4/projects/%s/repository/files/%s/raw?ref=%s",
		scheme, host, encodedProject, encodedPath, url.QueryEscape(ref))
	return p.providerBase.open(ctx, p.client, "gitlab", apiURL, auth)
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
	// refKinds 缓存 ref 探测结果，可为 nil（不缓存）。
	refKinds *sync.Map
}

func (p *GiteaProvider) Fetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) ([]byte, error) {
	resp, err := p.openFetch(ctx, gitURL, ref, filepath, auth)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return p.readBody(resp)
}

// giteaRefForms 是 Gitea raw URL 支持的 ref 形式，按常见程度排序。
// Gitea 不会把 tag/commit 当成 branch 解析：同一个 SHA 在 /raw/branch/<sha> 下 404，
// 在 /raw/commit/<sha> 下 200，因此必须按形式探测而不能只拼 branch。
var giteaRefForms = []string{"branch", "tag", "commit"}

// refOrder 返回本次要尝试的 ref 形式。命中缓存时把缓存过的形式排到最前：
// 后续文件一次请求即可，同时保留回退——ref 从 tag 改成同名 branch 后缓存会自愈。
func (p *GiteaProvider) refOrder(key string) []string {
	if p.refKinds == nil {
		return giteaRefForms
	}
	v, ok := p.refKinds.Load(key)
	if !ok {
		return giteaRefForms
	}
	cached := v.(string)
	order := make([]string, 0, len(giteaRefForms))
	order = append(order, cached)
	for _, form := range giteaRefForms {
		if form != cached {
			order = append(order, form)
		}
	}
	return order
}

func (p *GiteaProvider) openFetch(ctx context.Context, gitURL, ref, filepath string, auth *Auth) (*http.Response, error) {
	scheme, host, owner, repo, err := parseGiteaURL(gitURL)
	if err != nil {
		return nil, err
	}

	key := fmt.Sprintf("%s/%s/%s:%s", host, owner, repo, ref)
	for _, form := range p.refOrder(key) {
		rawURL := fmt.Sprintf("%s://%s/%s/%s/raw/%s/%s/%s", scheme, host, owner, repo, form, ref, filepath)
		resp, err := p.providerBase.do(ctx, p.client, "gitea", rawURL, auth)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case http.StatusOK:
			if p.refKinds != nil {
				p.refKinds.Store(key, form)
			}
			return resp, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			// 上游说凭据不行，换 ref 形式没有意义。
			resp.Body.Close()
			return nil, ErrAuthRequired
		case http.StatusNotFound:
			// 可能只是 ref 形式不对：关掉响应，试下一种。
			resp.Body.Close()
		default:
			resp.Body.Close()
			return nil, fmt.Errorf("gitea returned %d", resp.StatusCode)
		}
	}

	// 三种形式都 404：沿用全局语义 —— 无凭据视为需要鉴权（私有仓库不泄露存在性），
	// 携带凭据才是真的 not-found（ref/文件不存在或凭据无权限）。
	if !hasAuth(auth) {
		return nil, ErrAuthRequired
	}
	return nil, ErrNotFound
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

// IdentifyProvider 识别 Git URL 的提供商类型。
// 已知关键字（github.com / gitlab.com / 含 gitea / gitlab 子串）走快速路径，
// 不发网络请求；自托管实例（域名不含关键字）回退到 API 探测：
//
//	GET <scheme>://<host>/api/v1/version  → 200 即 Gitea
//	GET <scheme>://<host>/api/v4/version  → 200 即 GitLab
//
// 探测正例按 host 缓存，避免对同一自托管站点重复探测；探测走 m.client（代理/超时生效），
// 且每个探测 URL 先过 SSRF 校验。均未命中返回 ""。
func (m *Manager) IdentifyProvider(gitURL string) string {
	// 配置了 GitHub 企业版/自建实例时，它的 host 直接识别为 github：
	// GHE 域名通常不含 "github.com"，靠下面的 API 探测也探不出来（GHE 有 /api/v3，
	// 但没有 Gitea/GitLab 的 version 端点）。
	if m.githubBaseHost != "" {
		if _, host, ok := probeHost(gitURL); ok && strings.EqualFold(host, m.githubBaseHost) {
			return "github"
		}
	}

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

// extractHost 从 git URL 中提取 host（不含端口），用于限流分桶。
// 若 URL 解析失败则返回空字符串（不限流）。
func extractHost(gitURL string) string {
	u, err := url.Parse(gitURL)
	if err != nil || u.Host == "" {
		return ""
	}
	// 只取 hostname，不含端口
	h, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		// u.Host 可能不含端口，直接返回
		return u.Host
	}
	return h
}

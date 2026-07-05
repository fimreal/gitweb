package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/limingrui/gitweb/internal/cache"
	"github.com/limingrui/gitweb/internal/provider"
	"github.com/limingrui/gitweb/internal/registry"
	"github.com/limingrui/gitweb/internal/render"
)

// viewerCSP 是 viewer 页面下发的严格 CSP：
// 限制脚本来源为同源 + 本页一次性 nonce（用于内联启动脚本），防止用户仓库 HTML
// 的脚本逃逸；图片允许任意源与 data/blob；frame-src 允许同源 iframe（承载用户 HTML）。
// nonce 每次请求随机生成，仅本页内联脚本可用。
func viewerCSP(nonce string) string {
	return "default-src 'self'; script-src 'self' 'nonce-" + nonce + "'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data: blob: *; connect-src 'self'; frame-src 'self'"
}

// newNonce 生成 16 字节随机 base64url nonce，用于 CSP 内联脚本放行。
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type Server struct {
	registry    *registry.Registry
	provider    *provider.Manager
	cache       *cache.Cache
	renderer    *render.Renderer
	baseURL     string
	maxFileSize int64
	password    string // 非空时所有页面需登录后才能访问
}

func New(reg *registry.Registry, prov *provider.Manager, c *cache.Cache, rend *render.Renderer, baseURL string, maxFileSize int64, password string) *Server {
	return &Server{
		registry:    reg,
		provider:    prov,
		cache:       c,
		renderer:    rend,
		baseURL:     baseURL,
		maxFileSize: maxFileSize,
		password:    password,
	}
}

func (s *Server) SetupRoutes() *gin.Engine {
	r := gin.Default()

	r.GET("/healthz", s.handleHealth)

	// 若设置了 password，所有路由都需要登录（cookie 校验）。
	// 公开路径（/login、/logout、/static/*、/healthz）由 middleware 跳过。
	r.GET("/login", s.handleLoginPage)
	r.POST("/login", s.handleLoginSubmit)
	r.GET("/logout", s.handleLogout)
	r.Use(s.authMiddleware())

	r.GET("/", s.handleIndex)

	api := r.Group("/api")
	{
		api.POST("/sites", s.handleRegisterSite)
		api.GET("/sites", s.handleListSites)
		api.DELETE("/sites/:pathid", s.handleDeleteSite)
		api.POST("/sites/:pathid/refresh", s.handleRefreshSite)
		api.GET("/sites/:pathid/tree", s.handleGetTree)
		api.GET("/sites/:pathid/branches", s.handleListBranches)
	}

	// /:pathid/*filepath 同时承载 viewer 页面（空 filepath）与文件内容（有 filepath）。
	// Gin 不允许 catch-all 与独立 /:pathid/ 路由共存，故合并到一条路由，空路径走 viewer。
	r.GET("/:pathid/*filepath", s.handleSiteFile)

	return r
}

// isPublicPath 返回不需要登录就能访问的路径。仅在 s.password != "" 时生效。
func isPublicPath(p string) bool {
	if p == "/login" || p == "/logout" || p == "/healthz" {
		return true
	}
	return strings.HasPrefix(p, "/static/")
}

// loginCookieName 是登录凭证 cookie 的名称。
const loginCookieName = "gitweb_auth"

// repoAuthCookieName 是 viewer 私有仓库凭据 cookie 的前缀。
// 前端在保存凭据时同步写到 localStorage + cookie，浏览器导航（地址栏直接访问
// /<pathid>/<file>）会自动带 cookie，后端从中恢复 Authorization 头。
const repoAuthCookieName = "gitweb_repo_auth"

// authMiddleware 校验登录 cookie；未通过则重定向到 /login。
// 仅在 s.password != "" 时挂载。
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.password == "" || isPublicPath(c.Request.URL.Path) || s.checkLogin(c) {
			c.Next()
			return
		}
		// API 请求返回 401，页面请求 302 到 /login
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		redirect := c.Request.URL.RequestURI()
		c.Redirect(http.StatusFound, "/login?redirect="+base64.RawURLEncoding.EncodeToString([]byte(redirect)))
		c.Abort()
	}
}

// checkLogin 常量时间比较 cookie 值与 s.password。
func (s *Server) checkLogin(c *gin.Context) bool {
	if s.password == "" {
		return true
	}
	v, err := c.Cookie(loginCookieName)
	if err != nil || v == "" {
		return false
	}
	// 简单字符串等值比较；常量时间避免 timing 攻击
	return subtle.ConstantTimeCompare([]byte(v), []byte(s.password)) == 1
}

// handleLoginPage 渲染登录页。若有 redirect query 则带回到表单 hidden 字段。
func (s *Server) handleLoginPage(c *gin.Context) {
	if s.password == "" {
		c.Redirect(http.StatusFound, "/")
		return
	}
	redirect := c.Query("redirect")
	if redirect != "" {
		if raw, err := base64.RawURLEncoding.DecodeString(redirect); err == nil {
			redirect = string(raw)
		} else {
			redirect = ""
		}
	}
	c.HTML(http.StatusOK, "login.html", gin.H{
		"BaseURL":  s.baseURL,
		"Redirect": redirect,
		"Error":   "",
	})
}

// handleLoginSubmit 校验密码并种 cookie，成功则重定向到 redirect 或 /。
func (s *Server) handleLoginSubmit(c *gin.Context) {
	if s.password == "" {
		c.Redirect(http.StatusFound, "/")
		return
	}
	pw := c.PostForm("password")
	redirect := c.PostForm("redirect")
	if subtle.ConstantTimeCompare([]byte(pw), []byte(s.password)) == 1 {
		// 种 cookie：HttpOnly + SameSite=Lax + 30 天
		c.SetSameSite(http.SameSiteLaxMode)
		c.SetCookie(loginCookieName, pw, 30*24*3600, "/", "", false, true)
		if redirect == "" || strings.HasPrefix(redirect, "/") == false {
			redirect = "/"
		}
		c.Redirect(http.StatusFound, redirect)
		return
	}
	c.HTML(http.StatusUnauthorized, "login.html", gin.H{
		"BaseURL":  s.baseURL,
		"Redirect": redirect,
		"Error":   true,
	})
}

// handleLogout 清除 cookie 后回到登录页（若已设 password；否则直接回首页）。
func (s *Server) handleLogout(c *gin.Context) {
	if s.password != "" {
		c.SetCookie(loginCookieName, "", -1, "/", "", false, true)
		c.Redirect(http.StatusFound, "/login")
	} else {
		c.Redirect(http.StatusFound, "/")
	}
}

func (s *Server) handleIndex(c *gin.Context) {
	c.HTML(http.StatusOK, "index.html", gin.H{
		"BaseURL": s.baseURL,
	})
}

func (s *Server) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) handleRegisterSite(c *gin.Context) {
	var req struct {
		GitURL string `json:"git_url"`
		PathID string `json:"pathid"`
		Ref    string `json:"ref"`
		Hidden bool   `json:"hidden"`
		// 注：注册不再带凭据。私有仓库凭据在访问时运行时输入，存浏览器 sessionStorage。
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
		return
	}

	if req.GitURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "git_url is required"})
		return
	}

	providerType := s.provider.IdentifyProvider(req.GitURL)
	if providerType == "" {
		// 自托管实例探测失败常见原因：域名解析到私网 IP 被 SSRF 拦截。
		// 给一个更可操作的提示，而不是泛泛的 "unsupported"。
		if hint := s.provider.SsrfHint(req.GitURL); hint != "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported git provider: " + hint})
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported git provider"})
		}
		return
	}

	gitURL := req.GitURL
	ref := req.Ref

	// 对 GitHub，标准化浏览器 URL（支持 /tree/{ref}/path 形式）
	if providerType == "github" {
		if normalizedURL, extractedRef, err := provider.NormalizeGitHubURL(req.GitURL); err == nil {
			gitURL = normalizedURL
			if ref == "" {
				ref = extractedRef
			}
		}
	}

	site, err := s.registry.Register(gitURL, req.PathID, ref, providerType, nil, req.Hidden)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"pathid": site.PathID,
		"url":    fmt.Sprintf("%s/%s/", s.requestBaseURL(c), site.PathID),
	})
}

func (s *Server) handleListSites(c *gin.Context) {
	sites := s.registry.ListPublic()
	result := make([]gin.H, len(sites))
	for i, site := range sites {
		result[i] = gin.H{
			"pathid":     site.PathID,
			"git_url":    site.GitURL,
			"ref":        site.Ref,
			"provider":   site.Provider,
			"created_at": site.CreatedAt,
			"views":      site.Views,
		}
	}
	c.JSON(http.StatusOK, gin.H{"sites": result})
}

func (s *Server) handleDeleteSite(c *gin.Context) {
	pathID := c.Param("pathid")
	if err := s.registry.Remove(pathID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	s.cache.Invalidate(pathID + ":")
	c.JSON(http.StatusOK, gin.H{"status": "deleted"})
}

func (s *Server) handleRefreshSite(c *gin.Context) {
	pathID := c.Param("pathid")
	if _, err := s.registry.Get(pathID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	s.cache.Invalidate(pathID + ":")
	c.JSON(http.StatusOK, gin.H{"status": "refreshed"})
}

func (s *Server) handleGetTree(c *gin.Context) {
	pathID := c.Param("pathid")

	site, err := s.registry.Get(pathID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
		return
	}

	// 凭据从请求头透传（运行时输入，存浏览器 sessionStorage），用完即弃。
	auth := authFromHeader(c, site.Auth)
	// 临时诊断：打印收到的 auth 类型（不打印凭据本身）
	authType := "none"
	if auth != nil {
		authType = auth.Type
		if auth.Type == "basic" {
			authType = "basic(user=" + auth.Username + ")"
		}
	}
	log.Printf("[auth-diag] tree pathid=%s authType=%s authHeader=%q", pathID, authType, c.GetHeader("Authorization")[:0]+"(len="+fmt.Sprint(len(c.GetHeader("Authorization")))+")")

	// ?ref= 用于 viewer 临时切换分支：覆盖 site.Ref，但不写入 state。
	ref := effectiveRef(c, site.Ref)

	tree, err := s.provider.FetchTree(context.Background(), site.Provider, site.GitURL, ref, auth)
	if err != nil {
		if errors.Is(err, provider.ErrAuthRequired) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		log.Printf("[tree-error] pathid=%s provider=%s giturl=%s ref=%s err=%v", pathID, site.Provider, site.GitURL, ref, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch tree: " + err.Error()})
		return
	}

	filtered := provider.FilterRenderableFiles(tree, s.renderer.IsRenderable)
	c.JSON(http.StatusOK, gin.H{"files": filtered})
}

// handleListBranches 列出仓库分支，供 viewer 临时切换分支下拉使用。
func (s *Server) handleListBranches(c *gin.Context) {
	pathID := c.Param("pathid")

	site, err := s.registry.Get(pathID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
		return
	}

	auth := authFromHeader(c, site.Auth)

	branches, err := s.provider.ListBranches(context.Background(), site.Provider, site.GitURL, auth)
	if err != nil {
		if errors.Is(err, provider.ErrAuthRequired) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to list branches: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"branches": branches, "current": site.Ref})
}

// effectiveRef 从 ?ref= query 取临时分支，空则回退 site 默认 ref。
func effectiveRef(c *gin.Context, defaultRef string) string {
	if r := strings.TrimSpace(c.Query("ref")); r != "" {
		return r
	}
	return defaultRef
}

func (s *Server) handleSiteFile(c *gin.Context) {
	pathID := c.Param("pathid")
	filepath := strings.TrimPrefix(c.Param("filepath"), "/")

	site, err := s.registry.Get(pathID)
	if err != nil {
		c.HTML(http.StatusNotFound, "error.html", gin.H{
			"Code":    404,
			"Message": "Site not found",
		})
		return
	}

	// 空 filepath：返回 viewer 页面骨架，下发严格 CSP（带本页 nonce）。
	if filepath == "" {
		nonce, err := newNonce()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"Code":    500,
				"Message": "failed to generate nonce",
			})
			return
		}
		// 浏览数 +1（内存累计，防抖落盘）
		s.registry.IncrementViews(pathID)
		c.Header("Content-Security-Policy", viewerCSP(nonce))
		c.HTML(http.StatusOK, "viewer.html", gin.H{
			"PathID":     pathID,
			"GitURL":     site.GitURL,
			"Ref":        site.Ref,
			"Provider":   site.Provider,
			"CreatedAt":  site.CreatedAt,
			"HasPresetAuth": site.Auth != nil, // 预置了凭据（私有仓库预置）；运行时注册恒为 false
			"RepoName":   repoNameFromURL(site.GitURL),
			"Views":      site.Views + 1, // +1 包含本次访问
			"Nonce":      nonce,
		})
		return
	}

	// 凭据从请求头透传（运行时输入），不与缓存关联、用完即弃。
	auth := authFromHeader(c, site.Auth)

	// ?ref= 临时切换分支（不写入 state）。缓存键带上 ref，避免不同分支同路径串缓存。
	ref := effectiveRef(c, site.Ref)

	// 缓存键不含凭据（凭据可能因用户而异）。缓存的是文件字节/片段，
	// 公开仓库内容对所有用户一致；私有仓库的内容也按 pathid:filepath 共享缓存——
	// 若需更严格隔离，可把缓存限定为公开仓库（此处按 YAGNI 暂不区分）。
	cacheKey := fmt.Sprintf("%s:%s:%s", pathID, ref, filepath)
	if cached, ok := s.cache.Get(cacheKey); ok {
		s.serveContent(c, filepath, cached)
		return
	}

	content, err := s.provider.Fetch(context.Background(), site.Provider, site.GitURL, ref, filepath, auth)
	if err != nil {
		s.serveFetchError(c, err)
		return
	}

	// 二进制/图片/HTML：直接缓存原始字节并透传；md/txt：缓存渲染片段。
	kind := s.renderer.Kind(filepath)
	switch kind {
	case render.KindMarkdown, render.KindText:
		rendered, err := s.renderer.Render(filepath, content)
		if err != nil {
			c.HTML(http.StatusInternalServerError, "error.html", gin.H{
				"Code":    500,
				"Message": "Render failed: " + err.Error(),
			})
			return
		}
		s.cache.Set(cacheKey, []byte(rendered))
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(rendered))
	default:
		// KindHTML / KindImage / KindBinary / KindUnsupported：透传原始字节
		s.cache.Set(cacheKey, content)
		s.serveContent(c, filepath, content)
	}
}

// serveContent 按文件类型设置 Content-Type 并返回原始字节。
func (s *Server) serveContent(c *gin.Context, filepath string, content []byte) {
	switch s.renderer.Kind(filepath) {
	case render.KindHTML:
		c.Data(http.StatusOK, "text/html; charset=utf-8", content)
	case render.KindImage:
		c.Data(http.StatusOK, contentTypeForImage(filepath), content)
	case render.KindText, render.KindMarkdown:
		// 走到这里说明是缓存命中的片段
		c.Data(http.StatusOK, "text/html; charset=utf-8", content)
	default:
		c.Data(http.StatusOK, "application/octet-stream", content)
	}
}

func (s *Server) serveFetchError(c *gin.Context, err error) {
	if errors.Is(err, provider.ErrNotFound) {
		c.HTML(http.StatusNotFound, "error.html", gin.H{
			"Code":    404,
			"Message": "File not found",
		})
		return
	}
	if errors.Is(err, provider.ErrAuthRequired) {
		// 401 让前端探测到并弹出凭据输入框
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	if errors.Is(err, provider.ErrTooLarge) {
		c.HTML(http.StatusRequestEntityTooLarge, "error.html", gin.H{
			"Code":    413,
			"Message": "File too large",
		})
		return
	}
	c.HTML(http.StatusBadGateway, "error.html", gin.H{
		"Code":    502,
		"Message": "Failed to fetch file: " + err.Error(),
	})
}

// authFromHeader 从请求的 Authorization 头解析凭据，用于私有仓库运行时鉴权。
// 支持的形式：
//   - "Bearer <token>" / "token <token>"：token 鉴权
//   - "PRIVATE-TOKEN: <token>"（GitLab 风格，单独头）
//   - "Basic <base64(user:pass)>"：账号密码
//
// 若请求未带凭据但站点预置了凭据（配置文件 sites.auth），回退到预置凭据。
// 凭据只用于本次请求，不暂存、不落盘、不进日志。
func authFromHeader(c *gin.Context, fallback *registry.Auth) *provider.Auth {
	a := &provider.Auth{}

	if h := c.GetHeader("Authorization"); h != "" {
		switch {
		case strings.HasPrefix(h, "Bearer "):
			a.Type = "token"
			a.Token = strings.TrimPrefix(h, "Bearer ")
		case strings.HasPrefix(h, "token "):
			a.Type = "token"
			a.Token = strings.TrimPrefix(h, "token ")
		case strings.HasPrefix(h, "Basic "):
			if raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic ")); err == nil {
				if parts := strings.SplitN(string(raw), ":", 2); len(parts) == 2 {
					a.Type = "basic"
					a.Username = parts[0]
					a.Password = parts[1]
				}
			}
		}
	}
	if a.Type == "" {
		if pt := c.GetHeader("PRIVATE-TOKEN"); pt != "" {
			a.Type = "token"
			a.Token = pt
		}
	}

	// 兜底1：从 cookie 恢复 viewer 端保存的私有仓库凭据。
	// 浏览器地址栏直接访问 /<pathid>/<file> 时无法发 Authorization 头，
	// 但同源请求会自动带 cookie，这里从中恢复鉴权。
	if a.Type == "" {
		if v, err := c.Cookie(repoAuthCookieName + "_" + c.Param("pathid")); err == nil && v != "" {
			decodeCookieAuth(v, a)
		}
	}

	if a.Type == "" && fallback != nil {
		return &provider.Auth{
			Type:     fallback.Type,
			Token:    fallback.Token,
			Username: fallback.Username,
			Password: fallback.Password,
		}
	}
	if a.Type == "" {
		return nil
	}
	return a
}

// decodeCookieAuth 从 cookie 值（base64 编码的 JSON {"type":"token","token":"...","username":"...","password":"..."}）
// 解析出 auth 到 a。失败时保持 a 不变。
func decodeCookieAuth(v string, a *provider.Auth) {
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		raw = []byte(v) // 兜底当作明文 JSON
	}
	var parsed struct {
		Type     string `json:"type"`
		Token    string `json:"token"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return
	}
	a.Type = parsed.Type
	a.Token = parsed.Token
	a.Username = parsed.Username
	a.Password = parsed.Password
}

// requestBaseURL 基于当前请求推导「访问本服务的根 URL」。
// 优先采用反代头（X-Forwarded-Proto / X-Forwarded-Host），否则回退到请求本身的
// scheme + Host 头。这样从 192.168.x.x、localhost 或公网域名访问时，返回的链接
// host 都与用户实际访问的一致，而非配置里固定的 baseURL。
// s.baseURL 仅在请求头缺失的极端情况下兜底。
func (s *Server) requestBaseURL(c *gin.Context) string {
	scheme := c.GetHeader("X-Forwarded-Proto")
	if scheme == "" {
		if c.Request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	if host == "" {
		return strings.TrimRight(s.baseURL, "/")
	}
	return scheme + "://" + host
}

// repoNameFromURL 从 git URL 解析出 owner/repo 作为显示名。
func repoNameFromURL(gitURL string) string {
	parts := strings.Split(strings.Trim(gitURL, "/"), "/")
	if len(parts) < 2 {
		return gitURL
	}
	// 形如 https://github.com/owner/repo -> ["https:", "", "github.com", "owner", "repo"]
	// 取最后两段
	owner := parts[len(parts)-2]
	repo := strings.TrimSuffix(parts[len(parts)-1], ".git")
	return owner + "/" + repo
}

func contentTypeForImage(filepath string) string {
	switch strings.ToLower(filepath[strings.LastIndex(filepath, "."):]) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".bmp":
		return "image/bmp"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

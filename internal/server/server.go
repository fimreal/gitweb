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
	"path"
	"strings"
	"sync"
	"time"

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
	registry       *registry.Registry
	provider       *provider.Manager
	cache          *cache.Cache
	renderer       *render.Renderer
	baseURL        string
	password       string
	sessions       sync.Map
	sessionExpiry  time.Duration
	loginFailures  sync.Map
	loginRateLimit time.Duration
	loginMaxFails  int
}

type sessionEntry struct {
	expiresAt time.Time
}

type loginFailEntry struct {
	count        int
	lockoutUntil time.Time
}

// loginFailGracePeriod 是登录失败条目在锁定过期后仍保留的宽限期。
// 期间条目不再阻塞登录，但保留计数用于"连续失败才锁定"的判定；
// 超过宽限期未再失败则被清理 goroutine 删除，避免 sync.Map 无限增长。
const loginFailGracePeriod = time.Hour

func New(reg *registry.Registry, prov *provider.Manager, c *cache.Cache, rend *render.Renderer, baseURL string, password string) *Server {
	s := &Server{
		registry:       reg,
		provider:       prov,
		cache:          c,
		renderer:       rend,
		baseURL:        baseURL,
		password:       password,
		sessionExpiry:  30 * 24 * time.Hour,
		loginRateLimit: 5 * time.Minute,
		loginMaxFails:  10,
	}
	// 仅在启用密码层时启动登录失败条目的过期清理，避免 sync.Map 无限增长。
	if password != "" {
		go s.cleanLoginFailures()
	}
	return s
}

// cleanLoginFailures 定期清理过期的登录失败条目。
// 与进程同生命周期，无需显式停止（进程退出即终止）。
func (s *Server) cleanLoginFailures() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-loginFailGracePeriod)
		s.loginFailures.Range(func(key, val interface{}) bool {
			entry := val.(*loginFailEntry)
			// 锁定已过期且超过宽限期未再失败 → 删除
			if entry.lockoutUntil.Before(cutoff) {
				s.loginFailures.Delete(key)
			}
			return true
		})
	}
}

func (s *Server) SetupRoutes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	r.Use(s.authMiddleware())

	r.GET("/healthz", s.handleHealth)
	r.GET("/login", s.handleLoginPage)
	r.POST("/login", s.handleLoginSubmit)
	r.POST("/logout", s.handleLogout)

	r.GET("/", s.handleIndex)

	api := r.Group("/api")
	{
		api.POST("/sites", s.handleRegisterSite)
		api.GET("/sites", s.handleListSites)
		api.DELETE("/sites/:pathid", s.handleDeleteSite)
		api.POST("/sites/:pathid/refresh", s.handleRefreshSite)
		api.GET("/sites/:pathid/tree", s.handleGetTree)
		api.GET("/sites/:pathid/branches", s.handleListBranches)
		api.PATCH("/sites/:pathid/hidden", s.handleSetHidden)
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

// loginCookieName 是登录会话 cookie 的名称。
const loginCookieName = "gitweb_auth"

// repoAuthCookieName 是 viewer 私有仓库凭据 cookie 的前缀。
// 前端在保存凭据时同步写到 localStorage + cookie，浏览器导航（地址栏直接访问
// /<pathid>/<file>）会自动带 cookie，后端从中恢复 Authorization 头。
const repoAuthCookieName = "gitweb_repo_auth"

// isSecureRequest 根据请求判断是否应下发 Secure cookie。
// 优先看 X-Forwarded-Proto，其次看 c.Request.TLS。
func isSecureRequest(c *gin.Context) bool {
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" {
		return proto == "https"
	}
	return c.Request.TLS != nil
}

// authMiddleware 校验登录会话；未通过则重定向到 /login。
// 仅在 s.password != "" 时生效。
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.password == "" || isPublicPath(c.Request.URL.Path) || s.checkLogin(c) {
			c.Next()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		redirect := c.Request.URL.RequestURI()
		c.Redirect(http.StatusFound, "/login?redirect="+base64.RawURLEncoding.EncodeToString([]byte(redirect)))
		c.Abort()
	}
}

// checkLogin 校验会话 cookie 是否有效（存在于 sessions map 且未过期）。
func (s *Server) checkLogin(c *gin.Context) bool {
	if s.password == "" {
		return true
	}
	token, err := c.Cookie(loginCookieName)
	if err != nil || token == "" {
		return false
	}
	v, ok := s.sessions.Load(token)
	if !ok {
		return false
	}
	entry := v.(sessionEntry)
	if time.Now().After(entry.expiresAt) {
		s.sessions.Delete(token)
		return false
	}
	return true
}

// safeRedirect 校验 redirect 目标是否为本地路径，防止开放重定向。
// 仅允许以单个 / 开头的相对路径，拒绝 //host、/\\host、http:// 等外部跳转。
func safeRedirect(redirect string) string {
	if redirect == "" {
		return "/"
	}
	if !strings.HasPrefix(redirect, "/") {
		return "/"
	}
	if strings.HasPrefix(redirect, "//") || strings.HasPrefix(redirect, "/\\") {
		return "/"
	}
	return redirect
}

// generateSessionToken 生成 32 字节随机 base64url token。
func generateSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
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
			redirect = safeRedirect(string(raw))
		} else {
			redirect = "/"
		}
	}
	c.HTML(http.StatusOK, "login.html", gin.H{
		"BaseURL":  s.baseURL,
		"Redirect": redirect,
		"Error":    false,
	})
}

// handleLoginSubmit 校验密码并签发会话 cookie，成功则重定向到 redirect 或 /。
func (s *Server) handleLoginSubmit(c *gin.Context) {
	if s.password == "" {
		c.Redirect(http.StatusFound, "/")
		return
	}
	ip := c.ClientIP()
	if locked, until := s.isLoginLocked(ip); locked {
		c.HTML(http.StatusTooManyRequests, "login.html", gin.H{
			"BaseURL":  s.baseURL,
			"Redirect": safeRedirect(c.PostForm("redirect")),
			"Error":    true,
			"Locked":   true,
			"LockSec":  int(time.Until(until).Seconds()) + 1,
		})
		return
	}
	pw := c.PostForm("password")
	redirect := safeRedirect(c.PostForm("redirect"))
	if subtle.ConstantTimeCompare([]byte(pw), []byte(s.password)) == 1 {
		token, err := generateSessionToken()
		if err != nil {
			c.HTML(http.StatusInternalServerError, "login.html", gin.H{
				"BaseURL":  s.baseURL,
				"Redirect": redirect,
				"Error":    true,
			})
			return
		}
		s.sessions.Store(token, sessionEntry{
			expiresAt: time.Now().Add(s.sessionExpiry),
		})
		s.resetLoginFailures(ip)
		c.SetSameSite(http.SameSiteLaxMode)
		maxAge := int(s.sessionExpiry.Seconds())
		c.SetCookie(loginCookieName, token, maxAge, "/", "", isSecureRequest(c), true)
		c.Redirect(http.StatusFound, redirect)
		return
	}
	s.recordLoginFailure(ip)
	c.HTML(http.StatusUnauthorized, "login.html", gin.H{
		"BaseURL":  s.baseURL,
		"Redirect": redirect,
		"Error":    true,
	})
}

// handleLogout 清除会话后回到登录页（若已设 password；否则直接回首页）。
func (s *Server) handleLogout(c *gin.Context) {
	if s.password != "" {
		if token, err := c.Cookie(loginCookieName); err == nil && token != "" {
			s.sessions.Delete(token)
		}
		c.SetCookie(loginCookieName, "", -1, "/", "", isSecureRequest(c), true)
		c.Redirect(http.StatusFound, "/login")
	} else {
		c.Redirect(http.StatusFound, "/")
	}
}

// isLoginLocked 检查 IP 是否在登录锁定窗口内。
func (s *Server) isLoginLocked(ip string) (bool, time.Time) {
	v, ok := s.loginFailures.Load(ip)
	if !ok {
		return false, time.Time{}
	}
	entry := v.(*loginFailEntry)
	if time.Now().Before(entry.lockoutUntil) {
		return true, entry.lockoutUntil
	}
	return false, time.Time{}
}

// recordLoginFailure 记录一次登录失败，达到阈值则进入锁定。
func (s *Server) recordLoginFailure(ip string) {
	v, _ := s.loginFailures.LoadOrStore(ip, &loginFailEntry{})
	entry := v.(*loginFailEntry)
	if time.Now().After(entry.lockoutUntil) {
		entry.count = 0
	}
	entry.count++
	if entry.count >= s.loginMaxFails {
		entry.lockoutUntil = time.Now().Add(s.loginRateLimit)
	}
}

// resetLoginFailure 登录成功后清除该 IP 的失败计数。
func (s *Server) resetLoginFailures(ip string) {
	s.loginFailures.Delete(ip)
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

func (s *Server) handleSetHidden(c *gin.Context) {
	pathID := c.Param("pathid")
	var req struct {
		Hidden bool `json:"hidden"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := s.registry.SetHidden(pathID, req.Hidden); err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "site not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hidden": req.Hidden})
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
			"Hidden":     site.Hidden,
		})
		return
	}

	// 凭据从请求头透传（运行时输入），不与缓存关联、用完即弃。
	auth := authFromHeader(c, site.Auth)

	// ?ref= 临时切换分支（不写入 state）。缓存键带上 ref，避免不同分支同路径串缓存。
	ref := effectiveRef(c, site.Ref)

	// ?raw=1 强制以 text/plain 返回原始源码（用于 HTML/MD/Text 的"查看源码"）。
	raw := c.Query("raw") == "1"
	// ?embed=1 返回独立渲染页面（用于分享链接，只渲染文件内容，无 viewer chrome）。
	embed := c.Query("embed") == "1"

	// 缓存键不含凭据（凭据可能因用户而异）。缓存的是文件字节/片段，
	// 公开仓库内容对所有用户一致；私有仓库的内容也按 pathid:filepath 共享缓存——
	// 若需更严格隔离，可把缓存限定为公开仓库（此处按 YAGNI 暂不区分）。
	// raw 模式缓存的是原始字节，和非 raw 的渲染片段不同，需单独缓存。
	cacheKey := fmt.Sprintf("%s:%s:%s", pathID, ref, filepath)
	if raw {
		cacheKey += ":raw"
	}
	if cached, ok := s.cache.Get(cacheKey); ok {
		if raw {
			c.Data(http.StatusOK, "text/plain; charset=utf-8", cached)
		} else {
			s.serveContent(c, filepath, cached)
		}
		return
	}

	content, err := s.provider.Fetch(context.Background(), site.Provider, site.GitURL, ref, filepath, auth)
	if err != nil {
		s.serveFetchError(c, err)
		return
	}

	// raw 模式：对文本类文件（HTML/MD/Text/代码）以 text/plain 返回原始字节。
	// 图片/二进制不受 raw 影响（源码无意义），走正常流程。
	if raw {
		switch s.renderer.Kind(filepath) {
		case render.KindHTML, render.KindMarkdown, render.KindText:
			s.cache.Set(cacheKey, content)
			c.Data(http.StatusOK, "text/plain; charset=utf-8", content)
			return
		}
	}

	// embed 模式：返回独立渲染页面（分享链接用），只渲染文件内容，无 viewer chrome。
	// HTML 文件本身已是完整页面，直接透传；其余类型渲染后套 embed 模板。
	if embed {
		kind := s.renderer.Kind(filepath)
		filename := path.Base(filepath)
		repoName := repoNameFromURL(site.GitURL)
		switch kind {
		case render.KindHTML:
			s.cache.Set(cacheKey, content)
			c.Data(http.StatusOK, "text/html; charset=utf-8", content)
			return
		case render.KindImage:
			s.cache.Set(cacheKey, content)
			imgURL := "/" + pathID + "/" + filepath + "?ref=" + ref
			c.HTML(http.StatusOK, "embed.html", gin.H{
				"PathID":   pathID,
				"RepoName": repoName,
				"Ref":      ref,
				"Filename": filename,
				"Content":  `<div class="image-container"><img src="` + imgURL + `" alt="` + filename + `"></div>`,
			})
			return
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
			c.HTML(http.StatusOK, "embed.html", gin.H{
				"PathID":   pathID,
				"RepoName": repoName,
				"Ref":      ref,
				"Filename": filename,
				"Content":  rendered,
			})
			return
		default:
			// 二进制/不支持预览的文件：直接透传（触发下载）
			s.cache.Set(cacheKey, content)
			s.serveContent(c, filepath, content)
			return
		}
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
	if errors.Is(err, provider.ErrRateLimited) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "rate limit exceeded for this host, please try again later"})
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
		return
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

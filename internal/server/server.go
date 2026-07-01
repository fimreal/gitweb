package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/limingrui/gitweb/internal/cache"
	"github.com/limingrui/gitweb/internal/provider"
	"github.com/limingrui/gitweb/internal/registry"
	"github.com/limingrui/gitweb/internal/render"
)

// viewerCSP 是 viewer 页面与文件内容响应下发的严格 CSP：
// 限制脚本来源为同源，防止用户仓库 HTML 的脚本逃逸；图片允许任意源与 data/blob；
// frame-src 允许同源 iframe（承载用户 HTML）。
const viewerCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob: *; connect-src 'self'; frame-src 'self'"

type Server struct {
	registry    *registry.Registry
	provider    *provider.Manager
	cache       *cache.Cache
	renderer    *render.Renderer
	baseURL     string
	maxFileSize int64
}

func New(reg *registry.Registry, prov *provider.Manager, c *cache.Cache, rend *render.Renderer, baseURL string, maxFileSize int64) *Server {
	return &Server{
		registry:    reg,
		provider:    prov,
		cache:       c,
		renderer:    rend,
		baseURL:     baseURL,
		maxFileSize: maxFileSize,
	}
}

func (s *Server) SetupRoutes() *gin.Engine {
	r := gin.Default()

	r.GET("/", s.handleIndex)
	r.GET("/healthz", s.handleHealth)

	api := r.Group("/api")
	{
		api.POST("/sites", s.handleRegisterSite)
		api.GET("/sites", s.handleListSites)
		api.DELETE("/sites/:pathid", s.handleDeleteSite)
		api.POST("/sites/:pathid/refresh", s.handleRefreshSite)
		api.GET("/sites/:pathid/tree", s.handleGetTree)
	}

	// /:pathid/*filepath 同时承载 viewer 页面（空 filepath）与文件内容（有 filepath）。
	// Gin 不允许 catch-all 与独立 /:pathid/ 路由共存，故合并到一条路由，空路径走 viewer。
	r.GET("/:pathid/*filepath", s.handleSiteFile)

	return r
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported git provider"})
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

	site, err := s.registry.Register(gitURL, req.PathID, ref, providerType, nil)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"pathid": site.PathID,
		"url":    fmt.Sprintf("%s/%s/", s.baseURL, site.PathID),
	})
}

func (s *Server) handleListSites(c *gin.Context) {
	sites := s.registry.List()
	result := make([]gin.H, len(sites))
	for i, site := range sites {
		result[i] = gin.H{
			"pathid":     site.PathID,
			"git_url":    site.GitURL,
			"ref":        site.Ref,
			"provider":   site.Provider,
			"created_at": site.CreatedAt,
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

	tree, err := s.provider.FetchTree(context.Background(), site.Provider, site.GitURL, site.Ref, auth)
	if err != nil {
		if errors.Is(err, provider.ErrAuthRequired) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch tree: " + err.Error()})
		return
	}

	filtered := provider.FilterRenderableFiles(tree, s.renderer.IsRenderable)
	c.JSON(http.StatusOK, gin.H{"files": filtered})
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

	// 空 filepath：返回 viewer 页面骨架，下发严格 CSP。
	if filepath == "" {
		c.Header("Content-Security-Policy", viewerCSP)
		c.HTML(http.StatusOK, "viewer.html", gin.H{
			"PathID":   pathID,
			"GitURL":   site.GitURL,
			"Ref":      site.Ref,
			"RepoName": repoNameFromURL(site.GitURL),
		})
		return
	}

	// 凭据从请求头透传（运行时输入），不与缓存关联、用完即弃。
	auth := authFromHeader(c, site.Auth)

	// 缓存键不含凭据（凭据可能因用户而异）。缓存的是文件字节/片段，
	// 公开仓库内容对所有用户一致；私有仓库的内容也按 pathid:filepath 共享缓存——
	// 若需更严格隔离，可把缓存限定为公开仓库（此处按 YAGNI 暂不区分）。
	cacheKey := fmt.Sprintf("%s:%s", pathID, filepath)
	if cached, ok := s.cache.Get(cacheKey); ok {
		s.serveContent(c, filepath, cached)
		return
	}

	content, err := s.provider.Fetch(context.Background(), site.Provider, site.GitURL, site.Ref, filepath, auth)
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

package server

import (
	"context"
	"errors"
	"net/url"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/limingrui/gitweb/internal/cache"
	"github.com/limingrui/gitweb/internal/provider"
	"github.com/limingrui/gitweb/internal/registry"
	"github.com/limingrui/gitweb/internal/render"
)

type Server struct {
	registry    *registry.Registry
	provider    *provider.Manager
	cache       *cache.Cache
	renderer    *render.Renderer
	adminToken  string
	baseURL     string
	maxFileSize int64
}

func New(reg *registry.Registry, prov *provider.Manager, c *cache.Cache, rend *render.Renderer, adminToken, baseURL string, maxFileSize int64) *Server {
	return &Server{
		registry:    reg,
		provider:    prov,
		cache:       c,
		renderer:    rend,
		adminToken:  adminToken,
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
		api.POST("/sites", s.authMiddleware(), s.handleRegisterSite)
		api.GET("/sites", s.authMiddleware(), s.handleListSites)
		api.DELETE("/sites/:pathid", s.authMiddleware(), s.handleDeleteSite)
		api.POST("/sites/:pathid/refresh", s.authMiddleware(), s.handleRefreshSite)
		api.GET("/sites/:pathid/tree", s.handleGetTree)
	}

	r.GET("/:pathid/*filepath", s.handleSiteFile)

	return r
}

func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.adminToken == "" {
			c.Next()
			return
		}

		auth := c.GetHeader("Authorization")
		if auth != "Bearer "+s.adminToken && auth != s.adminToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		c.Next()
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
		Auth   *struct {
			Type     string `json:"type"`
			Token    string `json:"token"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auth"`
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

	// 对于 GitHub，标准化浏览器 URL
	if providerType == "github" {
		if normalizedURL, extractedRef, err := normalizeGitHubURL(req.GitURL); err == nil {
			gitURL = normalizedURL
			// 如果用户没有指定 ref，使用从 URL 提取的 ref
			if ref == "" {
				ref = extractedRef
			}
		}
	}

	var auth *registry.Auth
	if req.Auth != nil {
		auth = &registry.Auth{
			Type:     req.Auth.Type,
			Token:    req.Auth.Token,
			Username: req.Auth.Username,
			Password: req.Auth.Password,
		}
	}

	site, err := s.registry.Register(gitURL, req.PathID, ref, providerType, auth)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"pathid": site.PathID,
		"url":    fmt.Sprintf("%s/%s/", s.baseURL, site.PathID),
	})
}

// normalizeGitHubURL 从浏览器 URL 中提取标准 Git URL 和 ref
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
		ref = "main"
	}
	
	return gitURL, ref, nil
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

	var auth *provider.Auth
	if site.Auth != nil {
		auth = &provider.Auth{
			Type:     site.Auth.Type,
			Token:    site.Auth.Token,
			Username: site.Auth.Username,
			Password: site.Auth.Password,
		}
	}

	tree, err := s.provider.FetchTree(context.Background(), site.Provider, site.GitURL, site.Ref, auth)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to fetch tree: " + err.Error()})
		return
	}

	filtered := provider.FilterRenderableFiles(tree)
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

	// 如果没有指定文件，返回查看器页面
	if filepath == "" {
		c.HTML(http.StatusOK, "viewer.html", gin.H{
			"PathID": pathID,
			"GitURL": site.GitURL,
			"Ref":    site.Ref,
		})
		return
	}

	page := 1
	if pageStr := c.Query("page"); pageStr != "" {
		if p, err := strconv.Atoi(pageStr); err == nil && p > 0 {
			page = p
		}
	}

	cacheKey := fmt.Sprintf("%s:%s:%d", pathID, filepath, page)
	if cached, ok := s.cache.Get(cacheKey); ok {
		c.Data(http.StatusOK, "text/html; charset=utf-8", cached)
		return
	}

	var auth *provider.Auth
	if site.Auth != nil {
		auth = &provider.Auth{
			Type:     site.Auth.Type,
			Token:    site.Auth.Token,
			Username: site.Auth.Username,
			Password: site.Auth.Password,
		}
	}

	content, err := s.provider.Fetch(context.Background(), site.Provider, site.GitURL, site.Ref, filepath, auth)
	if err != nil {
		if err == provider.ErrNotFound {
			c.HTML(http.StatusNotFound, "error.html", gin.H{
				"Code":    404,
				"Message": "File not found",
			})
		} else {
			c.HTML(http.StatusBadGateway, "error.html", gin.H{
				"Code":    502,
				"Message": "Failed to fetch file: " + err.Error(),
			})
		}
		return
	}

	if int64(len(content)) > s.maxFileSize {
		c.HTML(http.StatusRequestEntityTooLarge, "error.html", gin.H{
			"Code":    413,
			"Message": "File too large",
		})
		return
	}

	if !s.renderer.ShouldRender(filepath) {
		c.Data(http.StatusOK, "application/octet-stream", content)
		return
	}

	// HTML 文件直接返回原始内容（用于 iframe）
	lastDot := strings.LastIndex(filepath, ".")
	ext := ""
	if lastDot >= 0 {
		ext = strings.ToLower(filepath[lastDot:])
	}
	if ext == ".html" || ext == ".htm" {
		s.cache.Set(cacheKey, content)
		c.Data(http.StatusOK, "text/html; charset=utf-8", content)
		return
	}

	rendered, totalPages, err := s.renderer.Render(filepath, content, page)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "error.html", gin.H{
			"Code":    500,
			"Message": "Render failed: " + err.Error(),
		})
		return
	}

	html := s.wrapContent(rendered, filepath, page, totalPages, pathID)
	s.cache.Set(cacheKey, []byte(html))

	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(html))
}

func (s *Server) wrapContent(content, filepath string, page, totalPages int, pathID string) string {
	pagination := ""
	if totalPages > 1 {
		pagination = fmt.Sprintf(`<div class="pagination">`)
		if page > 1 {
			pagination += fmt.Sprintf(`<a href="?page=%d">Previous</a>`, page-1)
		}
		pagination += fmt.Sprintf(` Page %d / %d `, page, totalPages)
		if page < totalPages {
			pagination += fmt.Sprintf(`<a href="?page=%d">Next</a>`, page+1)
		}
		pagination += `</div>`
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s</title>
    <link rel="stylesheet" href="/static/css/style.css">
</head>
<body>
    <div class="container">
        <header>
            <h1>%s</h1>
            <div class="controls">
                <button id="theme-toggle">Toggle Theme</button>
                <button id="lang-toggle">EN/中文</button>
            </div>
        </header>
        <main>
            %s
        </main>
        %s
    </div>
    <script src="/static/js/app.js"></script>
</body>
</html>`, filepath, filepath, content, pagination)
}

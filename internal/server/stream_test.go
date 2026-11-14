package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/fimreal/gitweb/internal/cache"
	"github.com/fimreal/gitweb/internal/provider"
	"github.com/fimreal/gitweb/internal/registry"
	"github.com/fimreal/gitweb/internal/render"
	"github.com/fimreal/gitweb/web"
)

// streamFixture 是一个可打真实路由的 Server，上游换成一个假装成 Gitea 的 httptest 源站。
// hits 计数用来判定「这次请求有没有真的走缓存」——走缓存时源站必须一次都不被碰。
type streamFixture struct {
	r    *gin.Engine
	hits *atomic.Int64
}

func (f *streamFixture) fetch(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

// newStreamFixture 复刻 main.go 的装配，但把 provider 的 allow 列表放开到 127.0.0.1
// 并把 pathid=site 指向模拟源站，绕开注册接口直接验文件路由。
func newStreamFixture(t *testing.T, files map[string]string) *streamFixture {
	t.Helper()
	return newStreamFixtureWithRenderer(t, files, render.New())
}

// newStreamFixtureWithRenderer 允许注入带配置的 Renderer（如自定义 extra_text_exts）。
func newStreamFixtureWithRenderer(t *testing.T, files map[string]string, renderer *render.Renderer) *streamFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard

	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(up.Close)

	reg := registry.New()
	prov := provider.NewManager(5*time.Second, "", "", []string{"127.0.0.1", "localhost"}, nil, 1<<20, 100)
	srv := New(reg, prov, cache.New(time.Minute, 16), renderer, testBaseURL, "")
	if _, err := reg.Register(up.URL+"/o/repo", "site", "main", "gitea", nil, false); err != nil {
		t.Fatalf("register site: %v", err)
	}
	r := srv.SetupRoutes()
	tpl, err := web.LoadTemplates()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}
	r.SetHTMLTemplate(tpl)

	return &streamFixture{r: r, hits: &hits}
}

// TestStreamedPassthroughCarriesLengthAndHitsCache 验透传类文件的流式契约：响应必须带
// Content-Length（前端进度条的分母全靠它）、Content-Type 与缓冲版一致、字节原样不出错，
// 且二次请求不再碰源站。
func TestStreamedPassthroughCarriesLengthAndHitsCache(t *testing.T) {
	body := strings.Repeat("abcdefghij", 64) // 640 字节，源站会自动给出 Content-Length
	f := newStreamFixture(t, map[string]string{"blob.dat": body})

	first := f.fetch(t, "/site/blob.dat")
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", first.Code)
	}
	if got := first.Body.String(); got != body {
		t.Errorf("body mismatch: got %d bytes, want %d", len(got), len(body))
	}
	if got := first.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %q, want %q", got, strconv.Itoa(len(body)))
	}
	if got := first.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}

	second := f.fetch(t, "/site/blob.dat")
	if second.Body.String() != body {
		t.Errorf("cached body mismatch: got %d bytes, want %d", second.Body.Len(), len(body))
	}
	if n := f.hits.Load(); n != 1 {
		t.Errorf("upstream hits = %d, want 1 (second request should come from cache)", n)
	}
}

// TestScriptExtensionsServeAsText 锁定 Windows 脚本扩展名的分发契约：
// 普通浏览页按文本渲染成 <pre><code>，?raw=1 以 text/plain 返回原始源码
// （否则 `irm <url>?raw=1 | iex` 拿到的是字节流而不是脚本）。
func TestScriptExtensionsServeAsText(t *testing.T) {
	ps1 := "Write-Host 'hi'\n$env:FOO = 'bar'\n"
	cmd := "@echo off\r\necho hi\r\n"
	f := newStreamFixture(t, map[string]string{"bootstrap.ps1": ps1, "install.cmd": cmd})

	raw := f.fetch(t, "/site/bootstrap.ps1?raw=1")
	if got := raw.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("ps1 raw Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := raw.Body.String(); got != ps1 {
		t.Errorf("ps1 raw body = %q, want %q", got, ps1)
	}

	view := f.fetch(t, "/site/bootstrap.ps1")
	if got := view.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("ps1 view Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if got := view.Body.String(); !strings.HasPrefix(got, "<pre><code>") || !strings.Contains(got, "Write-Host") {
		t.Errorf("ps1 view body = %q, want plain-text rendering", got)
	}

	cmdRaw := f.fetch(t, "/site/install.cmd?raw=1")
	if got := cmdRaw.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("cmd raw Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := cmdRaw.Body.String(); got != cmd {
		t.Errorf("cmd raw body = %q, want %q", got, cmd)
	}
}

// TestConfiguredExtraTextExts 验 render.extra_text_exts 端到端生效：同一扩展名
// 不开配置时仍是二进制，开启后 raw 与渲染页都走文本路径。
func TestConfiguredExtraTextExts(t *testing.T) {
	src := "hello from a custom extension\n"
	files := map[string]string{"notes.abc": src}

	off := newStreamFixture(t, files)
	if got := off.fetch(t, "/site/notes.abc?raw=1").Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("without config: Content-Type = %q, want application/octet-stream", got)
	}

	on := newStreamFixtureWithRenderer(t, files, render.New(".abc"))
	raw := on.fetch(t, "/site/notes.abc?raw=1")
	if got := raw.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("with config: raw Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := raw.Body.String(); got != src {
		t.Errorf("with config: raw body = %q, want %q", got, src)
	}

	view := on.fetch(t, "/site/notes.abc")
	if got := view.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("with config: view Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if got := view.Body.String(); !strings.HasPrefix(got, "<pre><code>") || !strings.Contains(got, "custom extension") {
		t.Errorf("with config: view body = %q, want plain-text rendering", got)
	}
}

// TestStreamedRawSourceIsPlainText 验「查看源码」走的是流式透传：拿到的是原始字节而非
// 渲染片段，且类型为 text/plain。
func TestStreamedRawSourceIsPlainText(t *testing.T) {
	src := "package main\n\nfunc main() {}\n"
	f := newStreamFixture(t, map[string]string{"main.go": src})

	w := f.fetch(t, "/site/main.go?raw=1")
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := w.Body.String(); got != src {
		t.Errorf("body = %q, want raw source %q", got, src)
	}
	if got := w.Header().Get("Content-Length"); got != strconv.Itoa(len(src)) {
		t.Errorf("Content-Length = %q, want %q", got, strconv.Itoa(len(src)))
	}
}

// TestRenderedTypesStayBuffered 锁住流式的边界：md 与 embed 必须整体渲染，不能因为
// 「顺手也流一下」而把渲染这一步跳过。同时验 raw 与非 raw 的缓存键互不污染。
func TestRenderedTypesStayBuffered(t *testing.T) {
	md := "# Title\n\nsome *text*\n"
	f := newStreamFixture(t, map[string]string{"README.md": md})

	rendered := f.fetch(t, "/site/README.md")
	if got := rendered.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
	}
	if got := rendered.Body.String(); !strings.Contains(got, "<h1") || strings.Contains(got, "*text*") {
		t.Errorf("body = %q, want rendered markdown", got)
	}

	// 同一文件的 raw 视图必须落在另一个缓存键上。
	rawView := f.fetch(t, "/site/README.md?raw=1")
	if got := rawView.Body.String(); got != md {
		t.Errorf("raw body = %q, want original markdown %q", got, md)
	}

	again := f.fetch(t, "/site/README.md")
	if got := again.Body.String(); !strings.Contains(got, "<h1") {
		t.Errorf("second render came back as %q — raw bytes leaked into the view cache", got)
	}
	// 四次请求只回源两次：view 与 embed 共用一个缓存键（都存渲染片段），raw 单独占一个。
	if n := f.hits.Load(); n != 2 {
		t.Errorf("upstream hits = %d, want 2", n)
	}

	embedded := f.fetch(t, "/site/README.md?embed=1")
	if embedded.Code != http.StatusOK {
		t.Fatalf("embed status = %d, want 200", embedded.Code)
	}
	if got := embedded.Body.String(); !strings.Contains(got, "<h1") {
		t.Errorf("embed body = %q, want rendered markdown inside the embed page", got)
	}
}

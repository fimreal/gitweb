package server

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fimreal/gitweb/internal/cache"
	"github.com/fimreal/gitweb/internal/provider"
	"github.com/fimreal/gitweb/internal/registry"
	"github.com/fimreal/gitweb/internal/render"
	"github.com/fimreal/gitweb/web"
	"github.com/gin-gonic/gin"
)

const testBaseURL = "http://localhost:8080"

// newTestServer 复刻 cmd/gitweb/main.go 的装配：Server + web.LoadTemplates() + 静态资源。
// 模板必须显式挂上，否则 c.HTML 渲染不出页面（main.go 里也是这么做的）；
// 静态资源也必须挂上，否则页面里引用的 css/js 一律 404，资源存在性断言形同虚设。
// 可选传入访问密码：/login 仅在设置了密码时渲染，否则 302 跳回首页。
func newTestServer(t *testing.T, password ...string) (*gin.Engine, *registry.Registry) {
	t.Helper()

	pw := ""
	if len(password) > 0 {
		pw = password[0]
	}

	prov := provider.NewManager(5*time.Second, "", "", nil, nil, 1<<20, 100, "")
	return newTestServerWithProvider(t, prov, pw)
}

// newTestServerWithProvider 用调用方提供的 provider.Manager 装配测试路由，
// 便于需要自定义白名单（如放行 httptest 的 127.0.0.1）的用例复用其余装配。
func newTestServerWithProvider(t *testing.T, prov *provider.Manager, password string) (*gin.Engine, *registry.Registry) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard

	reg := registry.New()
	srv := New(reg, prov, cache.New(time.Minute, 16), render.New(), testBaseURL, password)

	r := srv.SetupRoutes()
	tpl, err := web.LoadTemplates()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}
	r.SetHTMLTemplate(tpl)

	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		t.Fatalf("fs.Sub static: %v", err)
	}
	r.StaticFS("/static", http.FS(staticFS))

	return r, reg
}

// postSite 调用 POST /api/sites，返回状态码与解析后的响应体。
// 成功新建站点是 201；requestBaseURL 会读 req.Host，httptest 默认值是 example.com，
// 这里显式对齐 testBaseURL 以免断言漂移。
func postSite(t *testing.T, r *gin.Engine, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sites", strings.NewReader(body))
	req.Host = "localhost:8080"
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := map[string]any{}
	if data, err := io.ReadAll(w.Result().Body); err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return w.Code, out
}

func TestIsValidAuthorityHost(t *testing.T) {
	good := []string{
		"localhost", "localhost:8080", "127.0.0.1:8099",
		"git.example.com", "git.example.com:443", "sub.domain.co.uk",
		"[::1]", "[::1]:8080", "[2001:db8::1]:9000",
	}
	bad := []string{
		"",
		`x"><img src=x onerror=alert(1)>`,
		"attacker.com/evil",
		"attacker.com/evil?next=1",
		"evil.com#frag",
		"user:pass@host",
		"a b", "a\tb", "a\nb",
		"-leading-dash.com", "trailing-dash-.com",
		"host:port", "host:80x", "host:99999x",
		"[::1", "::1]", "[not-an-ip]:80", "[::1]8080",
		strings.Repeat("a", 254),
	}
	for _, h := range good {
		if !isValidAuthorityHost(h) {
			t.Errorf("isValidAuthorityHost(%q) = false, want true", h)
		}
	}
	for _, h := range bad {
		if isValidAuthorityHost(h) {
			t.Errorf("isValidAuthorityHost(%q) = true, want false", h)
		}
	}
}

func TestIsHTTPSGitURL(t *testing.T) {
	good := []string{
		"https://github.com/fimreal/gitweb.git",
		"https://github.com/fimreal/gitweb",
		"http://git.example.com/team/repo.git",
		"http://localhost:3000/user/repo.git",
		"https://gitlab.com/group/sub/project.git",
	}
	bad := []string{
		"",
		"github.com/fimreal/gitweb.git",      // 无 scheme
		"git@github.com:fimreal/gitweb.git",  // ssh 传输协议
		"javascript:alert(1)",                // 伪协议
		"data:text/html;base64,PHNjcmlwdD4=", // data URI
		"https://github.com@evil.com/repo",   // userinfo 混淆：IdentifyProvider 会命中 github.com
		"file:///etc/passwd",
		"ssh://git@host/repo",
		"https://",
		"https://a b.com/repo",
		"http://[::1/notclosed/repo",
	}
	// 注：host 是否可信（如 https://evil.com/x）不在本层断言——isHTTPSGitURL 只管
	// URL 形态，host 白名单由 provider 的 SSRF 守卫负责。
	for _, u := range good {
		if !isHTTPSGitURL(u) {
			t.Errorf("isHTTPSGitURL(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if isHTTPSGitURL(u) {
			t.Errorf("isHTTPSGitURL(%q) = true, want false", u)
		}
	}
}

func TestEffectiveRefRejectsInjectionAndTraversal(t *testing.T) {
	cases := []struct {
		query string
		want  string
	}{
		{"", "main"},
		{"?ref=develop", "develop"},
		{"?ref=refs%2Fheads%2Fmain", "refs/heads/main"},
		// 路径穿越：借同一 allow_hosts 白名单 host 读取其他仓库
		{"?ref=../../other-owner/other-repo/main/x", "main"},
		{"?ref=..%2F..%2Fother%2Frepo%2Fmain%2Fx", "main"},
		// 查询串与片段注入：?ref=main&recursive=false 会改写上游请求
		{"?ref=main%26recursive%3Dfalse", "main"},
		{"?ref=main#frag", "main"},
		// HTML 注入
		{`?ref=%3Cimg%20src%3Dx%20onerror%3Dalert(1)%3E`, "main"},
		{"?ref=%20%20", "main"}, // 纯空白经 TrimSpace 后为空，回落默认
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/gitweb/"+tc.query, nil)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = req
		c.Params = gin.Params{{Key: "pathid", Value: "gitweb"}}
		if got := effectiveRef(c, "main"); got != tc.want {
			t.Errorf("effectiveRef(query %q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

// TestRegisterSiteRejectsMaliciousRef 锁住存储型 XSS 的服务端入口：
// 恶意 ref 必须 400，且绝不能出现在 GET /api/sites 的回显里。
func TestRegisterSiteRejectsMaliciousRef(t *testing.T) {
	r, reg := newTestServer(t)

	payloads := []string{
		`<img src=x onerror=document.title="XSS-PWNED">`,
		"</script><script>alert(1)</script>",
		"../../other-owner/other-repo/main/x",
	}
	for _, p := range payloads {
		code, _ := postSite(t, r, `{"git_url":"https://github.com/o/repo.git","ref":`+jsonString(p)+`}`, nil)
		if code != http.StatusBadRequest {
			t.Errorf("POST /api/sites with ref %q: status %d, want 400", p, code)
		}
	}
	if n := len(reg.ListPublic()); n != 0 {
		t.Fatalf("%d malicious sites were persisted, want 0", n)
	}

	// 正常 ref 必须仍然放行，避免校验过紧把功能打死。
	legit := []string{"main", "feature/x", "v1.2.3", "3e2f1a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f"}
	for i, p := range legit {
		pathid := "ok" + string(rune('a'+i))
		code, body := postSite(t, r,
			`{"git_url":"https://github.com/o/repo.git","pathid":`+jsonString(pathid)+`,"ref":`+jsonString(p)+`}`, nil)
		if code != http.StatusCreated {
			t.Errorf("POST /api/sites with legit ref %q: status %d body %v, want 201", p, code, body)
		}
	}
}

func TestRegisterSiteRejectsMalformedGitURL(t *testing.T) {
	r, _ := newTestServer(t)

	bad := []string{
		"git@github.com:fimreal/gitweb.git",
		"javascript:alert(document.cookie)",
		"https://github.com@evil.com/repo",
		"not-a-url",
	}
	for _, u := range bad {
		code, _ := postSite(t, r, `{"git_url":`+jsonString(u)+`,"ref":"main"}`, nil)
		if code != http.StatusBadRequest {
			t.Errorf("POST /api/sites with git_url %q: status %d, want 400", u, code)
		}
	}
}

// TestRegisterSiteURLIgnoresSpoofedForwardedHeaders 覆盖 requestBaseURL 的注入面：
// 伪造的 X-Forwarded-Host / -Proto 不得进入响应 url 字段（前端会写进 innerHTML 与 href）。
func TestRegisterSiteURLIgnoresSpoofedForwardedHeaders(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"html in forwarded host", map[string]string{"X-Forwarded-Host": `x"><img src=x onerror=alert(1)>`}},
		{"javascript scheme", map[string]string{"X-Forwarded-Proto": "javascript"}},
		{"path off forwarded host", map[string]string{"X-Forwarded-Host": "evil.com/attack"}},
		{"userinfo confusion", map[string]string{"X-Forwarded-Host": "github.com@evil.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newTestServer(t)
			code, body := postSite(t, r,
				`{"git_url":"https://github.com/o/repo.git","pathid":"p1","ref":"main"}`, tc.headers)
			if code != http.StatusCreated {
				t.Fatalf("status %d, want 201 (body %v)", code, body)
			}
			got, _ := body["url"].(string)
			if got != testBaseURL+"/p1/" {
				t.Errorf("url = %q, want %q — spoofed header leaked into the response", got, testBaseURL+"/p1/")
			}
		})
	}
}

// jsonString 把任意字符串编码成 JSON 字面量，避免在测试里手工拼接转义。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestSiteFileCacheIsolatedByCredential 回归测试缓存键的凭据隔离边界：
// 私有仓库先被带凭据抓取并写入缓存后，匿名请求必须重新走上游鉴权并拿到 401，
// 而不是命中前一个用户留下的缓存条目读出内容。
func TestSiteFileCacheIsolatedByCredential(t *testing.T) {
	const (
		wantPath = "/owner/repo/raw/branch/main/private.txt"
		wantAuth = "token secret-token"
		content  = "private content"
	)

	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		// 私有仓库对无凭据/错误凭据统一回 404（不泄露仓库存在性），
		// provider 会把它翻译成 ErrAuthRequired，handler 再回 401。
		if r.URL.Path != wantPath || r.Header.Get("Authorization") != wantAuth {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, content)
	}))
	defer upstream.Close()

	// 放行 httptest 的环回地址，否则 SSRF 防护会在请求上游前就拒绝。
	prov := provider.NewManager(5*time.Second, "", "", []string{"127.0.0.1"}, nil, 1<<20, 100, "")
	r, reg := newTestServerWithProvider(t, prov, "")
	if _, err := reg.Register(upstream.URL+"/owner/repo", "priv", "main", "gitea", nil, false); err != nil {
		t.Fatalf("register site: %v", err)
	}

	get := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/priv/private.txt", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 1) 带凭据抓取成功，内容写入该凭据指纹对应的缓存条目。
	if w := get("secret-token"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), content) {
		t.Fatalf("credentialed fetch: status %d body %q, want 200 containing %q", w.Code, w.Body.String(), content)
	}

	// 2) 同一凭据再次抓取应命中缓存：指纹稳定，不再打上游。
	if w := get("secret-token"); w.Code != http.StatusOK {
		t.Fatalf("second credentialed fetch: status %d, want 200", w.Code)
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 1 {
		t.Fatalf("upstream hits after two credentialed fetches = %d, want 1 (cache not reused for same credential)", got)
	}

	// 3) 匿名抓取：必须因凭据隔离而未命中缓存，重新鉴权失败。
	w := get("")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous fetch: status %d body %q, want 401 — credential-scoped cache leaked", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), content) {
		t.Fatalf("anonymous fetch leaked private content: %q", w.Body.String())
	}
	// 假上游对无凭据统一 404，Gitea provider 会按 branch/tag/commit 三种 ref 形式
	// 各探测一次（QEST-74），故匿名这一次逻辑抓取共产生 3 次上游命中：1+3=4。
	if got := atomic.LoadInt32(&upstreamHits); got != 4 {
		t.Fatalf("upstream hits after anonymous fetch = %d, want 4 (anonymous request must not hit the credentialed cache entry)", got)
	}
}

// TestSiteFileCachePresetAuthFallback 覆盖站点预置凭据（site.Auth）的回退路径：
// 匿名请求经 authFromHeader 回退到预置凭据，指纹与显式携带的其他凭据不同，
// 因此既不能把预置凭据的内容漏给其他凭据，也不能每次匿名访问都打上游。
func TestSiteFileCachePresetAuthFallback(t *testing.T) {
	const (
		wantPath = "/owner/repo/raw/branch/main/private.txt"
		preset   = "preset-token"
		content  = "preset protected content"
	)

	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		if r.URL.Path != wantPath || r.Header.Get("Authorization") != "token "+preset {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, content)
	}))
	defer upstream.Close()

	prov := provider.NewManager(5*time.Second, "", "", []string{"127.0.0.1"}, nil, 1<<20, 100, "")
	r, reg := newTestServerWithProvider(t, prov, "")
	if _, err := reg.Register(upstream.URL+"/owner/repo", "priv", "main", "gitea",
		&registry.Auth{Type: "token", Token: preset}, false); err != nil {
		t.Fatalf("register site: %v", err)
	}

	get := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/priv/private.txt", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 1) 匿名请求回退到预置凭据后成功，并按该凭据的指纹入缓存。
	if w := get(""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), content) {
		t.Fatalf("anonymous fetch via preset auth: status %d body %q, want 200 containing %q", w.Code, w.Body.String(), content)
	}

	// 2) 再次匿名请求命中预置凭据指纹的缓存，不再打上游。
	if w := get(""); w.Code != http.StatusOK {
		t.Fatalf("second anonymous fetch: status %d, want 200", w.Code)
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 1 {
		t.Fatalf("upstream hits after two anonymous fetches = %d, want 1 (preset-auth cache not reused)", got)
	}

	// 3) 显式携带另一凭据：指纹不同，不得命中预置凭据缓存；上游以该凭据收到 404，
	//    provider 因「带凭据的 404」判定为真实不存在，handler 回 404。
	w := get("other-token")
	if w.Code != http.StatusNotFound {
		t.Fatalf("fetch with other credential: status %d body %q, want 404 — must not hit preset-auth cache", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), content) {
		t.Fatalf("other credential leaked preset-auth content: %q", w.Body.String())
	}
	// 同上：另一凭据被上游统一 404，三种 ref 形式各探测一次，1+3=4。
	if got := atomic.LoadInt32(&upstreamHits); got != 4 {
		t.Fatalf("upstream hits after other-credential fetch = %d, want 4 (must not share preset-auth cache entry)", got)
	}
}

// TestSiteCacheInvalidatedAcrossCredentialRefAndRawKeys 验证 DELETE / refresh 的
// pathid: 前缀失效在新键格式下仍覆盖所有变体：不同凭据指纹、不同 ref、raw 与非 raw。
func TestSiteCacheInvalidatedAcrossCredentialRefAndRawKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "content for "+r.URL.Path)
	}))
	defer upstream.Close()

	prov := provider.NewManager(5*time.Second, "", "", []string{"127.0.0.1"}, nil, 1<<20, 100, "")
	reg := registry.New()
	c := cache.New(time.Minute, 64)
	srv := New(reg, prov, c, render.New(), testBaseURL, "")
	r := srv.SetupRoutes()
	if _, err := reg.Register(upstream.URL+"/owner/repo", "priv", "main", "gitea", nil, false); err != nil {
		t.Fatalf("register site: %v", err)
	}

	fetch := func(auth, target string) {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s (auth %q): status %d, want 200", target, auth, w.Code)
		}
	}
	// 5 个互不相同的缓存键：tok-a/tok-b/anon × 同文件，tok-a 的 raw 变体，tok-a 的另一 ref。
	refill := func() {
		fetch("tok-a", "/priv/a.txt")
		fetch("tok-b", "/priv/a.txt")
		fetch("", "/priv/a.txt")
		fetch("tok-a", "/priv/a.txt?raw=1")
		fetch("tok-a", "/priv/a.txt?ref=dev")
	}

	refill()
	if got := c.Len(); got != 5 {
		t.Fatalf("cache entries after refill = %d, want 5", got)
	}

	// refresh：只失效缓存、保留站点。
	refreshReq := httptest.NewRequest(http.MethodPost, "/api/sites/priv/refresh", nil)
	refreshW := httptest.NewRecorder()
	r.ServeHTTP(refreshW, refreshReq)
	if refreshW.Code != http.StatusOK {
		t.Fatalf("refresh: status %d, want 200", refreshW.Code)
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("cache entries after refresh = %d, want 0 (all credential/ref/raw variants must be invalidated)", got)
	}

	// DELETE：同样必须清掉该 pathid 下全部变体。
	refill()
	if got := c.Len(); got != 5 {
		t.Fatalf("cache entries after second refill = %d, want 5", got)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/sites/priv", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: status %d, want 200", w.Code)
	}
	if got := c.Len(); got != 0 {
		t.Fatalf("cache entries after delete = %d, want 0 (all credential/ref/raw variants must be invalidated)", got)
	}
}

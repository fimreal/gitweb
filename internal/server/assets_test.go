package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// assertAssetsResolve 抽取 HTML 里引用的每个 /static/ 资源并逐个走一遍路由。
// 模板把资源名写错时，浏览器只是静默 404，服务端测试若不主动请求就永远发现不了；
// 前端重构会大量挪动样式与脚本，这一层比手写的 contains 清单更可靠。
func assertAssetsResolve(t *testing.T, r *gin.Engine, body string) {
	t.Helper()
	seen := map[string]bool{}
	for _, path := range regexp.MustCompile(`/static/[^"'()\s]+`).FindAllString(body, -1) {
		if seen[path] {
			continue
		}
		seen[path] = true
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code >= 400 {
			t.Errorf("referenced asset %s resolves to %d", path, w.Code)
		}
	}
}

// TestPageResourceManifest 是模板渲染冒烟基线：锁定「每个页面加载哪些 css/js」
// 与 viewer 的 CSP/nonce。下一步把各页 head 抽成公共片段时会刻意改动这张表，
// 届时应显式更新本表而不是让它静默通过——每个页面都只用得到的脚本子集，
// 少带一个就会留下「某脚本引用了别处定义的全局」这类只有点开才炸的洞。
func TestPageResourceManifest(t *testing.T) {
	r, reg := newTestServer(t)
	if _, err := reg.Register("https://github.com/o/demo.git", "demo", "main", "github", nil, false); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		path     string
		status   int
		contains []string
		absent   []string
	}{
		{
			name: "index", path: "/", status: http.StatusOK,
			contains: []string{"/static/css/style.css", "/static/css/toast.css", "/static/js/toast.js", "/static/js/app.js", "/static/js/theme.js"},
			absent:   []string{"/static/css/viewer.css"},
		},
		{
			name: "viewer", path: "/demo/", status: http.StatusOK,
			contains: []string{"/static/css/viewer.css", "/static/css/toast.css", "/static/js/toast.js", "/static/js/theme.js"},
			absent:   []string{"/static/css/style.css"},
		},
		{
			name: "not found", path: "/missing-file-that-does-not-exist/x", status: http.StatusNotFound,
			contains: []string{"Error 404", "/static/css/style.css", "/static/js/theme.js"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.status {
				t.Fatalf("GET %s: status %d, want %d", tc.path, w.Code, tc.status)
			}
			html, _ := io.ReadAll(w.Result().Body)
			body := string(html)
			for _, want := range tc.contains {
				if !strings.Contains(body, want) {
					t.Errorf("GET %s: missing %q", tc.path, want)
				}
			}
			for _, no := range tc.absent {
				if strings.Contains(body, no) {
					t.Errorf("GET %s: unexpectedly loads %q", tc.path, no)
				}
			}
			assertAssetsResolve(t, r, body)
		})
	}
}

// TestLoginPageResourceManifest 锁住登录页的两种既有行为：
// 未配置访问密码时 /login 不应渲染（302 回首页），配置了才渲染登录卡片。
func TestLoginPageResourceManifest(t *testing.T) {
	t.Run("no password redirects", func(t *testing.T) {
		r, _ := newTestServer(t)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
		if w.Code != http.StatusFound {
			t.Fatalf("GET /login without password: status %d, want 302", w.Code)
		}
	})

	t.Run("with password renders login card", func(t *testing.T) {
		r, _ := newTestServer(t, "s3cret")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET /login with password: status %d, want 200", w.Code)
		}
		html, _ := io.ReadAll(w.Result().Body)
		body := string(html)
		for _, want := range []string{"/static/css/style.css", `action="/login"`, "login-card", "/static/js/theme.js"} {
			if !strings.Contains(body, want) {
				t.Errorf("login page missing %q", want)
			}
		}
		if strings.Contains(body, "/static/css/viewer.css") {
			t.Error("login page unexpectedly loads viewer.css")
		}
		assertAssetsResolve(t, r, body)
	})
}

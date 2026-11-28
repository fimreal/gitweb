package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fimreal/gitweb/internal/cache"
	"github.com/fimreal/gitweb/internal/provider"
	"github.com/fimreal/gitweb/internal/registry"
	"github.com/fimreal/gitweb/internal/render"
	"github.com/gin-gonic/gin"
)

// refsFixture 起一个假的 Gitea 上游 + 一个注册到它的 gitweb 路由，
// 用来验证 /api/sites/:pathid/refs 的过滤、上限与校验行为。
type refsFixture struct {
	r       *gin.Engine
	mu      sync.Mutex
	paths   []string
	queries []string
}

func newRefsFixture(t *testing.T, reply string) *refsFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard

	f := &refsFixture{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.queries = append(f.queries, r.URL.RawQuery)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}))
	t.Cleanup(up.Close)

	reg := registry.New()
	prov := provider.NewManager(5*time.Second, "", "", []string{"127.0.0.1", "localhost"}, nil, 1<<20, 100, "")
	srv := New(reg, prov, cache.New(time.Minute, 16), render.New(), testBaseURL, "")
	if _, err := reg.Register(up.URL+"/o/repo", "site", "main", "gitea", nil, false); err != nil {
		t.Fatalf("register site: %v", err)
	}
	f.r = srv.SetupRoutes()
	return f
}

func (f *refsFixture) get(t *testing.T, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "localhost:8080"
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

func (f *refsFixture) seen() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.queries...)
}

// TestSearchRefsEndpointFiltersServerSide：关键字原样转给上游（服务端过滤），
// 返回带类型的结果与 current，且 limit 体现在上游的 limit 参数上。
func TestSearchRefsEndpointFiltersServerSide(t *testing.T) {
	f := newRefsFixture(t, `[{"name":"feature/login"}]`)

	w := f.get(t, "/api/sites/site/refs?q=feature&limit=7")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got struct {
		Refs []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"refs"`
		Truncated bool   `json:"truncated"`
		Current   string `json:"current"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Current != "main" {
		t.Errorf("current = %q, want main", got.Current)
	}
	if len(got.Refs) != 2 || got.Refs[0].Name != "feature/login" || got.Refs[0].Type != "branch" || got.Refs[1].Type != "tag" {
		t.Errorf("refs = %+v, want 分支+tag", got.Refs)
	}
	paths, queries := f.seen()
	if len(paths) != 2 {
		t.Fatalf("upstream 请求数 = %d (%v), want 2（branches+tags）", len(paths), paths)
	}
	for _, p := range paths {
		if !strings.Contains(p, "/api/v1/repos/o/repo/") {
			t.Errorf("upstream path = %q, want gitea repo api", p)
		}
	}
	// 分支直接把关键字透传给上游；tag 接口不认 q，走分页后在服务端过滤。
	if !strings.Contains(strings.Join(paths, " "), "/branches") || !strings.Contains(strings.Join(paths, " "), "/tags") {
		t.Errorf("upstream paths = %v, want branches + tags", paths)
	}
	var branchQuery, tagQuery string
	for i, p := range paths {
		switch {
		case strings.HasSuffix(p, "/branches"):
			branchQuery = queries[i]
		case strings.HasSuffix(p, "/tags"):
			tagQuery = queries[i]
		}
	}
	if !strings.Contains(branchQuery, "q=feature") || !strings.Contains(branchQuery, "limit=7") {
		t.Errorf("branches query = %q, want q=feature + limit=7", branchQuery)
	}
	if !strings.Contains(tagQuery, "page=1") {
		t.Errorf("tags query = %q, want 分页参数", tagQuery)
	}
}

// TestSearchRefsEndpointRejectsBadQuery：非法关键字（空格、..）直接 400，
// 不会带着任意串去打上游。
func TestSearchRefsEndpointRejectsBadQuery(t *testing.T) {
	f := newRefsFixture(t, `[]`)
	for _, q := range []string{"feat ure", "../etc", "a%20b", strings.Repeat("a", 101)} {
		w := f.get(t, "/api/sites/site/refs?q="+url.QueryEscape(q))
		if w.Code != http.StatusBadRequest {
			t.Errorf("q=%q status = %d, want 400", q, w.Code)
		}
	}
	if paths, _ := f.seen(); len(paths) != 0 {
		t.Errorf("非法关键字不该请求上游，实际 = %v", paths)
	}
}

// TestSearchRefsEndpointUnknownSite：未知站点 404。
func TestSearchRefsEndpointUnknownSite(t *testing.T) {
	f := newRefsFixture(t, `[]`)
	if w := f.get(t, "/api/sites/nope/refs?q=main"); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// TestValidRefQuery：关键字校验的边界，空串合法（列默认分支），字符集与 ref 对齐。
func TestValidRefQuery(t *testing.T) {
	cases := []struct {
		q    string
		want bool
	}{
		{"", true},
		{"main", true},
		{"agent/fire/3e7d", true},
		{"v0.5.0", true},
		{"feat_ure-1.2", true},
		{"feat ure", false},
		{"../../etc", false},
		{"a..b", false},
		{"分支", false},
		{strings.Repeat("a", 101), false},
	}
	for _, tc := range cases {
		if got := validRefQuery(tc.q); got != tc.want {
			t.Errorf("validRefQuery(%q) = %v, want %v", tc.q, got, tc.want)
		}
	}
}

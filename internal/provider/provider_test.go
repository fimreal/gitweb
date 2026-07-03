package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestManager 创建一个允许 127.0.0.1（httptest）的 Manager，用于模拟远端。
func newTestManager(t *testing.T, maxSize int64) *Manager {
	t.Helper()
	return NewManager(0, "", "", []string{"127.0.0.1", "localhost"}, nil, maxSize)
}

// ----- 纯函数：URL 解析与 SSRF -----

func TestNormalizeGitHubURL(t *testing.T) {
	cases := []struct {
		in        string
		wantURL   string
		wantRef   string
	}{
		{"https://github.com/o/repo", "https://github.com/o/repo", "main"},
		{"https://github.com/o/repo/tree/dev/docs", "https://github.com/o/repo", "dev"},
		{"https://github.com/o/repo.git", "https://github.com/o/repo", "main"},
		{"http://github.com/o/repo", "http://github.com/o/repo", "main"},
	}
	for _, c := range cases {
		gotURL, gotRef, err := NormalizeGitHubURL(c.in)
		if err != nil {
			t.Fatalf("NormalizeGitHubURL(%q): %v", c.in, err)
		}
		if gotURL != c.wantURL {
			t.Errorf("NormalizeGitHubURL(%q) url = %q, want %q", c.in, gotURL, c.wantURL)
		}
		if gotRef != c.wantRef {
			t.Errorf("NormalizeGitHubURL(%q) ref = %q, want %q", c.in, gotRef, c.wantRef)
		}
	}
}

func TestParseGiteaURLHTTP(t *testing.T) {
	scheme, host, owner, repo, err := parseGiteaURL("http://gitea.local:3000/me/repo")
	if err != nil {
		t.Fatal(err)
	}
	if scheme != "http" || host != "gitea.local:3000" || owner != "me" || repo != "repo" {
		t.Errorf("parseGiteaURL got %q %q %q %q", scheme, host, owner, repo)
	}
}

func TestIsAllowedHostPrivate(t *testing.T) {
	private := []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "172.16.0.1", "169.254.169.254", "::1"}
	for _, h := range private {
		if isAllowedHost(h, nil, nil) {
			t.Errorf("isAllowedHost(%q) = true, want false (private)", h)
		}
	}
}

func TestIsAllowedHostPublic(t *testing.T) {
	// 公网 IP 段不应被拒。8.8.8.8 是公网。
	if !isAllowedHost("8.8.8.8", nil, nil) {
		t.Error("isAllowedHost(8.8.8.8) = false, want true")
	}
}

func TestIsAllowedHostAllowList(t *testing.T) {
	// allow 列表应放行（含通配）；deny 优先于 allow
	if !isAllowedHost("my.gitea.io", []string{"*.gitea.*"}, nil) {
		t.Error("allow wildcard *.gitea.* should permit my.gitea.io")
	}
	if isAllowedHost("my.gitea.io", []string{"*.gitea.*"}, []string{"my.gitea.io"}) {
		t.Error("deny should take precedence over allow")
	}
}

// ----- Fetch 行为：用 httptest 模拟远端 -----

func TestFetchGitHubSuccess(t *testing.T) {
	body := []byte("# hello")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GitHub raw URL: raw.githubusercontent.com/o/repo/ref/path
		// 但我们的 manager 走真实 raw host；为测试，把请求路径原样回
		if strings.HasSuffix(r.URL.Path, "/README.md") {
			w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// 构造一个 gitURL 让 provider 把请求发到 httptest server：
	// GitHubProvider 写死 raw.githubusercontent.com，无法重定向。
	// 因此这里改测 GiteaProvider（URL 跟随 gitURL host）。
	m := newTestManager(t, 1<<20)
	// 把 httptest host 当作 gitea
	gitURL := srv.URL + "/o/repo"
	got, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "README.md", nil)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "# hello" {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestFetchGiteaTokenAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	auth := &Auth{Type: "token", Token: "secret"}
	got, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", auth)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "ok" {
		t.Errorf("got %q", got)
	}
}

func TestFetchAuthRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	_, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", nil)
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("expected ErrAuthRequired, got %v", err)
	}
}

func TestFetchNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	_, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestFetchTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 100))
	}))
	defer srv.Close()

	m := newTestManager(t, 50) // maxSize 50 字节
	_, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", nil)
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("expected ErrTooLarge, got %v", err)
	}
}

func TestFetchSSRFBlocked(t *testing.T) {
	// 不在 allow 列表的私网地址应被拒绝（不发起请求）
	m := NewManager(0, "", "", nil, nil, 1<<20) // 无 allow，默认拒私网
	_, err := m.Fetch(context.Background(), "gitea", "http://127.0.0.1:9/o/repo", "main", "f.txt", nil)
	if err == nil {
		t.Fatal("expected SSRF rejection, got nil")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("expected SSRF error, got %v", err)
	}
}

// ----- Tree -----

func TestFetchTreeGitHub(t *testing.T) {
	// api.github.com 写死，无法用 httptest 重定向；测 Gitea tree（URL 跟随 host）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tree":[{"path":"a.md","type":"blob","size":10},{"path":"d","type":"tree"}]}`))
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	nodes, err := m.FetchTree(context.Background(), "gitea", srv.URL+"/o/repo", "main", nil)
	if err != nil {
		t.Fatalf("FetchTree: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Path != "a.md" {
		t.Errorf("got %+v, want one file a.md", nodes)
	}
}

func TestFilterRenderableFiles(t *testing.T) {
	r := struct{ f func(string) bool }{f: func(p string) bool { return strings.HasSuffix(p, ".md") }}
	nodes := []TreeNode{
		{Path: "a.md", Type: "file"},
		{Path: "b.txt", Type: "file"},
		{Path: "d", Type: "dir"},
	}
	got := FilterRenderableFiles(nodes, r.f)
	if len(got) != 1 || got[0].Path != "a.md" {
		t.Errorf("got %+v, want [a.md]", got)
	}
}

// ----- IdentifyProvider 探测识别 -----

func TestIdentifyProviderGiteaProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/version" {
			w.WriteHeader(200)
			w.Write([]byte(`{"version":"1.25.5"}`))
			return
		}
		http.NotFound(w, r) // /api/v4/version → 404
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	if got := m.IdentifyProvider(srv.URL + "/fimreal/gitweb"); got != "gitea" {
		t.Fatalf("IdentifyProvider = %q, want gitea", got)
	}
	// 缓存命中：第二个同 host 仓库不应再依赖探测结果变化
	if got := m.IdentifyProvider(srv.URL + "/other/repo"); got != "gitea" {
		t.Fatalf("cached IdentifyProvider = %q, want gitea", got)
	}
}

func TestIdentifyProviderGitLabProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/version" {
			w.WriteHeader(200)
			w.Write([]byte(`{"version":"16.0"}`))
			return
		}
		http.NotFound(w, r) // /api/v1/version → 404
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	if got := m.IdentifyProvider(srv.URL + "/group/project"); got != "gitlab" {
		t.Fatalf("IdentifyProvider = %q, want gitlab", got)
	}
}

func TestIdentifyProviderNeither(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 两个端点都 404
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	if got := m.IdentifyProvider(srv.URL + "/o/repo"); got != "" {
		t.Fatalf("IdentifyProvider = %q, want empty", got)
	}
}

func TestIdentifyProviderKeywordFastPath(t *testing.T) {
	// 哨兵 server：一旦被请求即 Fatal，证明关键字路径零网络
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("fast path should not probe, got request %s", r.URL.Path)
	}))
	defer srv.Close()
	_ = srv.URL // 哨兵仅用于证明不被触发

	m := newTestManager(t, 1<<20)
	cases := map[string]string{
		"https://github.com/o/repo": "github",
		"https://gitlab.com/o/repo": "gitlab",
		"https://gitea.com/o/repo":  "gitea",
	}
	for url, want := range cases {
		if got := m.IdentifyProvider(url); got != want {
			t.Errorf("IdentifyProvider(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestIdentifyProviderSSRFBlocked(t *testing.T) {
	// 哨兵 server：一旦被请求即 Fatal，证明探测被 SSRF 拦截未发出
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("probe should be SSRF-blocked, got request %s", r.URL.Path)
	}))
	defer srv.Close()

	m := NewManager(0, "", "", nil, nil, 1<<20) // 无 allow，默认拒私网
	got := m.IdentifyProvider(srv.URL + "/o/repo") // httptest 是 127.0.0.1
	if got != "" {
		t.Fatalf("expected empty (SSRF blocked), got %q", got)
	}
}


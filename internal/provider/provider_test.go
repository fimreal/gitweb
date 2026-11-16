package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestManager 创建一个允许 127.0.0.1（httptest）的 Manager，用于模拟远端。
func newTestManager(t *testing.T, maxSize int64) *Manager {
	t.Helper()
	return NewManager(0, "", "", []string{"127.0.0.1", "localhost"}, nil, maxSize, 0, "")
}

// ----- 纯函数：URL 解析与 SSRF -----

func TestNormalizeGitHubURL(t *testing.T) {
	cases := []struct {
		in      string
		wantURL string
		wantRef string
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

	// 携带凭据时 404 视为真实的 not-found（文件/分支不存在或凭据无权限）
	m := newTestManager(t, 1<<20)
	auth := &Auth{Type: "token", Token: "secret"}
	_, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", auth)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestFetchNotFoundNoAuth：无凭据时 404 应视为需要鉴权。
// GitHub/GitLab/Gitea 对私有仓库在无凭据时统一返回 404（不泄露存在性），
// 把它映射为 ErrAuthRequired 才能触发前端凭据输入。
func TestFetchNotFoundNoAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	_, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", nil)
	if !errors.Is(err, ErrAuthRequired) {
		t.Errorf("expected ErrAuthRequired, got %v", err)
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
	m := NewManager(0, "", "", nil, nil, 1<<20, 0, "") // 无 allow，默认拒私网
	_, err := m.Fetch(context.Background(), "gitea", "http://127.0.0.1:9/o/repo", "main", "f.txt", nil)
	if err == nil {
		t.Fatal("expected SSRF rejection, got nil")
	}
	if !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("expected SSRF error, got %v", err)
	}
}

// ----- singleflight 凭据隔离：并发跨凭据不得共享上游结果 -----

// heldUpstream 把 mock 上游的每个请求钉在 handler 里：登记到达身份后阻塞，
// 直到测试显式 unblock()，没有超时放行。第一路在放行前永不完成，因此第二路
// 是否并入在途航班只取决于合并键，与调度时序无关：键不含凭据（旧实现）时
// 第二路必然并入，其身份永远不会到达，waitForArrivals 超时判失败——隔离测试
// 在旧实现下必然失败；键混入指纹（修复后）时第二路独立成行，两种身份同时
// 被观察到即证明两路确实同时在途，此时放行再断言各自结果。
type heldUpstream struct {
	mu       sync.Mutex
	arrivals []string
	release  chan struct{}
	once     sync.Once
}

func (h *heldUpstream) arrive(identity string) {
	h.mu.Lock()
	h.arrivals = append(h.arrivals, identity)
	h.mu.Unlock()
	<-h.release
}

func (h *heldUpstream) unblock() {
	h.once.Do(func() { close(h.release) })
}

func (h *heldUpstream) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.arrivals)
}

// waitForArrivals 等到每种身份都至少到达一次。按身份而非次数等：匿名 404 会按
// branch/tag/commit 三种 ref 形式重试（QEST-74），同一身份可能到达多次。
// 超时是失败判定而非放行兜底——被等的身份若并入在途航班就永远不会到达。
func (h *heldUpstream) waitForArrivals(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		seen := make(map[string]bool, len(h.arrivals))
		for _, a := range h.arrivals {
			seen[a] = true
		}
		var missing []string
		for _, w := range want {
			if !seen[w] {
				missing = append(missing, w)
			}
		}
		got := append([]string(nil), h.arrivals...)
		h.mu.Unlock()
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("upstream arrivals = %q, missing identities %q：缺失身份说明该路并入了在途航班（合并键未隔离凭据）", got, missing)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitInSingleflightDo 等到至少 n 个 goroutine 的调用栈都在 singleflight.Do 内
// （一个在执行航班函数，其余在等结果）。同凭据合并时第二路不再产生上游请求，
// 到达事件无法证明它已进入 Do，只能观察运行时栈；轮询条件是确定状态而非睡眠
// 猜测，超时仅用于失败判定。
func waitInSingleflightDo(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 1<<20)
	for {
		stack := string(buf[:runtime.Stack(buf, true)])
		if strings.Count(stack, "singleflight.(*Group).Do(") >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines inside singleflight.Do < %d after 5s", n)
		}
		time.Sleep(time.Millisecond)
	}
}

type fetchResult struct {
	data []byte
	err  error
}

// TestFetchConcurrentCredentialIsolation：相同 provider/URL/ref/path，带凭据与
// 匿名的两路并发。带凭据的一路必须拿到内容；匿名的一路必须 ErrAuthRequired
// 且拿不到任何内容——合并键不含凭据时，匿名会复用带凭据请求的上游结果读到
// 私有内容（或反过来把带凭据的一路拖成未授权）。
func TestFetchConcurrentCredentialIsolation(t *testing.T) {
	h := &heldUpstream{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.arrive(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "token secret" {
			w.Write([]byte("private content"))
			return
		}
		http.NotFound(w, r) // 私有仓库对匿名统一 404，三种 ref 形式都会试
	}))
	defer srv.Close()
	defer h.unblock() // 失败路径也放行 handler，否则 srv.Close 会一直等在途请求

	m := newTestManager(t, 1<<20)
	gitURL := srv.URL + "/o/repo"
	auth := &Auth{Type: "token", Token: "secret"}

	credentialed := make(chan fetchResult, 1)
	anonymous := make(chan fetchResult, 1)
	go func() {
		d, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "f.txt", auth)
		credentialed <- fetchResult{d, err}
	}()
	// 第一路已钉在 handler 里（在途航班确定存在），再放第二路进入 Fetch。
	h.waitForArrivals(t, "token secret")
	go func() {
		d, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "f.txt", nil)
		anonymous <- fetchResult{d, err}
	}()
	// 两种身份同时被观察到才放行：此刻两路确实都在途，并发窗口由构造保证。
	h.waitForArrivals(t, "token secret", "")
	h.unblock()

	a := <-credentialed
	if a.err != nil || string(a.data) != "private content" {
		t.Fatalf("credentialed fetch = (%q, %v), want private content", a.data, a.err)
	}
	b := <-anonymous
	if !errors.Is(b.err, ErrAuthRequired) || b.data != nil {
		t.Fatalf("anonymous fetch = (%q, %v), want ErrAuthRequired and no content", b.data, b.err)
	}
}

// TestFetchConcurrentDifferentCredentials：两个都有效但不同的凭据并发抓同一
// 文件，上游按凭据返回不同内容，各路必须拿到自己的那份。
func TestFetchConcurrentDifferentCredentials(t *testing.T) {
	h := &heldUpstream{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.arrive(r.Header.Get("Authorization"))
		switch r.Header.Get("Authorization") {
		case "token alpha":
			w.Write([]byte("content-alpha"))
		case "token beta":
			w.Write([]byte("content-beta"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	defer h.unblock()

	m := newTestManager(t, 1<<20)
	gitURL := srv.URL + "/o/repo"

	resA := make(chan fetchResult, 1)
	resB := make(chan fetchResult, 1)
	go func() {
		d, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "f.txt", &Auth{Type: "token", Token: "alpha"})
		resA <- fetchResult{d, err}
	}()
	h.waitForArrivals(t, "token alpha")
	go func() {
		d, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "f.txt", &Auth{Type: "token", Token: "beta"})
		resB <- fetchResult{d, err}
	}()
	h.waitForArrivals(t, "token alpha", "token beta")
	h.unblock()

	if a := <-resA; a.err != nil || string(a.data) != "content-alpha" {
		t.Fatalf("alpha fetch = (%q, %v), want content-alpha", a.data, a.err)
	}
	if b := <-resB; b.err != nil || string(b.data) != "content-beta" {
		t.Fatalf("beta fetch = (%q, %v), want content-beta", b.data, b.err)
	}
}

// TestFetchConcurrentSameCredentialStillMerges：凭据指纹隔离不能把 singleflight
// 整个废掉——相同凭据的并发请求仍应合并为一次上游调用。
func TestFetchConcurrentSameCredentialStillMerges(t *testing.T) {
	h := &heldUpstream{release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.arrive(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "token secret" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	defer h.unblock()

	m := newTestManager(t, 1<<20)
	gitURL := srv.URL + "/o/repo"
	auth := &Auth{Type: "token", Token: "secret"}

	resA := make(chan fetchResult, 1)
	resB := make(chan fetchResult, 1)
	go func() {
		d, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "f.txt", auth)
		resA <- fetchResult{d, err}
	}()
	h.waitForArrivals(t, "token secret")
	go func() {
		d, err := m.Fetch(context.Background(), "gitea", gitURL, "main", "f.txt", auth)
		resB <- fetchResult{d, err}
	}()
	// 第二路并入在途航班后没有上游事件可观察，等运行时确认两路都在 Do 里
	// 才放行，排除「第二路迟到、航班已完成」造成的 count=2 伪失败。
	waitInSingleflightDo(t, 2)
	h.unblock()

	if a := <-resA; a.err != nil || string(a.data) != "ok" {
		t.Fatalf("first fetch = (%q, %v), want ok", a.data, a.err)
	}
	if b := <-resB; b.err != nil || string(b.data) != "ok" {
		t.Fatalf("second fetch = (%q, %v), want ok", b.data, b.err)
	}
	if n := h.count(); n != 1 {
		t.Errorf("upstream requests = %d, want 1 (same-credential flights must merge)", n)
	}
}

// ----- 凭据指纹 -----

func TestAuthFingerprint(t *testing.T) {
	if got := AuthFingerprint(nil); got != "anon" {
		t.Errorf("nil auth fingerprint = %q, want anon", got)
	}
	a := &Auth{Type: "token", Token: "secret"}
	fp := AuthFingerprint(a)
	if fp != AuthFingerprint(a) {
		t.Error("fingerprint must be stable for the same auth")
	}
	if len(fp) != 16 {
		t.Errorf("fingerprint length = %d, want 16", len(fp))
	}
	if strings.Contains(fp, "secret") {
		t.Error("fingerprint must not contain the raw credential")
	}
	if fp == AuthFingerprint(nil) {
		t.Error("credentialed fingerprint must differ from anon")
	}
	if fp == AuthFingerprint(&Auth{Type: "token", Token: "other"}) {
		t.Error("different tokens must produce different fingerprints")
	}
	if fp == AuthFingerprint(&Auth{Type: "basic", Username: "u", Password: "p"}) {
		t.Error("token and basic credentials must produce different fingerprints")
	}
	// 字段切分歧义：「ab|c」与「a|bc」直接拼接相同，指纹必须不同。
	if AuthFingerprint(&Auth{Type: "ab", Token: "c"}) == AuthFingerprint(&Auth{Type: "a", Token: "bc"}) {
		t.Error("field-boundary ambiguity must produce different fingerprints")
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

	m := NewManager(0, "", "", nil, nil, 1<<20, 0, "") // 无 allow，默认拒私网
	got := m.IdentifyProvider(srv.URL + "/o/repo")     // httptest 是 127.0.0.1
	if got != "" {
		t.Fatalf("expected empty (SSRF blocked), got %q", got)
	}
}

// ----- Gitea ref 形式探测 -----

// newPathRecorder 返回一个只对 servePath 回 200、其余回 404 的 mock Gitea，
// 并记录收到的请求路径。Gitea 不把 tag/commit 当 branch 解析，ref 必须按
// branch→tag→commit 依次探测。
func newPathRecorder(t *testing.T, servePath string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == servePath {
			w.Write([]byte("ok"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func TestGiteaRefResolution(t *testing.T) {
	cases := []struct {
		name      string
		ref       string
		servePath string
		wantPath  string
		wantCalls int
	}{
		{"branch first", "main", "/o/repo/raw/branch/main/f.txt", "/o/repo/raw/branch/main/f.txt", 1},
		{"tag fallback", "v1.0.0", "/o/repo/raw/tag/v1.0.0/f.txt", "/o/repo/raw/tag/v1.0.0/f.txt", 2},
		{"commit fallback", "6cfd6f45", "/o/repo/raw/commit/6cfd6f45/f.txt", "/o/repo/raw/commit/6cfd6f45/f.txt", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, paths := newPathRecorder(t, tc.servePath)

			m := newTestManager(t, 1<<20)
			got, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", tc.ref, "f.txt", nil)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if string(got) != "ok" {
				t.Errorf("body = %q, want ok", got)
			}
			gotPaths := paths()
			if len(gotPaths) != tc.wantCalls {
				t.Fatalf("requests = %v, want %d", gotPaths, tc.wantCalls)
			}
			if last := gotPaths[len(gotPaths)-1]; last != tc.wantPath {
				t.Errorf("last request = %q, want %q", last, tc.wantPath)
			}
		})
	}
}

// TestGiteaRefResolutionCached：同站点的后续文件不应为 tag/commit ref 重新付
// 两次探测成本；命中缓存后应直接打正确形式。
func TestGiteaRefResolutionCached(t *testing.T) {
	srv, paths := newPathRecorder(t, "/o/repo/raw/tag/v1.0.0/f.txt")

	m := newTestManager(t, 1<<20)
	gitURL := srv.URL + "/o/repo"
	for i := 0; i < 2; i++ {
		if _, err := m.Fetch(context.Background(), "gitea", gitURL, "v1.0.0", "f.txt", nil); err != nil {
			t.Fatalf("Fetch #%d: %v", i+1, err)
		}
	}
	got := paths()
	if len(got) != 3 {
		t.Fatalf("requests = %v, want 3 (2 for first fetch, 1 for cached second)", got)
	}
	if got[2] != "/o/repo/raw/tag/v1.0.0/f.txt" {
		t.Errorf("cached fetch hit %q, want tag form first", got[2])
	}
}

// TestGiteaRefNotFoundSemantics：三种形式都 404 时沿用全局语义 —— 无凭据视为
// 需要鉴权，携带凭据才是真的 not-found。
func TestGiteaRefNotFoundSemantics(t *testing.T) {
	srv, _ := newPathRecorder(t, "")

	m := newTestManager(t, 1<<20)
	gitURL := srv.URL + "/o/repo"
	if _, err := m.Fetch(context.Background(), "gitea", gitURL, "missing", "f.txt", nil); !errors.Is(err, ErrAuthRequired) {
		t.Errorf("no auth: err = %v, want ErrAuthRequired", err)
	}
	auth := &Auth{Type: "token", Token: "secret"}
	if _, err := m.Fetch(context.Background(), "gitea", gitURL, "missing", "f.txt", auth); !errors.Is(err, ErrNotFound) {
		t.Errorf("with auth: err = %v, want ErrNotFound", err)
	}
}

// TestGiteaRefAuthFailureStopsProbing：401/403 表示凭据被拒，换 ref 形式没有意义，
// 应立即返回而不是再多打两次上游。
func TestGiteaRefAuthFailureStopsProbing(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	m := newTestManager(t, 1<<20)
	_, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", nil)
	if !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v, want ErrAuthRequired", err)
	}
	if len(paths) != 1 {
		t.Errorf("requests = %v, want exactly 1 before giving up", paths)
	}
}

// ----- 凭据头：三种 provider 的差异只在 applyAuth 一处 -----

func TestCredentialHeaders(t *testing.T) {
	t.Run("gitlab uses PRIVATE-TOKEN", func(t *testing.T) {
		var gotPrivate, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPrivate = r.Header.Get("PRIVATE-TOKEN")
			gotAuth = r.Header.Get("Authorization")
			w.Write([]byte("ok"))
		}))
		defer srv.Close()

		m := newTestManager(t, 1<<20)
		auth := &Auth{Type: "token", Token: "secret"}
		if _, err := m.Fetch(context.Background(), "gitlab", srv.URL+"/group/repo", "main", "f.txt", auth); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if gotPrivate != "secret" {
			t.Errorf("PRIVATE-TOKEN = %q, want secret", gotPrivate)
		}
		if gotAuth != "" {
			t.Errorf("Authorization = %q, want empty for GitLab", gotAuth)
		}
	})

	t.Run("gitea falls back to basic auth", func(t *testing.T) {
		var user, pass string
		var ok bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok = r.BasicAuth()
			w.Write([]byte("ok"))
		}))
		defer srv.Close()

		m := newTestManager(t, 1<<20)
		auth := &Auth{Type: "basic", Username: "u", Password: "p"}
		if _, err := m.Fetch(context.Background(), "gitea", srv.URL+"/o/repo", "main", "f.txt", auth); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if !ok || user != "u" || pass != "p" {
			t.Errorf("basic auth = (%q,%q,%v), want (u,p,true)", user, pass, ok)
		}
	})
}

// ----- GitHub 域名可配置（GHE） -----

func TestGitHubURLBuilders(t *testing.T) {
	pub := providerBase{}
	if got, want := pub.githubRaw("github.com", "o", "r", "main", "docs/a.md"), "https://raw.githubusercontent.com/o/r/main/docs/a.md"; got != want {
		t.Errorf("public raw = %q, want %q", got, want)
	}
	if got, want := pub.githubAPI("github.com", "/repos/o/r/branches"), "https://api.github.com/repos/o/r/branches"; got != want {
		t.Errorf("public api = %q, want %q", got, want)
	}

	ghe := providerBase{githubBase: "https://ghe.example.com", githubBaseHost: "ghe.example.com"}
	if got, want := ghe.githubRaw("ghe.example.com", "o", "r", "main", "docs/a.md"), "https://ghe.example.com/o/r/raw/main/docs/a.md"; got != want {
		t.Errorf("ghe raw = %q, want %q", got, want)
	}
	if got, want := ghe.githubAPI("ghe.example.com", "/repos/o/r/branches"), "https://ghe.example.com/api/v3/repos/o/r/branches"; got != want {
		t.Errorf("ghe api = %q, want %q", got, want)
	}

	// 配置了 GHE 也不应把公网 github.com 仓库改道到企业版实例。
	if got, want := ghe.githubRaw("github.com", "o", "r", "main", "a.md"), "https://raw.githubusercontent.com/o/r/main/a.md"; got != want {
		t.Errorf("mixed public raw = %q, want %q", got, want)
	}
	if got, want := ghe.githubAPI("github.com", "/repos/o/r/branches"), "https://api.github.com/repos/o/r/branches"; got != want {
		t.Errorf("mixed public api = %q, want %q", got, want)
	}
}

// TestGitHubConfiguredBaseFetch：配置 github_base_url 后，raw 请求必须打到
// 配置的 host（而不是公网 raw.githubusercontent.com），且仍带 token 头。
func TestGitHubConfiguredBaseFetch(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("ghe"))
	}))
	defer srv.Close()

	m := NewManager(0, "", "", []string{"127.0.0.1", "localhost"}, nil, 1<<20, 0, srv.URL)
	auth := &Auth{Type: "token", Token: "secret"}
	got, err := m.Fetch(context.Background(), "github", srv.URL+"/o/repo", "main", "f.txt", auth)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(got) != "ghe" {
		t.Errorf("body = %q, want ghe", got)
	}
	if gotPath != "/o/repo/raw/main/f.txt" {
		t.Errorf("path = %q, want GHE raw form", gotPath)
	}
	if gotAuth != "token secret" {
		t.Errorf("Authorization = %q, want token secret", gotAuth)
	}
}

// TestGitHubConfiguredBaseTreeAndBranches：tree / branches 也必须跟随配置的 API 地址。
func TestGitHubConfiguredBaseTreeAndBranches(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/branches") {
			w.Write([]byte(`[{"name":"main"},{"name":"dev"}]`))
			return
		}
		w.Write([]byte(`{"tree":[{"path":"a.md","type":"blob","size":3}]}`))
	}))
	defer srv.Close()

	m := NewManager(0, "", "", []string{"127.0.0.1", "localhost"}, nil, 1<<20, 0, srv.URL)
	gitURL := srv.URL + "/o/repo"

	nodes, err := m.FetchTree(context.Background(), "github", gitURL, "main", nil)
	if err != nil {
		t.Fatalf("FetchTree: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Path != "a.md" {
		t.Errorf("nodes = %+v, want [a.md]", nodes)
	}
	branches, err := m.ListBranches(context.Background(), "github", gitURL, nil)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(branches) != 2 || branches[1] != "dev" {
		t.Errorf("branches = %v, want [main dev]", branches)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, p := range []string{"/api/v3/repos/o/repo/git/trees/main", "/api/v3/repos/o/repo/branches"} {
		if !seen[p] {
			t.Errorf("API path %q not requested; seen=%v", p, seen)
		}
	}
}

// TestIdentifyProviderConfiguredGitHubBase：GHE 的 host 通常不含 "github.com"，
// 配置了 github_base_url 后应零网络直接识别为 github。
func TestIdentifyProviderConfiguredGitHubBase(t *testing.T) {
	m := NewManager(0, "", "", nil, nil, 1<<20, 0, "https://ghe.example.com/")
	if got := m.IdentifyProvider("https://ghe.example.com/o/repo"); got != "github" {
		t.Errorf("IdentifyProvider = %q, want github", got)
	}
}

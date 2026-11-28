package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// refSearchFixture 记录上游收到的查询，并按路径返回预置 JSON。
type refSearchFixture struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
	query []string
}

func newRefSearchFixture(t *testing.T, reply func(path, rawQuery string) string) *refSearchFixture {
	t.Helper()
	f := &refSearchFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.query = append(f.query, r.URL.RawQuery)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply(r.URL.Path, r.URL.RawQuery)))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *refSearchFixture) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

func (f *refSearchFixture) queries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.query...)
}

// queryFor 返回命中该路径后缀的请求的原始查询串：分支与 tag 是并发发出的，
// 顺序不稳定，断言必须按路径而不是按到达序。
func (f *refSearchFixture) queryFor(suffix string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, p := range f.paths {
		if strings.HasSuffix(p, suffix) {
			return f.query[i]
		}
	}
	return ""
}

func newRefSearchManager(base string) *Manager {
	return NewManager(5*time.Second, "", "", []string{"127.0.0.1", "localhost"}, nil, 1<<20, 100, base)
}

// TestSearchRefsGitHubPrefixMatch：有关键字时打 matching-refs 的 heads/tags 两路，
// 合并结果并按 limit 截断；关键字里的斜杠必须转义，不能原样拼进 URL 路径。
func TestSearchRefsGitHubPrefixMatch(t *testing.T) {
	f := newRefSearchFixture(t, func(path, rawQuery string) string {
		switch {
		case strings.HasSuffix(path, "/git/matching-refs/heads/feature/x"):
			return `[{"ref":"refs/heads/feature/x"},{"ref":"refs/heads/feature/x2"}]`
		case strings.HasSuffix(path, "/git/matching-refs/tags/feature/x"):
			return `[{"ref":"refs/tags/feature/x-rc1"}]`
		}
		return `[]`
	})
	m := newRefSearchManager(f.srv.URL)

	refs, truncated, err := m.SearchRefs(context.Background(), "github", f.srv.URL+"/o/repo", nil, "feature/x", 10)
	if err != nil {
		t.Fatalf("SearchRefs: %v", err)
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	want := []Ref{
		{Name: "feature/x", Type: RefKindBranch},
		{Name: "feature/x2", Type: RefKindBranch},
		{Name: "feature/x-rc1", Type: RefKindTag},
	}
	if len(refs) != len(want) {
		t.Fatalf("refs = %+v, want %+v", refs, want)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("refs[%d] = %+v, want %+v", i, refs[i], want[i])
		}
	}
	seen := strings.Join(f.seen(), " ")
	if !strings.Contains(seen, "/git/matching-refs/heads/feature/x") ||
		!strings.Contains(seen, "/git/matching-refs/tags/feature/x") {
		t.Errorf("upstream paths = %v, want heads+tags matching-refs", seen)
	}
	for _, q := range f.queries() {
		if !strings.Contains(q, "per_page=10") {
			t.Errorf("upstream query %q 缺少 per_page=limit", q)
		}
	}
}

// TestSearchRefsGitHubEmptyQueryListsBranchesOnly：空关键字只列分支（不拉 tag），
// 且条数受 limit 约束 —— 首屏打开菜单时不该把整个 ref 空间拉下来。
func TestSearchRefsGitHubEmptyQueryListsBranchesOnly(t *testing.T) {
	f := newRefSearchFixture(t, func(path, rawQuery string) string {
		return `[{"name":"main"},{"name":"dev"},{"name":"release"}]`
	})
	m := newRefSearchManager(f.srv.URL)

	refs, truncated, err := m.SearchRefs(context.Background(), "github", f.srv.URL+"/o/repo", nil, "", 2)
	if err != nil {
		t.Fatalf("SearchRefs: %v", err)
	}
	if !truncated {
		t.Errorf("truncated = false, want true（3 条命中被截到 2 条）")
	}
	if len(refs) != 2 || refs[0].Name != "main" || refs[0].Type != RefKindBranch {
		t.Errorf("refs = %+v, want 前两条分支", refs)
	}
	for _, p := range f.seen() {
		if strings.Contains(p, "/tags") || strings.Contains(p, "matching-refs") {
			t.Errorf("空关键字不该请求 %q", p)
		}
	}
}

// TestSearchRefsGiteaForwardsQuery：分支用 branches 的 q 过滤；tag 接口不认 q，
// 由我们分页取回后在服务端过滤，命中之外的同页 tag 必须被丢掉。
func TestSearchRefsGiteaForwardsQuery(t *testing.T) {
	f := newRefSearchFixture(t, func(path, rawQuery string) string {
		if strings.HasSuffix(path, "/tags") {
			return `[{"name":"v0.5.0"},{"name":"v0.4.0"}]`
		}
		return `[{"name":"agent/fire/3e7d174a19e7"}]`
	})
	m := newRefSearchManager("")

	refs, _, err := m.SearchRefs(context.Background(), "gitea", f.srv.URL+"/o/repo", nil, "v0.5", 10)
	if err != nil {
		t.Fatalf("SearchRefs: %v", err)
	}
	if len(refs) != 2 || refs[0].Type != RefKindBranch || refs[1] != (Ref{Name: "v0.5.0", Type: RefKindTag}) {
		t.Errorf("refs = %+v, want 分支一条 + 过滤后的 tag v0.5.0", refs)
	}
	paths := f.seen()
	if len(paths) != 2 {
		t.Fatalf("upstream 请求数 = %d (%v), want 2（branches+tags）", len(paths), paths)
	}
	if q := f.queryFor("/branches"); !strings.Contains(q, "q=v0.5") {
		t.Errorf("branches query = %q, want 带 q=v0.5", q)
	}
	if q := f.queryFor("/tags"); strings.Contains(q, "q=") || !strings.Contains(q, "page=1") {
		t.Errorf("tags query = %q, want 分页参数且不靠 q（上游不认）", q)
	}
}

// TestSearchRefsGitLabUsesSearch：GitLab 关键字走 search 参数，且带 limit。
func TestSearchRefsGitLabUsesSearch(t *testing.T) {
	f := newRefSearchFixture(t, func(path, rawQuery string) string {
		return `[{"name":"feature/login"}]`
	})
	m := newRefSearchManager("")

	refs, _, err := m.SearchRefs(context.Background(), "gitlab", f.srv.URL+"/group/proj", nil, "feature", 15)
	if err != nil {
		t.Fatalf("SearchRefs: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("refs = %+v, want 分支与 tag 各一条", refs)
	}
	for _, q := range f.queries() {
		if !strings.Contains(q, "search=feature") || !strings.Contains(q, "per_page=15") {
			t.Errorf("upstream query = %q, want search + per_page", q)
		}
	}
}

// TestSearchRefsRejectsUnknownProvider：未支持的类型直接报错，而不是返回空列表。
func TestSearchRefsRejectsUnknownProvider(t *testing.T) {
	m := newRefSearchManager("")
	if _, _, err := m.SearchRefs(context.Background(), "svn", "https://example.com/o/repo", nil, "x", 10); err == nil {
		t.Fatal("want error for unsupported provider, got nil")
	}
}

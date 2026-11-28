package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Ref 是仓库里一个可切换的引用。Type 区分分支与 tag：同名时前端靠它区分展示。
type Ref struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

const (
	RefKindBranch = "branch"
	RefKindTag    = "tag"
)

// Gitea 的 tags 接口不认 q（branches 认，实测 1.27），tag 只能分页取回后在
// 服务端按关键字过滤。页数封顶，避免大型仓库为了一个词把上千 tag 全扫一遍。
const (
	giteaTagScanPageSize = 50
	giteaTagScanPages    = 3
)

// 引用搜索的返回上限，防止上游一次吐出上千条。
const (
	refSearchDefaultLimit = 30
	refSearchMaxLimit     = 100
)

// SearchRefs 按关键字在服务端过滤分支与 tag，最多返回 limit 条。
//
// 过滤交给各平台 API：GitHub 用 git/matching-refs 的前缀匹配（分支与 tag 各一次，
// 并发查），GitLab 用 repository/{branches,tags} 的 search，Gitea 用 branches 的 q
// 加 tags 的分页扫描。空关键字只返回分支，与旧的「切换分支」下拉保持一致。
//
// truncated 表示命中的条数已经顶到 limit 或扫描页数用尽，前端可提示「还有更多」。
func (m *Manager) SearchRefs(ctx context.Context, providerType, gitURL string, auth *Auth, query string, limit int) ([]Ref, bool, error) {
	if limit <= 0 {
		limit = refSearchDefaultLimit
	}
	if limit > refSearchMaxLimit {
		limit = refSearchMaxLimit
	}

	// 与 tree/branches 一致：先过 host 限流，再打上游。
	host := extractHost(gitURL)
	if host != "" {
		limiter := m.getLimiter(host)
		if !limiter.Allow() {
			return nil, false, ErrRateLimited
		}
	}

	base := m.base(false)
	switch providerType {
	case "github":
		return searchGitHubRefs(ctx, m.client, base, gitURL, auth, query, limit)
	case "gitlab":
		return searchGitLabRefs(ctx, m.client, base, gitURL, auth, query, limit)
	case "gitea":
		return searchGiteaRefs(ctx, m.client, base, gitURL, auth, query, limit)
	default:
		return nil, false, fmt.Errorf("ref search not supported for %s", providerType)
	}
}

// runRefQueries 并发跑几路互不依赖的查询，按传入顺序合并结果。
// 分支与 tag 是两次独立的往返，串行会让首开耗时等于两次之和；任一路出错即整体失败，
// 避免「只搜到分支」这种半截结果被当成完整答案展示。
func runRefQueries(fns ...func() ([]Ref, bool, error)) ([]Ref, bool, error) {
	type result struct {
		refs      []Ref
		truncated bool
		err       error
	}
	outs := make([]result, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() ([]Ref, bool, error)) {
			defer wg.Done()
			refs, truncated, err := fn()
			outs[i] = result{refs: refs, truncated: truncated, err: err}
		}(i, fn)
	}
	wg.Wait()

	var refs []Ref
	truncated := false
	for _, out := range outs {
		if out.err != nil {
			return nil, false, out.err
		}
		refs = append(refs, out.refs...)
		truncated = truncated || out.truncated
	}
	return refs, truncated, nil
}

// capRefs 截到 limit 条，并报告是否还有剩余。
func capRefs(refs []Ref, limit int) ([]Ref, bool) {
	if len(refs) <= limit {
		return refs, false
	}
	return refs[:limit], true
}

func searchGitHubRefs(ctx context.Context, client *http.Client, b providerBase, gitURL string, auth *Auth, query string, limit int) ([]Ref, bool, error) {
	owner, repo, err := parseGitHubURL(gitURL)
	if err != nil {
		return nil, false, err
	}
	_, gitHost, _ := probeHost(gitURL)

	// 空关键字：只列分支（与旧行为一致），per_page 就是上限，少拉一页。
	if query == "" {
		apiURL := b.githubAPI(gitHost, fmt.Sprintf("/repos/%s/%s/branches?per_page=%d", owner, repo, limit))
		names, err := gitHubBranchNames(ctx, client, b, "github", apiURL, auth)
		if err != nil {
			return nil, false, err
		}
		refs := make([]Ref, 0, len(names))
		for _, n := range names {
			refs = append(refs, Ref{Name: n, Type: RefKindBranch})
		}
		capped, more := capRefs(refs, limit)
		return capped, more, nil
	}

	refs, truncated, err := runRefQueries(
		func() ([]Ref, bool, error) {
			return githubMatchingRefs(ctx, client, b, gitHost, owner, repo, auth, "heads", RefKindBranch, query, limit)
		},
		func() ([]Ref, bool, error) {
			return githubMatchingRefs(ctx, client, b, gitHost, owner, repo, auth, "tags", RefKindTag, query, limit)
		},
	)
	if err != nil {
		return nil, false, err
	}
	capped, more := capRefs(refs, limit)
	return capped, truncated || more, nil
}

// githubMatchingRefs 用 GitHub 的前缀匹配接口取 heads 或 tags 下的 ref。
func githubMatchingRefs(ctx context.Context, client *http.Client, b providerBase, gitHost, owner, repo string, auth *Auth, segment, typ, query string, limit int) ([]Ref, bool, error) {
	apiURL := b.githubAPI(gitHost, fmt.Sprintf("/repos/%s/%s/git/matching-refs/%s/%s?per_page=%d",
		owner, repo, segment, url.PathEscape(query), limit))
	resp, err := b.open(ctx, client, "github", apiURL, auth)
	if err != nil {
		return nil, false, err
	}
	var payload []struct {
		Ref string `json:"ref"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
	resp.Body.Close()
	if decodeErr != nil {
		return nil, false, fmt.Errorf("github matching-refs json decode failed: %v", decodeErr)
	}
	prefix := "refs/" + segment + "/"
	refs := make([]Ref, 0, len(payload))
	for _, item := range payload {
		if !strings.HasPrefix(item.Ref, prefix) {
			continue
		}
		refs = append(refs, Ref{Name: strings.TrimPrefix(item.Ref, prefix), Type: typ})
	}
	return refs, len(payload) >= limit, nil
}

func searchGitLabRefs(ctx context.Context, client *http.Client, b providerBase, gitURL string, auth *Auth, query string, limit int) ([]Ref, bool, error) {
	scheme, host, projectPath, err := parseGitLabURL(gitURL)
	if err != nil {
		return nil, false, err
	}
	project := url.PathEscape(projectPath)

	// 空关键字只给分支，省掉 tag 那次往返。
	if query == "" {
		refs, truncated, err := gitlabRefList(ctx, client, b, scheme, host, project, auth, "branches", RefKindBranch, "", limit)
		if err != nil {
			return nil, false, err
		}
		capped, more := capRefs(refs, limit)
		return capped, truncated || more, nil
	}

	refs, truncated, err := runRefQueries(
		func() ([]Ref, bool, error) {
			return gitlabRefList(ctx, client, b, scheme, host, project, auth, "branches", RefKindBranch, query, limit)
		},
		func() ([]Ref, bool, error) {
			return gitlabRefList(ctx, client, b, scheme, host, project, auth, "tags", RefKindTag, query, limit)
		},
	)
	if err != nil {
		return nil, false, err
	}
	capped, more := capRefs(refs, limit)
	return capped, truncated || more, nil
}

// gitlabRefList 取 GitLab 的 branches 或 tags，search 参数在上游做关键字过滤。
func gitlabRefList(ctx context.Context, client *http.Client, b providerBase, scheme, host, project string, auth *Auth, path, typ, query string, limit int) ([]Ref, bool, error) {
	q := url.Values{}
	q.Set("per_page", strconv.Itoa(limit))
	if query != "" {
		q.Set("search", query)
	}
	apiURL := fmt.Sprintf("%s://%s/api/v4/projects/%s/repository/%s?%s", scheme, host, project, path, q.Encode())
	resp, err := b.open(ctx, client, "gitlab", apiURL, auth)
	if err != nil {
		return nil, false, err
	}
	var payload []struct {
		Name string `json:"name"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
	resp.Body.Close()
	if decodeErr != nil {
		return nil, false, fmt.Errorf("gitlab %s json decode failed: %v", path, decodeErr)
	}
	refs := make([]Ref, 0, len(payload))
	for _, item := range payload {
		refs = append(refs, Ref{Name: item.Name, Type: typ})
	}
	return refs, len(payload) >= limit, nil
}

func searchGiteaRefs(ctx context.Context, client *http.Client, b providerBase, gitURL string, auth *Auth, query string, limit int) ([]Ref, bool, error) {
	scheme, host, owner, repo, err := parseGiteaURL(gitURL)
	if err != nil {
		return nil, false, err
	}
	repoAPI := fmt.Sprintf("%s://%s/api/v1/repos/%s/%s", scheme, host, owner, repo)

	// 空关键字只给分支，与旧的下拉行为一致。
	if query == "" {
		refs, truncated, err := giteaBranchRefs(ctx, client, b, repoAPI, auth, "", limit)
		if err != nil {
			return nil, false, err
		}
		capped, more := capRefs(refs, limit)
		return capped, truncated || more, nil
	}

	// 分支与 tag 两路并发：分支一次请求，tag 要分页扫。
	refs, truncated, err := runRefQueries(
		func() ([]Ref, bool, error) {
			return giteaBranchRefs(ctx, client, b, repoAPI, auth, query, limit)
		},
		func() ([]Ref, bool, error) {
			return giteaTagRefs(ctx, client, b, repoAPI, auth, query)
		},
	)
	if err != nil {
		return nil, false, err
	}
	capped, more := capRefs(refs, limit)
	return capped, truncated || more, nil
}

// giteaBranchRefs 走 branches 的 q 参数，上游直接过滤。
func giteaBranchRefs(ctx context.Context, client *http.Client, b providerBase, repoAPI string, auth *Auth, query string, limit int) ([]Ref, bool, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if query != "" {
		q.Set("q", query)
	}
	names, err := fetchGiteaNames(ctx, client, b, repoAPI+"/branches?"+q.Encode(), auth)
	if err != nil {
		return nil, false, err
	}
	refs := make([]Ref, 0, len(names))
	for _, n := range names {
		refs = append(refs, Ref{Name: n, Type: RefKindBranch})
	}
	return refs, len(names) >= limit, nil
}

// giteaTagRefs 扫有限几页 tag 后在服务端按关键字过滤（上游 tags 接口不认 q）。
func giteaTagRefs(ctx context.Context, client *http.Client, b providerBase, repoAPI string, auth *Auth, query string) ([]Ref, bool, error) {
	needle := strings.ToLower(query)
	var refs []Ref
	truncated := false
	for page := 1; page <= giteaTagScanPages; page++ {
		pq := url.Values{}
		pq.Set("limit", strconv.Itoa(giteaTagScanPageSize))
		pq.Set("page", strconv.Itoa(page))
		names, err := fetchGiteaNames(ctx, client, b, repoAPI+"/tags?"+pq.Encode(), auth)
		if err != nil {
			return nil, false, err
		}
		for _, n := range names {
			if strings.Contains(strings.ToLower(n), needle) {
				refs = append(refs, Ref{Name: n, Type: RefKindTag})
			}
		}
		if len(names) < giteaTagScanPageSize {
			break // 已到最后一页
		}
		if page == giteaTagScanPages {
			truncated = true // 还有没扫到的 tag
		}
	}
	return refs, truncated, nil
}

// fetchGiteaNames 取 Gitea 的 branches/tags 名称列表（两接口的返回结构一致）。
func fetchGiteaNames(ctx context.Context, client *http.Client, b providerBase, apiURL string, auth *Auth) ([]string, error) {
	resp, err := b.open(ctx, client, "gitea", apiURL, auth)
	if err != nil {
		return nil, err
	}
	var payload []struct {
		Name string `json:"name"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
	resp.Body.Close()
	if decodeErr != nil {
		return nil, fmt.Errorf("gitea refs json decode failed: %v", decodeErr)
	}
	names := make([]string, 0, len(payload))
	for _, item := range payload {
		names = append(names, item.Name)
	}
	return names, nil
}

// gitHubBranchNames 拉取 GitHub 分支名列表（分页首屏即 limit）。
func gitHubBranchNames(ctx context.Context, client *http.Client, b providerBase, providerName, apiURL string, auth *Auth) ([]string, error) {
	resp, err := b.open(ctx, client, providerName, apiURL, auth)
	if err != nil {
		return nil, err
	}
	var payload []struct {
		Name string `json:"name"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
	resp.Body.Close()
	if decodeErr != nil {
		return nil, fmt.Errorf("github branches json decode failed: %v", decodeErr)
	}
	names := make([]string, 0, len(payload))
	for _, item := range payload {
		names = append(names, item.Name)
	}
	return names, nil
}

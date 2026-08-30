package server

import (
	"strings"
	"testing"

	"github.com/fimreal/gitweb/web"
)

// pageData 逐个列出 templates/ 下每个页面模板需要的字段。
//
// 这里绕开 HTTP 直接执行模板：viewer 与 embed 两条路由要联网取仓库内容才能渲染，
// 路由级测试覆盖不到它们，而「head / controls 片段抽漏一页」「片段名写错」这两类
// 恰好是模板骨架重构最容易留下的洞。片段名写错会在执行期报 no such template，
// 漏抽片段则表现为下面某个 want 断言失败。
func TestEveryPageIncludesSharedHead(t *testing.T) {
	r, _ := newTestServer(t)
	tpl, err := web.LoadTemplates()
	if err != nil {
		t.Fatalf("load templates: %v", err)
	}

	cases := []struct {
		name string
		data map[string]any
	}{
		{"index.html", map[string]any{}},
		{"error.html", map[string]any{"Code": 404, "Message": "not found"}},
		{"login.html", map[string]any{"Redirect": "/demo/", "Error": false, "Locked": false, "LockSec": 0}},
		{"embed.html", map[string]any{
			"Filename": "README.md", "RepoName": "demo", "PathID": "demo", "Ref": "main",
			"Content": "<p>hello</p>",
		}},
		{"viewer.html", map[string]any{
			"RepoName": "demo", "PathID": "demo", "Ref": "main", "GitURL": "https://github.com/o/demo.git",
			"Provider": "github", "Nonce": "n0nc3", "Hidden": false, "HasPresetAuth": false,
			"CreatedAt": "", "Views": 0,
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			if err := tpl.ExecuteTemplate(&sb, tc.name, tc.data); err != nil {
				t.Fatalf("execute %s: %v", tc.name, err)
			}
			body := sb.String()
			for _, want := range []string{
				`<meta charset="UTF-8">`,
				`viewport-fit=cover`,
				`/static/css/tokens.css`,
				`/static/js/theme.js`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("%s 缺少 head 片段产物 %q", tc.name, want)
				}
			}
			// 片段只出现一次：登录页曾既引片段又留一份 theme.js，重复加载会让主题逻辑跑两遍。
			if n := strings.Count(body, `/static/js/theme.js`); n != 1 {
				t.Errorf("%s 加载 theme.js %d 次，应为 1 次", tc.name, n)
			}
			switch tc.name {
			case "index.html", "login.html":
				for _, want := range []string{`id="theme-toggle"`, `id="lang-toggle"`} {
					if !strings.Contains(body, want) {
						t.Errorf("%s 缺少 controls 片段产物 %q", tc.name, want)
					}
				}
			default:
				if strings.Contains(body, `id="lang-toggle"`) {
					t.Errorf("%s 不该出现 controls 片段", tc.name)
				}
			}
			assertAssetsResolve(t, r, body)
		})
	}
}

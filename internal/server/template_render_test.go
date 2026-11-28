package server

import (
	"regexp"
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
			if tc.name == "viewer.html" {
				// 「切换 ref」支持边输边筛：输入框与结果列表必须在弹窗内成对出现
				for _, want := range []string{`id="ref-results"`, `aria-controls="ref-results"`} {
					if !strings.Contains(body, want) {
						t.Errorf("viewer.html 缺少 ref 搜索下拉 %q", want)
					}
				}
			}
			switch tc.name {
			case "index.html":
				// 右上角要能一键跳到 GitHub 仓库（与主题/语言按钮同一排）
				for _, want := range []string{`href="https://github.com/fimreal/gitweb"`, `class="icon-btn github-link"`} {
					if !strings.Contains(body, want) {
						t.Errorf("index.html 缺少 GitHub 仓库入口 %q", want)
					}
				}
			case "viewer.html":
				// 文件目录的搜索入口必须挂在浮窗内，且带标题栏开关（折叠/展开）
				for _, want := range []string{`id="tree-search"`, `id="tree-search-clear"`, `id="tree-search-toggle"`} {
					if !strings.Contains(body, want) {
						t.Errorf("viewer.html 缺少文件搜索控件 %q", want)
					}
				}
				// 默认态一律收起：分档默认态已作废，别再让它悄悄变回展开。
				if !strings.Contains(body, `id="floating-tree-search" data-open="false"`) {
					t.Error("搜索框应默认收起（data-open=false）")
				}
				if !strings.Contains(body, `aria-controls="floating-tree-search" aria-expanded="false"`) {
					t.Error("搜索开关的初始 aria-expanded 应为 false")
				}
				// 搜索框的语义必须是「搜索框」而不是凭据字段：否则页面里一旦有密码输入，
				// Chrome 的密码管理器就会对着搜索框弹已存账号补全。
				for _, want := range []string{`type="search"`, `name="q"`, `autocomplete="off"`} {
					if !strings.Contains(inputTag(t, body, "tree-search"), want) {
						t.Errorf("tree-search 缺少 %q（会被 Chrome 当成登录表单字段）", want)
					}
				}
				for _, want := range []string{`type="search"`, `autocomplete="off"`} {
					if !strings.Contains(inputTag(t, body, "ref-input"), want) {
						t.Errorf("ref-input 缺少 %q（会被 Chrome 当成登录表单字段）", want)
					}
				}
				// 仓库凭据弹窗不是本站登录：password 字段必须标 new-password，避免密码管理器接手。
				for _, id := range []string{"auth-token", "auth-password"} {
					tag := inputTag(t, body, id)
					if strings.Contains(tag, `autocomplete="current-password"`) {
						t.Errorf("%s 不该标 current-password（会让整页被判定成登录表单）", id)
					}
					if !strings.Contains(tag, `autocomplete="new-password"`) {
						t.Errorf("%s 缺少 autocomplete=new-password", id)
					}
				}
			case "login.html":
				// 真正的站点密码框要保留正常填充能力，别被这轮的改动带坏。
				if tag := inputTag(t, body, "password"); !strings.Contains(tag, `autocomplete="current-password"`) {
					t.Errorf("login.html 的密码框必须保留 autocomplete=current-password，实际 = %s", tag)
				}
			}
			assertAssetsResolve(t, r, body)
		})
	}
}

// inputTag 取出指定 id 的 <input> 标签原文（含跨行属性），用于断言输入框语义。
func inputTag(t *testing.T, body, id string) string {
	t.Helper()
	re := regexp.MustCompile(`<input[^>]*id="` + regexp.QuoteMeta(id) + `"[^>]*>`)
	tag := re.FindString(body)
	if tag == "" {
		t.Fatalf("找不到 id=%q 的输入框", id)
	}
	return tag
}

package web

import (
	"io/fs"
	"strings"
	"testing"
)

// TestTemplateNamesDoNotCollide 守住 LoadTemplates 注释里那条约束：模板名取自文件名、
// 不含目录，所以 templates/ 与 templates/partials/ 下重名不会报错，只会让后解析的
// 静默覆盖先解析的。将来新增一个 partial 恰好撞上某个页面名，页面就会凭空换成片段内容。
func TestTemplateNamesDoNotCollide(t *testing.T) {
	seen := map[string]string{}
	err := fs.WalkDir(Templates, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := path[len("templates/"):]
		if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
			base = base[idx+1:]
		}
		if prev, ok := seen[base]; ok {
			t.Errorf("模板名 %s 重复：%s 与 %s 会互相覆盖", base, prev, path)
		}
		seen[base] = path
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
	if len(seen) < 5 {
		t.Fatalf("只发现 %d 个模板，多半是 glob 或目录改坏了", len(seen))
	}
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// TestLoadTemplatesFindsPartials 确认片段与页面在同一套模板里：装配漏掉 partials 目录时
// ParseFS 仍然成功，页面要到执行期才报 no such template，那时错的是每个请求。
func TestLoadTemplatesFindsPartials(t *testing.T) {
	tpl, err := LoadTemplates()
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	for _, name := range []string{"head", "controls", "index.html", "login.html", "viewer.html", "embed.html", "error.html"} {
		if tpl.Lookup(name) == nil {
			t.Errorf("模板 %s 未装配", name)
		}
	}
}

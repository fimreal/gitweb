package server

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/fimreal/gitweb/web"
)

// 设计约定的守门测试。这个仓库零构建、不引 npm lint，所以「设计变量只有一处真源」这条
// 约定改用 Go 正则钉死：色号、玻璃模糊、按主题双写只允许出现在 tokens.css，其余样式与
// 模板内联样式一律只消费变量。

var (
	reBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reStyleBlock   = regexp.MustCompile(`(?s)<style[^>]*>(.*?)</style>`)
	reHexColor     = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	reRGBColor     = regexp.MustCompile(`\brgba?\(`)
	reBackdrop     = regexp.MustCompile(`backdrop-filter`)
	reGlassToken   = regexp.MustCompile(`--glass-`)
	reDualTheme    = regexp.MustCompile(`:root:not\(`)
	reVarDefine    = regexp.MustCompile(`--[a-zA-Z0-9-]+\s*:`)
	reVarUse       = regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9-]+)`)
	reTokenAssign  = regexp.MustCompile(`(--[a-zA-Z0-9-]+)\s*:\s*([^;]+);`)
	reColorValue   = regexp.MustCompile(`^#[0-9a-fA-F]|^rgba?\(`)
	rePxFont       = regexp.MustCompile(`font-size: *[0-9.]+px`)
)

// styleSources 读出真正打进二进制的样式表与模板内联样式，键是可读文件名。
// 读嵌入文件系统而不是磁盘，保证守的就是线上产物。
func styleSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	css, err := web.Static.ReadDir("static/css")
	if err != nil {
		t.Fatalf("read embedded css dir: %v", err)
	}
	for _, e := range css {
		if e.IsDir() {
			continue
		}
		data, err := web.Static.ReadFile("static/css/" + e.Name())
		if err != nil {
			t.Fatalf("read css/%s: %v", e.Name(), err)
		}
		out[e.Name()] = stripComments(string(data))
	}
	err = fs.WalkDir(web.Templates, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".html") {
			return nil
		}
		data, err := web.Templates.ReadFile(path)
		if err != nil {
			return err
		}
		var all []string
		for _, block := range reStyleBlock.FindAllStringSubmatch(stripComments(string(data)), -1) {
			all = append(all, block[1])
		}
		if len(all) > 0 {
			out[path] = strings.Join(all, "\n")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded templates: %v", err)
	}
	return out
}

func stripComments(s string) string { return reBlockComment.ReplaceAllString(s, "") }

func tokensBlock(t *testing.T, src, start string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("tokens.css has no %q block", start)
	}
	rest := src[i+len(start):]
	j := strings.Index(rest, "\n}")
	if j < 0 {
		t.Fatalf("tokens.css block %q is unterminated", start)
	}
	return rest[:j]
}

// TestNoRawColorOutsideTokens 是「一处真源」的主判据：tokens.css 之外不得出现十六进制或
// rgb() 色号，也不得再定义设计变量。
func TestNoRawColorOutsideTokens(t *testing.T) {
	for name, src := range styleSources(t) {
		if name == "tokens.css" {
			continue
		}
		for _, p := range []*regexp.Regexp{reHexColor, reRGBColor, reVarDefine} {
			if m := p.FindString(src); m != "" {
				t.Errorf("%s: 出现被禁止的 %q —— 色号与变量定义只能写在 tokens.css", name, strings.TrimSpace(m))
			}
		}
	}
}

// TestNoGlassmorphism 连变量名一起挡：--glass-* 已从 tokens.css 删除，残留引用不会报错，
// 只会静默变成透明底，肉眼看不出来。
func TestNoGlassmorphism(t *testing.T) {
	for name, src := range styleSources(t) {
		for _, p := range []*regexp.Regexp{reBackdrop, reGlassToken} {
			if m := p.FindString(src); m != "" {
				t.Errorf("%s: 仍有玻璃残留 %q", name, m)
			}
		}
	}
}

// TestNoPerThemeDoubleWrite 挡住 [data-theme="dark"] X, :root:not([data-theme="light"]) X
// 这种双写：theme.js 在首帧前就把 data-theme 落成 light 或 dark，第二条选择器永远不生效，
// 留着只会让人误以为关掉 JS 也有深色兜底。
func TestNoPerThemeDoubleWrite(t *testing.T) {
	for name, src := range styleSources(t) {
		if m := reDualTheme.FindString(src); m != "" {
			t.Errorf("%s: 存在按主题双写 %q，data-theme 由 theme.js 显式写入，删掉 :root:not(...) 那一条", name, m)
		}
	}
}

// TestNoAbsoluteFontSizes 守住「字号默认 14」这条：根字号只在 tokens.css 声明一次
// （font-size: 87.5% = 14px），其余一律用 rem。写死 px 的那条规则不会报错，只是静默
// 停在旧刻度上——整页缩放与「改一处根字号」对它都不生效，肉眼又看不出差别。
func TestNoAbsoluteFontSizes(t *testing.T) {
	for name, src := range styleSources(t) {
		if name == "tokens.css" {
			continue
		}
		if m := rePxFont.FindString(src); m != "" {
			t.Errorf("%s: 出现 %q —— 根字号在 tokens.css，这里应写 rem", name, strings.TrimSpace(m))
		}
	}
}

// TestEveryConsumedTokenIsDefined 抓变量名写错、或变量只在某个页面定义这两种情况。
// 两者的表现都是那一格样式静默失效，很难归因。
func TestEveryConsumedTokenIsDefined(t *testing.T) {
	sources := styleSources(t)
	defined := map[string]bool{}
	for _, m := range reTokenAssign.FindAllStringSubmatch(sources["tokens.css"], -1) {
		defined[m[1]] = true
	}
	if len(defined) == 0 {
		t.Fatal("tokens.css 里一个变量都没有，这个门本身失效了")
	}
	for name, src := range sources {
		if name == "tokens.css" {
			continue
		}
		for _, m := range reVarUse.FindAllStringSubmatch(src, -1) {
			if !defined[m[1]] {
				t.Errorf("%s 消费了未定义的 %s", name, m[1])
			}
		}
	}
}

// TestColorTokensHaveDarkValues 要求凡是浅色值长成颜色的变量，暗色块必须给出自己的一份。
// 圆角、字体栈、缓动这类与主题无关的量不在检查范围内，所以不需要手工维护豁免清单。
func TestColorTokensHaveDarkValues(t *testing.T) {
	src := styleSources(t)["tokens.css"]
	names := func(block string) map[string]string {
		out := map[string]string{}
		for _, m := range reTokenAssign.FindAllStringSubmatch(block, -1) {
			out[m[1]] = strings.TrimSpace(m[2])
		}
		return out
	}
	light := names(tokensBlock(t, src, ":root {"))
	dark := names(tokensBlock(t, src, "[data-theme=\"dark\"] {"))

	if len(light) == 0 || len(dark) == 0 {
		t.Fatal("tokens.css 的浅色或暗色块为空")
	}
	for name, value := range light {
		if !reColorValue.MatchString(value) {
			continue
		}
		if _, ok := dark[name]; !ok {
			t.Errorf("%s 是颜色（浅色值 %s）但暗色块没有对应取值", name, value)
		}
	}
	for name := range dark {
		if _, ok := light[name]; !ok {
			t.Errorf("%s 只在暗色块定义，多半是拼错了变量名", name)
		}
	}
}

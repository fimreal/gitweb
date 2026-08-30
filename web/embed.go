// Package web 嵌入 templates + static 目录到二进制中。
package web

import (
	"embed"
	"html/template"
)

//go:embed templates
var Templates embed.FS

//go:embed static
var Static embed.FS

// templateFuncs 只有一项：embed 页把已渲染的 HTML 片段注入正文，需要告诉模板引擎它可信。
var templateFuncs = template.FuncMap{
	"safehtml": func(s string) template.HTML { return template.HTML(s) },
}

// LoadTemplates 装配页面与公共片段。
// 模板名取自文件名（不含目录），所以 templates/ 与 templates/partials/ 下的文件名不得重复：
// 重名不报错，只会被后解析的静默覆盖。装配只留这一处，免得各调用方各写一份 glob 后漂移。
func LoadTemplates() (*template.Template, error) {
	return template.New("").Funcs(templateFuncs).ParseFS(
		Templates,
		"templates/*.html",
		"templates/partials/*.html",
	)
}

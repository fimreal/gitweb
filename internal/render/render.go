package render

import (
	"bytes"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

type Renderer struct {
	md goldmark.Markdown
}

func New() *Renderer {
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.Table, extension.Strikethrough),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithUnsafe()),
	)
	return &Renderer{md: md}
}

func (r *Renderer) ShouldRender(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	
	// 文档和标记语言
	if ext == ".md" || ext == ".markdown" || ext == ".html" || ext == ".htm" || ext == ".txt" {
		return true
	}
	
	// 所有代码文件和配置文件都渲染
	renderExts := map[string]bool{
		// 脚本
		".sh": true, ".bash": true, ".zsh": true, ".fish": true,
		".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".jsx": true, ".tsx": true,
		".py": true, ".rb": true, ".pl": true, ".php": true, ".lua": true, ".vim": true,
		
		// 编程语言
		".c": true, ".h": true, ".cpp": true, ".cc": true, ".cxx": true, ".hpp": true,
		".go": true, ".rs": true, ".java": true, ".kt": true, ".scala": true,
		".cs": true, ".vb": true, ".fs": true, ".swift": true, ".m": true, ".mm": true,
		
		// Web
		".css": true, ".scss": true, ".sass": true, ".less": true,
		".vue": true, ".svelte": true,
		
		// 配置
		".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
		".conf": true, ".cfg": true, ".xml": true, ".env": true,
		
		// 其他
		".sql": true, ".log": true, ".csv": true, ".tsv": true,
	}
	
	if renderExts[ext] {
		return true
	}
	
	// 无扩展名的常见文本文件
	filename := filepath.Base(path)
	textFiles := map[string]bool{
		"README": true, "LICENSE": true, "CHANGELOG": true, "CONTRIBUTING": true,
		"AUTHORS": true, "COPYING": true, "INSTALL": true, "TODO": true,
		"Makefile": true, "Dockerfile": true, "Vagrantfile": true, "Gemfile": true,
		"Rakefile": true, "Procfile": true, ".gitignore": true, ".dockerignore": true,
	}
	
	return textFiles[filename]
}

func (r *Renderer) Render(path string, content []byte, page int) (string, int, error) {
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".md", ".markdown":
		return r.renderMarkdown(content, page)
	case ".html", ".htm":
		return r.renderHTML(content, page)
	default:
		return r.renderPlainText(content, page)
	}
}

func (r *Renderer) renderMarkdown(content []byte, page int) (string, int, error) {
	var buf bytes.Buffer
	if err := r.md.Convert(content, &buf); err != nil {
		return "", 0, err
	}
	return buf.String(), 1, nil
}

func (r *Renderer) renderHTML(content []byte, page int) (string, int, error) {
	return string(content), 1, nil
}

func (r *Renderer) renderPlainText(content []byte, page int) (string, int, error) {
	escaped := strings.ReplaceAll(string(content), "<", "&lt;")
	escaped = strings.ReplaceAll(escaped, ">", "&gt;")
	return "<pre><code>" + escaped + "</code></pre>", 1, nil
}

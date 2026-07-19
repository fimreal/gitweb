package render

import (
	"bytes"
	"encoding/csv"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// Kind 描述一个文件被渲染的方式。
type Kind int

const (
	KindHTML      Kind = iota // .html/.htm：原样透传，前端用 iframe sandbox 承载
	KindMarkdown             // .md/.markdown：goldmark 转 HTML 片段
	KindText                 // 纯文本/代码/配置：转义后放 <pre><code>
	KindImage                // 图片：透传原始字节，前端 <img>
	KindBinary               // 其它二进制：透传，前端提示下载
	KindUnsupported          // 不支持预览
)

// renderableExts 是全项目唯一的「可渲染文本扩展名」权威表。
// provider.FilterRenderableFiles 复用此表，避免三份表不同步。
var renderableExts = map[string]bool{
	// 文档/标记
	".md": true, ".markdown": true, ".mdx": true,
	".txt": true, ".text": true,
	".rst": true, ".asciidoc": true, ".adoc": true,
	".html": true, ".htm": true, ".xhtml": true,
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
	// 数据/其他
	".sql": true, ".log": true, ".csv": true, ".tsv": true,
}

// imageExts 图片扩展名（透传，前端 <img> 展示）。
var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".bmp": true, ".ico": true, ".webp": true, ".svg": true,
}

// textFiles 无扩展名的常见文本文件名。
var textFiles = map[string]bool{
	"README": true, "LICENSE": true, "CHANGELOG": true, "CONTRIBUTING": true,
	"AUTHORS": true, "COPYING": true, "INSTALL": true, "TODO": true,
	"Makefile": true, "Dockerfile": true, "Vagrantfile": true, "Gemfile": true,
	"Rakefile": true, "Procfile": true, ".gitignore": true, ".dockerignore": true,
}

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

// IsRenderable 报告该文件是否可在 viewer 中预览（文本/图片）。
// 供 provider.FilterRenderableFiles 与 server 共用。
func (r *Renderer) IsRenderable(path string) bool {
	switch r.Kind(path) {
	case KindUnsupported, KindBinary:
		return false
	default:
		return true
	}
}

// Kind 返回文件的渲染分派。这是扩展名判定的唯一来源。
func (r *Renderer) Kind(path string) Kind {
	ext := strings.ToLower(filepath.Ext(path))
	if ext != "" {
		switch ext {
		case ".html", ".htm", ".xhtml":
			return KindHTML
		case ".md", ".markdown":
			return KindMarkdown
		}
		if imageExts[ext] {
			return KindImage
		}
		if renderableExts[ext] {
			return KindText
		}
		// 有扩展名但不在可渲染表 → 二进制
		return KindBinary
	}
	// 无扩展名：按文件名判断
	if textFiles[filepath.Base(path)] {
		return KindText
	}
	// 未知无扩展名文件，保守当文本（git 仓库里大多是文本）
	return KindText
}

// Render 把文件字节渲染为 HTML 片段（不含外层骨架，由前端注入容器）。
// page 参数保留以便未来服务端分页，当前前端分页，始终渲染全文。
func (r *Renderer) Render(path string, content []byte) (string, error) {
	// CSV/TSV：转成 Markdown 表格语法后走 goldmark，输出 <table>。
	// 解析失败时回退到纯文本，保证源码可读。
	switch strings.ToLower(filepath.Ext(path)) {
	case ".csv":
		return r.renderCSV(content, false)
	case ".tsv":
		return r.renderCSV(content, true)
	}
	switch r.Kind(path) {
	case KindHTML:
		return string(content), nil
	case KindMarkdown:
		return r.renderMarkdown(content)
	default:
		return r.renderPlainText(content), nil
	}
}

func (r *Renderer) renderMarkdown(content []byte) (string, error) {
	var buf bytes.Buffer
	if err := r.md.Convert(content, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// renderCSV 把 CSV/TSV 转成 Markdown 表格语法，再走 goldmark 输出 <table>。
// 解析失败（非法 CSV）或空内容时回退到纯文本展示，保证源码可读。
func (r *Renderer) renderCSV(content []byte, tabSeparated bool) (string, error) {
	reader := csv.NewReader(bytes.NewReader(content))
	if tabSeparated {
		reader.Comma = '\t'
	}
	reader.FieldsPerRecord = -1 // 允许行间列数不一致，按原始顺序展示
	records, err := reader.ReadAll()
	if err != nil || len(records) == 0 {
		return r.renderPlainText(content), nil
	}
	var b strings.Builder
	header := records[0]
	sep := make([]string, len(header))
	for i := range sep {
		sep[i] = "---"
	}
	b.WriteString("| " + strings.Join(escapeMarkdownRow(header), " | ") + " |\n")
	b.WriteString("| " + strings.Join(sep, " | ") + " |\n")
	for _, row := range records[1:] {
		b.WriteString("| " + strings.Join(escapeMarkdownRow(row), " | ") + " |\n")
	}
	return r.renderMarkdown([]byte(b.String()))
}

// escapeMarkdownRow 转义单元格内容以适配 Markdown 表格语法：
// - "|" → "\|"（避免被当作列分隔符）
// - 行内换行折叠为空格（Markdown 表格单元不支持多行）
func escapeMarkdownRow(fields []string) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		f = strings.ReplaceAll(f, "\r\n", " ")
		f = strings.ReplaceAll(f, "\n", " ")
		f = strings.ReplaceAll(f, "|", "\\|")
		out[i] = f
	}
	return out
}

func (r *Renderer) renderPlainText(content []byte) string {
	escaped := strings.ReplaceAll(string(content), "<", "&lt;")
	escaped = strings.ReplaceAll(escaped, ">", "&gt;")
	return "<pre><code>" + escaped + "</code></pre>"
}

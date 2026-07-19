package render

import (
	"strings"
	"testing"
)

func TestKind(t *testing.T) {
	r := New()
	cases := []struct {
		path string
		want Kind
	}{
		{"a.md", KindMarkdown},
		{"a.markdown", KindMarkdown},
		{"a.html", KindHTML},
		{"a.htm", KindHTML},
		{"a.txt", KindText},
		{"a.go", KindText},
		{"a.json", KindText},
		{"a.png", KindImage},
		{"a.svg", KindImage},
		{"a.exe", KindBinary},
		{"a.zip", KindBinary},
		{"Makefile", KindText},
		{"README", KindText},
	}
	for _, c := range cases {
		if got := r.Kind(c.path); got != c.want {
			t.Errorf("Kind(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIsRenderable(t *testing.T) {
	r := New()
	renderable := []string{"a.md", "a.html", "a.txt", "a.go", "a.png", "Makefile"}
	notRenderable := []string{"a.exe", "a.zip", "a.bin"}
	for _, p := range renderable {
		if !r.IsRenderable(p) {
			t.Errorf("IsRenderable(%q) = false, want true", p)
		}
	}
	for _, p := range notRenderable {
		if r.IsRenderable(p) {
			t.Errorf("IsRenderable(%q) = true, want false", p)
		}
	}
}

func TestRenderMarkdown(t *testing.T) {
	r := New()
	out, err := r.Render("a.md", []byte("# Title\n\nhello **world**"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "<h1") || !strings.Contains(out, "<strong>world</strong>") {
		t.Errorf("markdown output missing expected tags: %s", out)
	}
}

func TestRenderHTMLPassthrough(t *testing.T) {
	r := New()
	in := "<!DOCTYPE html><p>hi</p>"
	out, err := r.Render("a.html", []byte(in))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if out != in {
		t.Errorf("html should pass through, got %q", out)
	}
}

func TestRenderPlainTextEscapes(t *testing.T) {
	r := New()
	out, err := r.Render("a.txt", []byte("<script>alert(1)</script>"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(out, "<script>") {
		t.Errorf("plain text must escape <script>, got %q", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("expected escaped script, got %q", out)
	}
	if !strings.HasPrefix(out, "<pre><code>") {
		t.Errorf("plain text should wrap in pre/code, got %q", out)
	}
}

func TestRenderCSV(t *testing.T) {
	r := New()
	in := []byte("name,age\nAlice,30\nBob,25\n")
	out, err := r.Render("a.csv", in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "<table") {
		t.Errorf("csv should render as table, got %q", out)
	}
	if !strings.Contains(out, "<th") || !strings.Contains(out, "name") {
		t.Errorf("csv header missing, got %q", out)
	}
	if !strings.Contains(out, "Alice") || !strings.Contains(out, "30") {
		t.Errorf("csv row data missing, got %q", out)
	}
	if strings.HasPrefix(out, "<pre><code>") {
		t.Errorf("csv should not fall back to plain text, got %q", out)
	}
}

func TestRenderCSVQuotedFields(t *testing.T) {
	r := New()
	// 含逗号、引号、换行的字段
	in := []byte("id,note\n1,\"hello, world\"\n2,\"line1\nline2\"\n")
	out, err := r.Render("a.csv", in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "<table") {
		t.Errorf("quoted csv should render as table, got %q", out)
	}
	// 含换行的字段应被折叠为空格（markdown 单元格不支持多行）
	if strings.Contains(out, "line1\nline2") {
		t.Errorf("newline in cell should collapse to space, got %q", out)
	}
}

func TestRenderCSVEscapedPipe(t *testing.T) {
	r := New()
	in := []byte("col\na|b\n")
	out, err := r.Render("a.csv", in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "<table") {
		t.Errorf("csv with pipe should render as table, got %q", out)
	}
	if !strings.Contains(out, "a|b") {
		t.Errorf("pipe char should appear in cell, got %q", out)
	}
}

func TestRenderTSV(t *testing.T) {
	r := New()
	in := []byte("name\tage\nAlice\t30\n")
	out, err := r.Render("a.tsv", in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "<table") {
		t.Errorf("tsv should render as table, got %q", out)
	}
	if !strings.Contains(out, "Alice") {
		t.Errorf("tsv row data missing, got %q", out)
	}
}

func TestRenderCSVMalformedFallsBackToText(t *testing.T) {
	r := New()
	// 非法 CSV：未闭合的引号字段
	in := []byte("a,b\n\"unclosed\n")
	out, err := r.Render("a.csv", in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// 回退到纯文本：用 <pre><code> 包裹原内容
	if !strings.HasPrefix(out, "<pre><code>") {
		t.Errorf("malformed csv should fall back to plain text, got %q", out)
	}
	if !strings.Contains(out, "unclosed") {
		t.Errorf("fallback should preserve original content, got %q", out)
	}
}

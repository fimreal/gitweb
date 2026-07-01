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

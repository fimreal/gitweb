package registry

import (
	"strings"
	"testing"
)

func TestRegisterCustomPathID(t *testing.T) {
	r := New()
	site, err := r.Register("https://github.com/o/repo", "myid", "main", "github", nil, false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if site.PathID != "myid" {
		t.Errorf("PathID = %q, want myid", site.PathID)
	}
	if site.Ref != "main" {
		t.Errorf("Ref = %q, want main", site.Ref)
	}
}

func TestRegisterDuplicatePathID(t *testing.T) {
	r := New()
	if _, err := r.Register("https://github.com/o/repo", "dup", "main", "github", nil, false); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, err := r.Register("https://github.com/o/other", "dup", "main", "github", nil, false)
	if err == nil {
		t.Fatal("expected duplicate error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected duplicate message, got %v", err)
	}
}

func TestRegisterAutoPathID(t *testing.T) {
	r := New()
	site, err := r.Register("https://github.com/o/repo", "", "main", "github", nil, false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(site.PathID) != 8 {
		t.Errorf("auto PathID length = %d, want 8", len(site.PathID))
	}
	if !IsValidPathID(site.PathID) {
		t.Errorf("auto PathID %q failed validation", site.PathID)
	}
}

func TestRegisterInvalidPathID(t *testing.T) {
	r := New()
	for _, id := range []string{"with space", "has/slash", "ab cd", "中文"} {
		if _, err := r.Register("https://github.com/o/repo", id, "main", "github", nil, false); err == nil {
			t.Errorf("expected error for invalid pathid %q, got nil", id)
		}
	}
}

func TestRegisterReservedPathID(t *testing.T) {
	r := New()
	for _, id := range []string{"api", "static", "healthz"} {
		if _, err := r.Register("https://github.com/o/repo", id, "main", "github", nil, false); err == nil {
			t.Errorf("expected error for reserved pathid %q, got nil", id)
		}
	}
}

func TestRegisterRefDefault(t *testing.T) {
	r := New()
	site, err := r.Register("https://github.com/o/repo", "x", "", "github", nil, false)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if site.Ref != "main" {
		t.Errorf("default Ref = %q, want main", site.Ref)
	}
}

func TestGetNotFound(t *testing.T) {
	r := New()
	if _, err := r.Get("nope"); err == nil {
		t.Fatal("expected not found, got nil")
	}
}

func TestRemove(t *testing.T) {
	r := New()
	if _, err := r.Register("https://github.com/o/repo", "rm", "main", "github", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("rm"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := r.Get("rm"); err == nil {
		t.Fatal("expected not found after remove")
	}
	if err := r.Remove("rm"); err == nil {
		t.Fatal("expected error removing non-existent")
	}
}

func TestIsValidPathID(t *testing.T) {
	good := []string{"a", "ab12", "my_id", "my-id", "AaBb09_-", strings.Repeat("a", 32)}
	bad := []string{"", "a b", "a/b", "a.b", "中文", strings.Repeat("a", 33)}
	for _, id := range good {
		if !IsValidPathID(id) {
			t.Errorf("IsValidPathID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if IsValidPathID(id) {
			t.Errorf("IsValidPathID(%q) = true, want false", id)
		}
	}
}

func TestIsValidRef(t *testing.T) {
	good := []string{
		"main", "master", "HEAD", "develop",
		"feature/x", "release/2026/08", "refs/heads/main", "refs/tags/v1.2.3",
		"v1.2.3", "v1.0.0-rc.1", "my_branch", "my-branch", "build-32",
		"3e2f1a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e4f", // 完整 commit SHA
		"3e2f1a8", // 缩写 SHA
	}
	bad := []string{
		"",
		`<img src=x onerror=document.title="XSS-PWNED">`, // 实测过的注册 payload
		"<script>alert(1)</script>",
		"a b", "a\tb", "a\nb",
		"a~b", "a^b", "a:b", "a?b", "a*b", "a[b", `a\b`, "a@{b", "a'b", `a"b`, "a&b",
		"a..b",
		"/leading", "trailing/", "double//slash",
		".hidden", "sub/.hidden", "trailing.", "sub/trailing.",
		"a.lock", "refs/heads/x.lock",
		"中文",
		"ref/" + strings.Repeat("a", 300),
	}
	for _, ref := range good {
		if !IsValidRef(ref) {
			t.Errorf("IsValidRef(%q) = false, want true", ref)
		}
	}
	for _, ref := range bad {
		if IsValidRef(ref) {
			t.Errorf("IsValidRef(%q) = true, want false", ref)
		}
	}
}

// TestRegisterRejectsMaliciousRef 是 /api/sites 存储型 XSS 的回归防线：
// ref 曾经未经任何校验直接落库，并经 GET /api/sites 回显到前端 innerHTML。
func TestRegisterRejectsMaliciousRef(t *testing.T) {
	r := New()
	payload := `<img src=x onerror=alert(1)>`
	if _, err := r.Register("https://github.com/o/repo", "evil", payload, "github", nil, false); err == nil {
		t.Fatalf("Register accepted malicious ref %q", payload)
	}
	if got := r.ListPublic(); len(got) != 0 {
		t.Fatalf("malicious site was stored; ListPublic returned %d", len(got))
	}

	// 空 ref 仍应回退到 main，而不是被当作非法值拒绝。
	site, err := r.Register("https://github.com/o/repo", "ok", "", "github", nil, false)
	if err != nil {
		t.Fatalf("Register with empty ref: %v", err)
	}
	if site.Ref != "main" {
		t.Errorf("empty ref defaulted to %q, want %q", site.Ref, "main")
	}
}

func TestHiddenNotInListPublic(t *testing.T) {
	r := New()
	if _, err := r.Register("https://github.com/o/pub", "pub", "main", "github", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("https://github.com/o/secret", "secret", "main", "github", nil, true); err != nil {
		t.Fatal(err)
	}
	pub := r.ListPublic()
	if len(pub) != 1 {
		t.Fatalf("ListPublic = %d sites, want 1", len(pub))
	}
	if pub[0].PathID != "pub" {
		t.Errorf("ListPublic pathid = %q, want pub", pub[0].PathID)
	}
	// 隐藏站点仍可直链 Get
	if _, err := r.Get("secret"); err != nil {
		t.Errorf("Get(secret): %v", err)
	}
}

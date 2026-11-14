package registry

import (
	"os"
	"path/filepath"
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

func TestEnablePersistenceRejectsUnwritableDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root 无视目录写权限，探针无法被不可写目录触发")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "readonly")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	// 先恢复权限，否则 TempDir 的清理删不掉只读目录里的东西。
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })

	for _, tc := range []struct{ name, path string }{
		{"unwritable dir", filepath.Join(ro, "gitweb.state.json")},
		{"missing dir", filepath.Join(dir, "nope", "gitweb.state.json")},
	} {
		r := New()
		err := r.EnablePersistence(tc.path)
		if err == nil {
			t.Fatalf("%s: expected error, got nil", tc.name)
		}
		// store 未挂上 = 后续 Register 不会去写一个永远写不进去的路径。
		if r.store != nil {
			t.Errorf("%s: store attached despite probe failure", tc.name)
		}
	}
}

func TestEnablePersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitweb.state.json")

	r := New()
	if err := r.EnablePersistence(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("https://github.com/o/repo", "keep", "dev", "github",
		&Auth{Type: "token", Token: "secret-token"}, true); err != nil {
		t.Fatal(err)
	}
	r.IncrementViews("keep")
	r.Close() // 停 flush goroutine 并把 dirty 的浏览数落盘

	r2 := New()
	if err := r2.EnablePersistence(path); err != nil {
		t.Fatal(err)
	}
	site, err := r2.Get("keep")
	if err != nil {
		t.Fatalf("site lost across reload: %v", err)
	}
	if site.Ref != "dev" {
		t.Errorf("Ref = %q, want dev", site.Ref)
	}
	if !site.Hidden {
		t.Error("Hidden = false, want true")
	}
	if site.Views != 1 {
		t.Errorf("Views = %d, want 1", site.Views)
	}
	// 不变量：state 文件不含凭据，reload 后 Auth 必为 nil。
	if site.Auth != nil {
		t.Errorf("Auth = %+v, want nil（凭据不持久化）", site.Auth)
	}
	if got := len(r2.ListPublic()); got != 0 {
		t.Errorf("ListPublic = %d, want 0（hidden 站点不进公开列表）", got)
	}
}

// overlay 是请求期凭据覆盖：只补「站点自身没有凭据」的缺口，不覆盖注册时自带的凭据。
func TestAuthOverlayAppliesOnlyWithoutSiteAuth(t *testing.T) {
	r := New()
	if _, err := r.Register("https://github.com/o/private", "plain", "main", "github", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("https://github.com/o/other", "own", "main", "github",
		&Auth{Type: "token", Token: "own-token"}, false); err != nil {
		t.Fatal(err)
	}

	r.SetAuthOverlay(map[string]*Auth{
		"plain": {Type: "token", Token: "overlay-token"},
		"own":   {Type: "token", Token: "overlay-token"},
	})

	site, err := r.Get("plain")
	if err != nil {
		t.Fatal(err)
	}
	if site.Auth == nil || site.Auth.Token != "overlay-token" {
		t.Errorf("plain site Auth = %+v, want overlay-token", site.Auth)
	}

	own, err := r.Get("own")
	if err != nil {
		t.Fatal(err)
	}
	if own.Auth == nil || own.Auth.Token != "own-token" {
		t.Errorf("own site Auth = %+v, want own-token（overlay 不得覆盖注册时自带的凭据）", own.Auth)
	}

	// overlay 不写回 map：清空后必须回落到注册时的样子。
	r.SetAuthOverlay(nil)
	site, err = r.Get("plain")
	if err != nil {
		t.Fatal(err)
	}
	if site.Auth != nil {
		t.Errorf("Auth = %+v, want nil（overlay 不得写回站点）", site.Auth)
	}
}

// overlay 只在请求期生效：即便在设置 overlay 之后再触发一次落盘，state 文件也不得出现凭据，
// 重新加载的注册表里 Auth 仍为 nil —— 重启后的凭据只能由调用方重新叠加。
func TestAuthOverlayNotPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gitweb.state.json")

	r := New()
	if err := r.EnablePersistence(path); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("https://github.com/o/private", "priv", "main", "github", nil, false); err != nil {
		t.Fatal(err)
	}
	r.SetAuthOverlay(map[string]*Auth{"priv": {Type: "token", Token: "secret-token"}})
	// 触发一次 Save（SetHidden 立即写盘），确认 overlay 之后的快照也不带凭据。
	if err := r.SetHidden("priv", true); err != nil {
		t.Fatal(err)
	}
	r.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-token") {
		t.Errorf("state file leaked overlay credentials:\n%s", data)
	}

	r2 := New()
	if err := r2.EnablePersistence(path); err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	site, err := r2.Get("priv")
	if err != nil {
		t.Fatalf("site lost across reload: %v", err)
	}
	if site.Auth != nil {
		t.Errorf("Auth = %+v, want nil（overlay 不落盘，重启后须重新叠加）", site.Auth)
	}
}

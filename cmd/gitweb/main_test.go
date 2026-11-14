package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/limingrui/gitweb/internal/config"
	"github.com/limingrui/gitweb/internal/registry"
)

// state 路径四层里，flag 与 env 的「显式空串」= 纯内存，必须与「没给」可区分，
// 否则关不掉持久化；config 层的 state_file 相反，空值只是未配置、继续向下继承。
func TestResolveStatePath(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  statePathSources
		want string
	}{
		{"nothing set", statePathSources{}, defaultStatePath},
		{"env wins over default", statePathSources{envValue: "/e.json", envSet: true}, "/e.json"},
		{"explicit empty env disables persistence", statePathSources{envSet: true}, ""},
		{"config wins over env", statePathSources{cfgValue: "/c.json", envValue: "/e.json", envSet: true}, "/c.json"},
		{"empty config line inherits env", statePathSources{cfgValue: "", envValue: "/e.json", envSet: true}, "/e.json"},
		{"flag wins over config and env", statePathSources{flagValue: "/f.json", flagSet: true, cfgValue: "/c.json", envValue: "/e.json", envSet: true}, "/f.json"},
		{"flag empty disables persistence despite config", statePathSources{flagSet: true, cfgValue: "/c.json"}, ""},
	} {
		if got := resolveStatePath(tc.src); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// overlay 的匹配键是 pathid：空 pathid 或没配 auth 的条目不得进入覆盖表，
// 否则「按 git_url 兜底」的歧义就会从这张表漏回来。
func TestBuildAuthOverlayKeysByPathID(t *testing.T) {
	specs := []config.SiteSpec{
		{GitURL: "https://git.example.com/o/a.git", PathID: "a", Auth: &config.AuthSpec{Type: "token", Token: "ta"}},
		{GitURL: "https://git.example.com/o/b.git", Auth: &config.AuthSpec{Type: "token", Token: "tb"}},
		{GitURL: "https://git.example.com/o/c.git", PathID: "c"},
	}
	overlay := buildAuthOverlay(specs)
	if len(overlay) != 1 {
		t.Fatalf("overlay size = %d, want 1: %+v", len(overlay), overlay)
	}
	if a := overlay["a"]; a == nil || a.Token != "ta" {
		t.Errorf("overlay[a] = %+v, want token ta", a)
	}
}

// 复现 issue 的主场景：首次启动播种带凭据的私有预置站点，state 落盘但不含凭据；
// 重启后预置被跳过（state 优先），凭据必须由 config 构建的 overlay 重新叠回来。
func TestAuthOverlaySurvivesRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "gitweb.state.json")
	specs := []config.SiteSpec{{
		GitURL: "https://git.example.com/o/private.git",
		PathID: "priv",
		Ref:    "main",
		Auth:   &config.AuthSpec{Type: "token", Token: "s3cret-token"},
	}}

	// 首次启动：state 不存在 → 播种，凭据只进内存。
	first := registry.New()
	if err := first.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	first.SetAuthOverlay(buildAuthOverlay(specs))
	if _, err := first.Register(specs[0].GitURL, specs[0].PathID, specs[0].Ref, "gitea",
		toRegistryAuth(specs[0].Auth), false); err != nil {
		t.Fatal(err)
	}
	first.Close()

	// 重启：state 已存在，启动流程不会再播种 —— 此时若没有 overlay，Get 拿到的 Auth 就是 nil，
	// 请求上游私有仓库直接 401。
	second := registry.New()
	if err := second.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	site, err := second.Get("priv")
	if err != nil {
		t.Fatalf("site lost across restart: %v", err)
	}
	if site.Auth != nil {
		t.Fatalf("state must not carry credentials, got %+v", site.Auth)
	}

	second.SetAuthOverlay(buildAuthOverlay(specs))
	site, err = second.Get("priv")
	if err != nil {
		t.Fatal(err)
	}
	if site.Auth == nil || site.Auth.Token != "s3cret-token" {
		t.Fatalf("auth lost after restart: overlay did not restore credentials, got %+v", site.Auth)
	}
	second.Close()

	// 再重启一次：overlay 依然不落盘，凭据不在 state 里。
	third := registry.New()
	if err := third.EnablePersistence(statePath); err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	site, err = third.Get("priv")
	if err != nil {
		t.Fatal(err)
	}
	if site.Auth != nil {
		t.Errorf("overlay leaked into state file: %+v", site.Auth)
	}
}

// 建议 3 的启动告警：配了凭据却没写 pathid 的预置站点，重启后无法被 overlay 匹配，必须 warn；
// 写了 pathid 且 overlay 命中的，不得误报。
func TestMissingAuthWarnings(t *testing.T) {
	r := registry.New()
	if _, err := r.Register("https://git.example.com/o/matched.git", "matched", "main", "gitea", nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("https://git.example.com/o/unmatched.git", "rand0mid", "main", "gitea", nil, false); err != nil {
		t.Fatal(err)
	}

	specs := []config.SiteSpec{
		{GitURL: "https://git.example.com/o/matched.git", PathID: "matched", Auth: &config.AuthSpec{Type: "token", Token: "t"}},
		{GitURL: "https://git.example.com/o/unmatched.git", Auth: &config.AuthSpec{Type: "token", Token: "t"}},
		{GitURL: "https://git.example.com/o/absent.git", Auth: &config.AuthSpec{Type: "token", Token: "t"}},
	}
	r.SetAuthOverlay(buildAuthOverlay(specs))

	warnings := missingAuthWarnings(r, specs)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %d, want 1: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "unmatched.git") {
		t.Errorf("warning should point at the preset without pathid, got %q", warnings[0])
	}
}

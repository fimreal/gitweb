package config

import (
	"os"
	"testing"
)

// state_file 是普通 string：空值只能表示「未配置」，不能表示「关掉持久化」。
// 本文件整份会过 os.ExpandEnv，所以未赋值的 ${VAR} 会变成空串 —— 若空串意味着关闭，
// 一个漏设的变量就会静默丢掉所有运行期注册的站点。
func TestStateFileEmptyMeansUnset(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"omitted", ""},
		{"explicit empty", `state_file: ""` + "\n"},
		{"unexpanded var", "state_file: ${GITWEB_UNSET_VAR_TOTTO}\n"},
	} {
		cfg, err := Load(writeConfig(t, tc.yaml))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cfg.StateFile != "" {
			t.Errorf("%s: StateFile = %q, want \"\" (交给下一层)", tc.name, cfg.StateFile)
		}
	}

	const want = "/var/lib/gitweb/state.json"
	cfg, err := Load(writeConfig(t, "state_file: "+want+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateFile != want {
		t.Errorf("StateFile = %q, want %s", cfg.StateFile, want)
	}
}

// github_base_url 空值 = 公网默认；非空必须是可拼 URL 的站点根，
// 带路径/查询串的值会让 raw/API 地址静默指向错误位置，必须在加载时拒绝。
func TestGitHubBaseURL(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fetch.GitHubBaseURL != "" {
		t.Errorf("GitHubBaseURL = %q, want empty default", cfg.Fetch.GitHubBaseURL)
	}

	cfg, err = Load(writeConfig(t, "fetch:\n  github_base_url: https://ghe.example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Fetch.GitHubBaseURL != "https://ghe.example.com" {
		t.Errorf("GitHubBaseURL = %q", cfg.Fetch.GitHubBaseURL)
	}

	for _, bad := range []string{
		"ghe.example.com",
		"ftp://ghe.example.com",
		"https://ghe.example.com/sub/path",
		"https://ghe.example.com/?x=1",
	} {
		if _, err := Load(writeConfig(t, "fetch:\n  github_base_url: "+bad+"\n")); err == nil {
			t.Errorf("github_base_url %q: expected validation error, got nil", bad)
		}
	}
}

// render.extra_text_exts 默认空（行为不变），显式配置时按 YAML 列表解析。
func TestRenderExtraTextExts(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Render.ExtraTextExts) != 0 {
		t.Errorf("default ExtraTextExts = %v, want empty", cfg.Render.ExtraTextExts)
	}

	cfg, err = Load(writeConfig(t, "render:\n  extra_text_exts: [\".foo\", \"bar\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(cfg.Render.ExtraTextExts), 2; got != want {
		t.Fatalf("ExtraTextExts = %v, want %d entries", cfg.Render.ExtraTextExts, want)
	}
	if cfg.Render.ExtraTextExts[0] != ".foo" || cfg.Render.ExtraTextExts[1] != "bar" {
		t.Errorf("ExtraTextExts = %v, want [.foo bar]", cfg.Render.ExtraTextExts)
	}
}

func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	body := "base_url: http://localhost:8080\nlisten: \":8080\"\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

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

func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	path := t.TempDir() + "/config.yaml"
	body := "base_url: http://localhost:8080\nlisten: \":8080\"\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

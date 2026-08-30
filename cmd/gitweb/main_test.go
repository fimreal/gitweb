package main

import "testing"

// state 路径的三层优先级里，"显式传空串关掉持久化" 与 "没传" 必须可区分，
// 否则关不掉、或该落盘的配置被静默覆盖。
func TestResolveStatePath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flagValue string
		flagSet   bool
		envValue  string
		envSet    bool
		want      string
	}{
		{"neither set falls back to default", "", false, "", false, defaultStatePath},
		{"env wins over default", "", false, "/data/state.json", true, "/data/state.json"},
		{"explicit empty env disables persistence", "", false, "", true, ""},
		{"flag wins over env", "/a/state.json", true, "/b/state.json", true, "/a/state.json"},
		{"flag empty disables persistence despite env", "", true, "/b/state.json", true, ""},
	} {
		got := resolveStatePath(tc.flagValue, tc.flagSet, tc.envValue, tc.envSet)
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

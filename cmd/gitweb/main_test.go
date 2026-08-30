package main

import "testing"

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
